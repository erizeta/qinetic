package qinetic

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/erizeta/qilat"
)

// Handler handles a request. A returned error goes to the App's ErrorHandler.
type Handler = func(c *Ctx) error

// maxPooledBody is the largest response buffer kept with a pooled Ctx; bigger ones are left to the GC.
const maxPooledBody = 64 << 10

// Ctx carries one request through its handlers. Like the qilat request it wraps, a Ctx and everything
// returned from it without copying are valid only until the handler chain returns; c.Context() may be kept.
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
	committed bool // status and buffered body sent; writes go to w
	dead      bool
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

// Context returns the request context. It is canceled when the client disconnects or the request
// finishes, and unlike the Ctx it may be kept after the handler returns.
func (c *Ctx) Context() context.Context {
	c.live()
	return c.r.Context()
}

// Protocol returns the request protocol: "HTTP/1.1", "HTTP/2", or "HTTP/3".
func (c *Ctx) Protocol() string {
	c.live()
	return c.r.Proto()
}

// Status sets the response status code. It has no effect once the response is committed.
func (c *Ctx) Status(code int) *Ctx {
	c.live()
	c.status = code
	return c
}

// Send replaces the response body with body.
func (c *Ctx) Send(body []byte) error {
	c.live()
	return c.setBody(body)
}

// SendString replaces the response body with s.
func (c *Ctx) SendString(s string) error {
	c.live()
	c.body = append(c.body[:0], s...)
	return c.bodyReplaced()
}

// JSON encodes v as the response body with Content-Type application/json.
func (c *Ctx) JSON(v any) error {
	c.live()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.w.Header().Set("Content-Type", "application/json")
	return c.setBody(b)
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

func (c *Ctx) writeCommitted() error {
	_, err := c.w.Write(c.body)
	c.body = c.body[:0]
	return err
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

// flush writes the buffered response. Content-Length is always set so bodies over qilat's 4 KiB
// buffer are not chunked; for HEAD, qilat keeps it and drops the body.
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
	var n [20]byte
	h.SetBytes("Content-Length", strconv.AppendInt(n[:0], int64(len(c.body)), 10))
	c.w.WriteHeader(status)
	if len(c.body) > 0 {
		// A failed write means the client is gone; qilat has already canceled the request context.
		_, _ = c.w.Write(c.body)
	}
}

func (c *Ctx) reset() {
	c.app, c.w, c.r = nil, nil, nil
	c.m.clear()
	c.route, c.handler = nil, 0
	c.status, c.committed = 0, false
	if cap(c.body) > maxPooledBody {
		c.body = nil
	} else {
		c.body = c.body[:0]
	}
}
