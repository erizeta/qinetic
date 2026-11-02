package qinetic

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/erizeta/qilat"
)

// ErrResponseSent is returned when writing a body after a qilat handler or middleware, run through
// FromQilat or FromQilatMiddleware, has finished the response: the bytes would land after a body framed
// or encoded by that handler, corrupting it, so they are not sent.
var ErrResponseSent = errors.New("qinetic: response already sent by a qilat handler")

// FromQilat runs a qilat handler, such as qilat.FileServer, as the response. The handler writes to the
// client directly, so the response is committed and an ErrorHandler cannot replace it; its status and size
// are still recorded for c.StatusCode and c.ResponseSize.
func FromQilat(h qilat.Handler) Handler {
	return func(c *Ctx) error {
		c.live()
		if c.sealed {
			return ErrResponseSent
		}
		if c.committed {
			return errors.New("qinetic: FromQilat after the response was committed")
		}
		c.committed = true
		c.body = c.body[:0]
		h.ServeHTTP(&recordingWriter{ResponseWriter: c.w, c: c, count: true}, c.r)
		c.sealed = true
		return nil
	}
}

// FromStd runs a net/http handler as the response, through qilat.FromStd. See FromQilat.
func FromStd(h http.Handler) Handler {
	return FromQilat(qilat.FromStd(h))
}

// FromQilatMiddleware runs qilat middleware, such as qilat.CompressWith, around the rest of the chain.
// Inside it the Ctx writes to the middleware's writer and reads the request the middleware passes on, which
// may differ, as with qilat.StripPrefix. Since qilat finishes that writer when its handler returns, the rest
// of the chain, any ErrorHandler, and the buffered response all complete inside the middleware: the
// response is then committed, and handlers registered before the adapter see it as sent. When the middleware
// answers without calling next, the rest of the chain does not run.
func FromQilatMiddleware(mw func(qilat.Handler) qilat.Handler) Handler {
	return func(c *Ctx) error {
		c.live()
		called := false
		inner := qilat.HandlerFunc(func(w qilat.ResponseWriter, r *qilat.Request) {
			called = true
			origW, origR := c.w, c.r
			// Restored even on panic, so a recover middleware outside writes to the real client.
			defer func() { c.w, c.r = origW, origR }()
			c.w, c.r = w, r
			if err := c.Next(); err != nil {
				c.app.handleError(c, err)
			}
			c.flush()
			c.committed = true
			c.body = c.body[:0] // sent; ResponseSize counts it once, in c.sent
		})
		rec := &recordingWriter{ResponseWriter: c.w, c: c}
		mw(inner).ServeHTTP(rec, c.r)
		if !called {
			// The middleware wrote the response itself; count it, since flush did not.
			c.sent += rec.n
		}
		c.committed, c.sealed = true, true
		return nil
	}
}

// FromStdMiddleware runs net/http middleware around the rest of the chain, through
// qilat.FromStdMiddleware. See FromQilatMiddleware.
func FromStdMiddleware(mw func(http.Handler) http.Handler) Handler {
	return FromQilatMiddleware(qilat.FromStdMiddleware(mw))
}

// recordingWriter notes the status, and with count the body size, of a response written past the Ctx.
// It keeps the optional interfaces of qilat writers, so sendfile, trailers, and hijacking still work.
type recordingWriter struct {
	qilat.ResponseWriter
	c       *Ctx
	count   bool
	n       int64
	decided bool // a final status was written; qilat ignores later ones
}

func (w *recordingWriter) WriteHeader(code int) {
	if code >= 200 && !w.decided {
		w.decided = true
		w.c.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.record(int64(n))
	return n, err
}

func (w *recordingWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	w.record(int64(n))
	return n, err
}

func (w *recordingWriter) record(n int64) {
	w.n += n
	if w.count {
		w.c.sent += n
	}
}

func (w *recordingWriter) ReadFrom(src io.Reader) (int64, error) {
	var n int64
	var err error
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		n, err = io.Copy(struct{ io.Writer }{w.ResponseWriter}, src)
	}
	w.record(n)
	return n, err
}

func (w *recordingWriter) Trailer() *qilat.Header {
	if ts, ok := w.ResponseWriter.(qilat.TrailerSetter); ok {
		return ts.Trailer()
	}
	return new(qilat.Header)
}

func (w *recordingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(qilat.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, qilat.ErrNotSupported
}
