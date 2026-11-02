package static

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/erizeta/qinetic"
)

type result struct {
	code   int
	header http.Header
	body   string
}

func get(t *testing.T, app *qinetic.App, method, target string, hdr ...string) result {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header, string(b)}
}

// tree builds public/{app.css, docs/index.html, raw/x.txt} next to a secret file outside it.
func tree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	public := filepath.Join(dir, "public")
	for name, body := range map[string]string{
		"public/app.css":         "body{}",
		"public/docs/index.html": "<h1>docs</h1>",
		"public/raw/x.txt":       "x",
		"secret.txt":             "top secret",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return public
}

func useApp(t *testing.T, cfg ...Config) *qinetic.App {
	app := qinetic.New()
	app.Use("/assets", New(tree(t), cfg...))
	app.Get("/assets/fallback.txt", func(c *qinetic.Ctx) error { return c.SendString("from route") })
	return app
}

func TestServeFile(t *testing.T) {
	app := useApp(t, Config{MaxAge: 60})
	r := get(t, app, "GET", "/assets/app.css")
	if r.code != 200 || r.body != "body{}" || r.header.Get("Content-Type") != "text/css; charset=utf-8" ||
		r.header.Get("Cache-Control") != "public, max-age=60" || r.header.Get("Last-Modified") == "" {
		t.Fatalf("got %+v", r)
	}
	if r = get(t, app, "HEAD", "/assets/app.css"); r.code != 200 || r.body != "" || r.header.Get("Content-Length") != "6" {
		t.Fatalf("HEAD: %+v", r)
	}
	if r = get(t, app, "GET", "/assets/app.css", "Range", "bytes=0-3"); r.code != 206 || r.body != "body" {
		t.Fatalf("Range: %+v", r)
	}
	lm := get(t, app, "GET", "/assets/app.css").header.Get("Last-Modified")
	if r = get(t, app, "GET", "/assets/app.css", "If-Modified-Since", lm); r.code != 304 || r.body != "" {
		t.Fatalf("If-Modified-Since: %+v", r)
	}
	if r = get(t, app, "GET", "/ASSETS/app.css"); r.code != 200 {
		t.Fatalf("case-insensitive prefix: %+v", r)
	}
}

func TestDirectories(t *testing.T) {
	app := useApp(t)
	if r := get(t, app, "GET", "/assets/docs/"); r.code != 200 || r.body != "<h1>docs</h1>" {
		t.Fatalf("index: %+v", r)
	}
	if r := get(t, app, "GET", "/assets/docs?v=1"); r.code != 301 || r.header.Get("Location") != "/assets/docs/?v=1" {
		t.Fatalf("redirect: %+v", r)
	}
	// No index and Browse off: the file names stay private.
	if r := get(t, app, "GET", "/assets/raw/"); r.code != 404 || strings.Contains(r.body, "x.txt") {
		t.Fatalf("listing without Browse: %+v", r)
	}
	browse := useApp(t, Config{Browse: true})
	if r := get(t, browse, "GET", "/assets/raw/"); r.code != 200 || !strings.Contains(r.body, `<a href="x.txt">x.txt</a>`) {
		t.Fatalf("Browse: %+v", r)
	}
}

func TestPassesOn(t *testing.T) {
	app := useApp(t)
	if r := get(t, app, "GET", "/assets/fallback.txt"); r.code != 200 || r.body != "from route" {
		t.Fatalf("missing file: %+v", r)
	}
	if r := get(t, app, "GET", "/assets/nope.txt"); r.code != 404 {
		t.Fatalf("missing everywhere: %+v", r)
	}
	if r := get(t, app, "POST", "/assets/app.css"); r.code != 404 {
		t.Fatalf("POST: %+v", r)
	}
}

func TestTraversal(t *testing.T) {
	app := useApp(t)
	for target, want := range map[string]int{
		"/assets/../secret.txt":               404, // the router never routes a dot segment
		"/assets/%2e%2e/secret.txt":           404,
		"/assets/%2E%2e%2Fsecret.txt":         400, // one segment for the router, "../secret.txt" once decoded
		"/assets/docs%2F..%2F..%2Fsecret.txt": 400,
		"/assets/..%5Csecret.txt":             404, // a backslash is a file name character in an fs.FS
	} {
		r := get(t, app, "GET", target)
		if r.code != want || strings.Contains(r.body, "top secret") {
			t.Errorf("%s: got %d %q, want %d", target, r.code, r.body, want)
		}
	}
}

func TestWildcardRouteAndGroup(t *testing.T) {
	app := qinetic.New()
	public := tree(t)
	app.Get("/files/*", New(public))
	app.Group("/v1").Use("/static", New(public))
	if r := get(t, app, "GET", "/files/raw/x.txt"); r.code != 200 || r.body != "x" {
		t.Fatalf("wildcard route: %+v", r)
	}
	if r := get(t, app, "GET", "/v1/static/app.css"); r.code != 200 || r.body != "body{}" {
		t.Fatalf("group prefix: %+v", r)
	}
	if r := get(t, app, "GET", "/files/raw%2Fx.txt"); r.code != 200 || r.body != "x" {
		t.Fatalf("encoded slash: %+v", r)
	}
}

func TestFSAndSingleFile(t *testing.T) {
	fsys := fstest.MapFS{"hello.txt": {Data: []byte("from fs"), ModTime: time.Unix(1, 0)}}
	app := qinetic.New()
	app.Use("/fs", New("", Config{FS: fsys}))
	public := tree(t)
	app.Get("/style.css", New(filepath.Join(public, "app.css")))
	app.Use("/skip", New(public, Config{Next: func(c *qinetic.Ctx) bool { return true }}))
	if r := get(t, app, "GET", "/fs/hello.txt"); r.code != 200 || r.body != "from fs" {
		t.Fatalf("fs.FS: %+v", r)
	}
	if r := get(t, app, "GET", "/style.css"); r.code != 200 || r.body != "body{}" {
		t.Fatalf("single file: %+v", r)
	}
	if r := get(t, app, "GET", "/skip/app.css"); r.code != 404 {
		t.Fatalf("Next: %+v", r)
	}
}

// windowsLikeFS refuses names with '\' or ':' as os.DirFS does on Windows, so that path is tested everywhere.
type windowsLikeFS struct{ fstest.MapFS }

func (f windowsLikeFS) Open(name string) (fs.File, error) {
	if strings.ContainsAny(name, `\:`) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return f.MapFS.Open(name)
}

func TestInvalidNameIsBadRequest(t *testing.T) {
	app := qinetic.New()
	app.Use("/fs", New("", Config{FS: windowsLikeFS{fstest.MapFS{"ok.txt": {Data: []byte("ok")}}}}))
	for target, want := range map[string]int{"/fs/ok.txt": 200, "/fs/a%5Cb.txt": 400, "/fs/c:x.txt": 400} {
		if r := get(t, app, "GET", target); r.code != want {
			t.Errorf("%s: got %d, want %d", target, r.code, want)
		}
	}
}

// noSeekFS hands out files without io.Seeker, as some fs.FS implementations do.
type noSeekFS struct{ fstest.MapFS }

type noSeekFile struct{ f fs.File }

func (n noSeekFile) Stat() (fs.FileInfo, error) { return n.f.Stat() }
func (n noSeekFile) Read(p []byte) (int, error) { return n.f.Read(p) }
func (n noSeekFile) Close() error               { return n.f.Close() }

func (f noSeekFS) Open(name string) (fs.File, error) {
	file, err := f.MapFS.Open(name)
	if err != nil {
		return nil, err
	}
	return noSeekFile{file}, nil
}

func TestMoreFileCases(t *testing.T) {
	mapFS := fstest.MapFS{
		"index.html":    {Data: []byte("root index")},
		"blob.txt":      {Data: []byte("0123456789")},
		"sub/inner.txt": {Data: []byte("inner")},
		"list/child/a":  {Data: []byte("a")},
		"list/file.txt": {Data: []byte("f")},
	}
	app := qinetic.New()
	app.Use("/m", New("", Config{FS: mapFS, Browse: true}))
	app.Use("/ns", New("", Config{FS: noSeekFS{mapFS}}))
	if r := get(t, app, "GET", "/m/"); r.code != 200 || r.body != "root index" {
		t.Fatalf("mount root index: %+v", r)
	}
	if r := get(t, app, "GET", "/m/sub%2F%2Finner.txt"); r.code != 400 {
		t.Fatalf("empty element after decoding: %+v", r)
	}
	if r := get(t, app, "GET", "/m/list/"); r.code != 200 || !strings.Contains(r.body, `<a href="child/">child/</a>`) ||
		!strings.Contains(r.body, `<a href="file.txt">file.txt</a>`) {
		t.Fatalf("listing: %+v", r)
	}
	if r := get(t, app, "GET", "/ns/blob.txt", "Range", "bytes=2-4"); r.code != 206 || r.body != "234" {
		t.Fatalf("file without Seek: %+v", r)
	}
}
