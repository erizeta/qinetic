package qinetic

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/erizeta/qilat"
)

// Status sets the response status code. It has no effect once the response is committed.
func (c *Ctx) Status(code int) *Ctx {
	c.live()
	c.status = code
	return c
}

// Set sets the response header key to val. qilat replaces CR, LF, and NUL in val, so a value taken from
// the request cannot inject header fields, and panics on an invalid key.
func (c *Ctx) Set(key, val string) {
	c.live()
	c.w.Header().Set(key, val)
}

// GetRespHeader returns a copy of the response header key, or def[0] (or "") when it is absent or empty.
func (c *Ctx) GetRespHeader(key string, def ...string) string {
	c.live()
	if v := c.w.Header().Peek(key); len(v) > 0 {
		return string(v)
	}
	return first(def)
}

// Append adds values to the comma-separated response header field, skipping values already listed.
func (c *Ctx) Append(field string, values ...string) {
	c.live()
	h := c.w.Header()
	cur := string(h.Peek(field))
	orig := cur
	for _, v := range values {
		if v == "" || listContains(cur, v) {
			continue
		}
		if cur == "" {
			cur = v
		} else {
			cur += ", " + v
		}
	}
	if cur != orig {
		h.Set(field, cur)
	}
}

func listContains(list, v string) bool {
	for item := range strings.SplitSeq(list, ",") {
		if strings.TrimSpace(item) == v {
			return true
		}
	}
	return false
}

// Vary adds fields to the Vary response header.
func (c *Ctx) Vary(fields ...string) {
	c.Append("Vary", fields...)
}

// Type sets Content-Type from a file extension such as "json" or ".html", or from a full media type.
// A charset, if given, replaces the type's own. Unknown extensions give application/octet-stream.
func (c *Ctx) Type(ext string, charset ...string) *Ctx {
	c.live()
	ct := ext
	if !strings.Contains(ext, "/") {
		ct = mime.TypeByExtension("." + strings.TrimPrefix(ext, "."))
		if ct == "" {
			ct = "application/octet-stream"
		}
	}
	if len(charset) > 0 {
		base, _, _ := strings.Cut(ct, ";")
		ct = strings.TrimSpace(base) + "; charset=" + charset[0]
	}
	c.w.Header().Set("Content-Type", ct)
	return c
}

// Send replaces the response body with body. It sets no Content-Type.
func (c *Ctx) Send(body []byte) error {
	c.live()
	return c.setBody(body)
}

// SendString replaces the response body with s, with Content-Type text/plain; charset=utf-8 unless one is set.
func (c *Ctx) SendString(s string) error {
	c.live()
	c.defaultType("text/plain; charset=utf-8")
	c.body = append(c.body[:0], s...)
	return c.bodyReplaced()
}

// SendStatus sets the status code and, if the body is empty, sends the status text.
func (c *Ctx) SendStatus(code int) error {
	c.live()
	c.status = code
	if len(c.body) == 0 && bodyAllowed(code) {
		return c.SendString(http.StatusText(code))
	}
	return nil
}

// JSON encodes v with Config.JSONEncoder (encoding/json by default) as the body, with Content-Type
// application/json.
func (c *Ctx) JSON(v any) error {
	c.live()
	b, err := c.app.jsonEncode(v)
	if err != nil {
		return err
	}
	c.w.Header().Set("Content-Type", "application/json")
	return c.setBody(b)
}

// XML encodes v with encoding/xml as the body, with Content-Type application/xml.
func (c *Ctx) XML(v any) error {
	c.live()
	b, err := xml.Marshal(v)
	if err != nil {
		return err
	}
	c.w.Header().Set("Content-Type", "application/xml")
	return c.setBody(b)
}

func (c *Ctx) defaultType(ct string) {
	if h := c.w.Header(); !h.Has("Content-Type") {
		h.Set("Content-Type", ct)
	}
}

func (c *Ctx) setBody(b []byte) error {
	c.body = append(c.body[:0], b...)
	return c.bodyReplaced()
}

func (c *Ctx) bodyReplaced() error {
	if c.committed {
		return c.writeCommitted()
	}
	return nil
}

// Redirect responds with a redirect to location, 303 See Other unless code is given. A location with
// control characters, such as CR or LF from an unchecked request value, is refused with an error.
func (c *Ctx) Redirect(location string, code ...int) error {
	c.live()
	status := http.StatusSeeOther
	if len(code) > 0 {
		status = code[0]
	}
	if status < 300 || status > 399 {
		return fmt.Errorf("qinetic: Redirect with non-3xx status %d", status)
	}
	for i := 0; i < len(location); i++ {
		if b := location[i]; b < 0x20 || b == 0x7f {
			return errors.New("qinetic: Redirect location contains a control character")
		}
	}
	c.w.Header().Set("Location", location)
	c.status = status
	c.body = c.body[:0]
	return nil
}

// Attachment sets Content-Disposition to attachment and, given a file name, sets Content-Type from its
// extension and offers its base name to the client.
func (c *Ctx) Attachment(filename ...string) {
	c.live()
	h := c.w.Header()
	if len(filename) == 0 {
		h.Set("Content-Disposition", "attachment")
		return
	}
	name := filepath.Base(filename[0])
	c.Type(filepath.Ext(name))
	disp := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if disp == "" {
		disp = "attachment"
	}
	h.Set("Content-Disposition", disp)
}

// Download sends file as an attachment named filename, or file's base name.
func (c *Ctx) Download(file string, filename ...string) error {
	c.live()
	name := file
	if len(filename) > 0 {
		name = filename[0]
	}
	c.Attachment(name)
	return c.SendFile(file)
}

// SendFile sends the named file with qilat.ServeFile, which handles Range and conditional requests and
// sets Content-Type. A name with a ".." element, as a decoded parameter like "a%2F..%2Fb" can produce,
// fails with ErrBadRequest; a missing file or directory with ErrNotFound. On success the response is
// committed and the file server picks the status, ignoring c.Status.
func (c *Ctx) SendFile(file string) error {
	c.live()
	if hasDotDot(file) {
		return ErrBadRequest
	}
	info, err := os.Stat(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ErrNotFound
	case errors.Is(err, fs.ErrPermission):
		return ErrForbidden
	case err != nil:
		return err
	case info.IsDir():
		return ErrNotFound
	}
	c.committed = true
	c.body = c.body[:0]
	qilat.ServeFile(c.w, c.r, file)
	return nil
}

func hasDotDot(name string) bool {
	for part := range strings.FieldsFuncSeq(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

// SendStream commits the response and copies r to it, closing r if it is an io.Closer. With size, it
// is sent as the Content-Length. The body set before is discarded.
func (c *Ctx) SendStream(r io.Reader, size ...int) error {
	c.live()
	if rc, ok := r.(io.Closer); ok {
		defer func() { _ = rc.Close() }()
	}
	if c.sealed {
		return ErrResponseSent
	}
	if c.committed {
		return errors.New("qinetic: SendStream after the response was committed")
	}
	if len(size) > 0 && size[0] >= 0 {
		c.w.Header().Set("Content-Length", strconv.Itoa(size[0]))
	}
	c.body = c.body[:0]
	if err := c.commit(); err != nil {
		return err
	}
	_, err := io.Copy(c.w, r)
	return err
}

// Writer commits the response and returns a writer for the rest of the body, sent as it is written.
// The response can no longer be replaced by an ErrorHandler.
func (c *Ctx) Writer() io.Writer {
	c.live()
	if c.sealed {
		return errWriter{ErrResponseSent}
	}
	_ = c.commit()
	return c.w
}

// errWriter fails every write, for a Writer asked for after the response was sealed.
type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }

// Flush commits the response and sends what has been written so far to the client.
func (c *Ctx) Flush() error {
	c.live()
	if c.sealed {
		return ErrResponseSent
	}
	if err := c.commit(); err != nil {
		return err
	}
	return c.w.Flush()
}

// Trailer returns the header whose fields are sent as trailers after the body. Using it drops the
// Content-Length the response would otherwise get, since HTTP/1.1 needs a chunked body for trailers.
func (c *Ctx) Trailer() *qilat.Header {
	c.live()
	c.noLength = true
	if ts, ok := c.w.(qilat.TrailerSetter); ok {
		return ts.Trailer()
	}
	return new(qilat.Header)
}
