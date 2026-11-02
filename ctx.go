package qinetic

import (
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/erizeta/qilat"
)

// Handler handles a request. A returned error goes to the App's ErrorHandler.
type Handler = func(c *Ctx) error

// maxPooledBody is the largest buffer kept with a pooled Ctx; bigger ones are left to the GC.
const maxPooledBody = 64 << 10

// Ctx carries one request through its handlers. Like the qilat request it wraps, a Ctx and every []byte
// returned from it are valid only until the handler chain returns; strings are copies and c.Context()
// may be kept.
//
// The response body is buffered and written once the chain and any ErrorHandler are done, so an
// ErrorHandler can still replace it. Headers go straight to the qilat ResponseWriter.
type Ctx struct {
	app *App
	w   qilat.ResponseWriter
	r   *qilat.Request

	m       matcher
	route   *route
	handler int // index of the running handler in route.handlers

	status    int
	body      []byte
	committed bool  // status and buffered body sent; writes go to w
	sealed    bool  // a qilat handler or middleware finished the response; nothing more may be written
	noLength  bool  // trailers are set, so flush must not declare a Content-Length
	sent      int64 // body bytes written to the client so far
	dead      bool

	// scratch holds decoded parameter values; it only grows during a request, so returned slices stay valid.
	scratch  []byte
	reqBody  []byte
	bodyRead bool
	bodyErr  error
	form     qilat.Args
	formRead bool
	mform    *multipart.Form
	formErr  error
	locals   map[any]any
}

func (c *Ctx) live() {
	if c.dead {
		panic("qinetic: Ctx used after its handler returned")
	}
}

// Next runs the next handler of the current route, or else the first handler of the next route that
// matches. When no route is left it returns ErrNotFound, or ErrMethodNotAllowed with an Allow header
// when the path matches routes of other methods.
func (c *Ctx) Next() error {
	c.live()
	if c.route != nil && c.handler+1 < len(c.route.handlers) {
		c.handler++
		return c.route.handlers[c.handler](c)
	}
	rt := c.m.next()
	if rt == nil {
		c.route = nil
		return c.noRoute()
	}
	c.route, c.handler = rt, 0
	return rt.handlers[0](c)
}

func (c *Ctx) noRoute() error {
	methods := c.app.router.allowed(c.r.URI().RawPath())
	if len(methods) == 0 || slices.Contains(methods, c.r.Method()) {
		return ErrNotFound
	}
	c.w.Header().Set("Allow", strings.Join(methods, ", "))
	return ErrMethodNotAllowed
}

func (c *Ctx) statusCode() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}

func bodyAllowed(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
}

func (c *Ctx) writeCommitted() error {
	if c.sealed {
		// The bytes would follow a body framed or encoded by someone else, such as a gzip stream.
		c.body = c.body[:0]
		return ErrResponseSent
	}
	n, err := c.w.Write(c.body)
	c.sent += int64(n)
	c.body = c.body[:0]
	return err
}

// commit sends the status and buffered body without a Content-Length, for responses that stream.
// Later writes go straight to the ResponseWriter and an ErrorHandler can no longer replace the response.
func (c *Ctx) commit() error {
	if c.committed {
		return nil
	}
	c.committed = true
	status := c.statusCode()
	c.w.WriteHeader(status)
	if !bodyAllowed(status) || len(c.body) == 0 {
		c.body = c.body[:0]
		return nil
	}
	return c.writeCommitted()
}

// flush writes the buffered response. Content-Length is set so bodies over qilat's 4 KiB buffer are not
// chunked, unless trailers follow; for HEAD, qilat keeps it and drops the body.
func (c *Ctx) flush() {
	if c.committed {
		return
	}
	status := c.statusCode()
	h := c.w.Header()
	if !bodyAllowed(status) {
		if status != http.StatusNotModified {
			h.Del("Content-Length")
		}
		c.w.WriteHeader(status)
		return
	}
	if c.noLength {
		h.Del("Content-Length")
	} else {
		var n [20]byte
		h.SetBytes("Content-Length", strconv.AppendInt(n[:0], int64(len(c.body)), 10))
	}
	c.w.WriteHeader(status)
	if len(c.body) > 0 {
		// A failed write means the client is gone; qilat has already canceled the request context.
		n, _ := c.w.Write(c.body)
		c.sent += int64(n)
	}
}

// StatusCode returns the response status: the one set so far, or 200.
func (c *Ctx) StatusCode() int {
	c.live()
	return c.statusCode()
}

// ResponseSize returns the size of the response body: what is buffered, plus what has already been sent
// once the response is committed. Bytes compressed by middleware count before compression.
func (c *Ctx) ResponseSize() int64 {
	c.live()
	return c.sent + int64(len(c.body))
}

// HandleError runs the App's error handling for err now, as it would after the chain: the body is
// discarded and Config.ErrorHandler writes the response. Middleware such as a logger use it to see the
// final status; the handler should then return nil.
func (c *Ctx) HandleError(err error) {
	c.live()
	if err != nil {
		c.app.handleError(c, err)
	}
}

func (c *Ctx) reset() {
	c.app, c.w, c.r = nil, nil, nil
	c.m.clear()
	c.route, c.handler = nil, 0
	c.status, c.committed, c.noLength, c.sent, c.sealed = 0, false, false, 0, false
	c.body = trimPooled(c.body)
	c.scratch = trimPooled(c.scratch)
	c.reqBody = trimPooled(c.reqBody)
	c.bodyRead, c.bodyErr = false, nil
	c.form.Reset()
	c.formRead, c.formErr = false, nil
	if c.mform != nil {
		// Parts are read from the in-memory body, so there are no temporary files; this is a safeguard.
		_ = c.mform.RemoveAll()
		c.mform = nil
	}
	c.locals = nil
}

func trimPooled(b []byte) []byte {
	if cap(b) > maxPooledBody {
		return nil
	}
	return b[:0]
}
