// Package static serves files from a directory or an fs.FS.
package static

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
)

// Config configures the middleware.
type Config struct {
	// Next skips the middleware when it returns true.
	Next func(c *qinetic.Ctx) bool
	// FS serves files from this file system instead of root, such as an embed.FS.
	FS fs.FS
	// Index is the file served for a directory. If empty, "index.html" is used.
	Index string
	// Browse lists a directory that has no index file. Off by default, so file names are not exposed.
	Browse bool
	// MaxAge sets Cache-Control: public, max-age=MaxAge (seconds) on files when positive.
	MaxAge int
}

// New returns a handler that serves files from root, or Config.FS, with Range and conditional requests.
// Files are opened through os.Root, so a symlink inside root cannot lead outside it; symlinks within root
// must be relative. New panics if root cannot be opened.
// Mount it under a prefix with Use, as in app.Use("/assets", static.New("./public")), or as a wildcard
// route, as in app.Get("/assets/*", static.New("./public")); the prefix is removed from the path. If root
// is a file, it is served for every request. GET and HEAD only; a missing file, or a directory with no
// index when Browse is off, passes the request on with c.Next. A path with a ".." element, as a decoded
// %2F..%2F would give, fails with qinetic.ErrBadRequest.
func New(root string, config ...Config) qinetic.Handler {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.Index == "" {
		cfg.Index = "index.html"
	}
	fsys, single := cfg.FS, ""
	if fsys == nil {
		dir := root
		if info, err := os.Stat(root); err == nil && !info.IsDir() {
			dir, single = filepath.Dir(root), filepath.Base(root)
		}
		// os.Root, unlike os.DirFS, refuses paths that leave dir, through symlinks included.
		r, err := os.OpenRoot(dir)
		if err != nil {
			panic(fmt.Sprintf("qinetic/static: %v", err))
		}
		fsys = r.FS()
	}
	cacheControl := ""
	if cfg.MaxAge > 0 {
		cacheControl = "public, max-age=" + strconv.Itoa(cfg.MaxAge)
	}

	return func(c *qinetic.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) || c.Method() != http.MethodGet && c.Method() != http.MethodHead {
			return c.Next()
		}
		rawPath := string(c.Request().URI().RawPath())
		name := single
		if name == "" {
			rest, err := relativePath(c, rawPath)
			if err != nil {
				return qinetic.ErrBadRequest
			}
			name = strings.Trim(rest, "/")
			if name == "" {
				name = "."
			}
			if !fs.ValidPath(name) {
				return qinetic.ErrBadRequest
			}
		}
		f, err := fsys.Open(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return c.Next()
		case errors.Is(err, fs.ErrInvalid):
			// Windows file systems refuse names with '\' or ':'.
			return qinetic.ErrBadRequest
		case err != nil:
			return err
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Relative links in an index page resolve against the directory only with the trailing slash.
			if !strings.HasSuffix(rawPath, "/") {
				target := rawPath + "/"
				if q := c.Request().URI().RawQuery(); len(q) > 0 {
					target += "?" + string(q)
				}
				return c.Redirect(target, http.StatusMovedPermanently)
			}
			index, err := fsys.Open(path.Join(name, cfg.Index))
			if err != nil {
				if cfg.Browse && errors.Is(err, fs.ErrNotExist) {
					return list(c, fsys, name)
				}
				return c.Next()
			}
			defer func() { _ = index.Close() }()
			if info, err = index.Stat(); err != nil {
				return err
			}
			f = index
		}
		content, ok := f.(io.ReadSeeker)
		if !ok {
			b, err := io.ReadAll(f)
			if err != nil {
				return err
			}
			content = bytes.NewReader(b)
		}
		if cacheControl != "" {
			c.Set("Cache-Control", cacheControl)
		}
		modtime := info.ModTime()
		return qinetic.FromQilat(qilat.HandlerFunc(func(w qilat.ResponseWriter, r *qilat.Request) {
			qilat.ServeContent(w, r, info.Name(), modtime, content)
		}))(c)
	}
}

// relativePath returns the decoded request path below the mount point: the "*" parameter of a wildcard
// route, or, under Use, what follows the route's prefix segments.
func relativePath(c *qinetic.Ctx, rawPath string) (string, error) {
	rt := c.Route()
	var rest string
	if len(rt.Params) > 0 && rt.Params[len(rt.Params)-1] == "*1" {
		rest = c.Params("*1")
	} else {
		segs := 0
		if p := strings.Trim(rt.Path, "/"); p != "" && rt.Method == "USE" {
			segs = strings.Count(p, "/") + 1
		}
		raw := strings.TrimPrefix(rawPath, "/")
		for range segs {
			_, raw, _ = strings.Cut(raw, "/")
		}
		var err error
		if rest, err = url.PathUnescape(raw); err != nil {
			return "", err
		}
	}
	for part := range strings.SplitSeq(rest, "/") {
		if part == ".." {
			return "", errors.New("dot-dot element")
		}
	}
	return rest, nil
}

// list writes a minimal HTML listing of dir, like net/http's FileServer.
func list(c *qinetic.Ctx, fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("<!doctype html>\n<pre>\n")
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		u := url.URL{Path: name}
		fmt.Fprintf(&b, "<a href=\"%s\">%s</a>\n", html.EscapeString(u.String()), html.EscapeString(name))
	}
	b.WriteString("</pre>\n")
	c.Type("html")
	return c.SendString(b.String())
}
