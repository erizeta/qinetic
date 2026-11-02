package qinetic

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/erizeta/qilat"
)

// App is a qinetic application. It is a qilat.Handler; routes must be registered before it serves
// its first request, after which registering panics.
type App struct {
	config Config
	router *router
	pool   sync.Pool
	// group is the root Router that App's routing methods delegate to.
	group Group
}

// New returns an App configured by cfg, if given.
func New(cfg ...Config) *App {
	app := &App{}
	if len(cfg) > 0 {
		app.config = cfg[0]
	}
	if app.config.ErrorHandler == nil {
		app.config.ErrorHandler = DefaultErrorHandler
	}
	if app.config.Logger == nil {
		app.config.Logger = slog.Default()
	}
	app.router = newRouter(app.config.CaseSensitive, app.config.StrictRouting)
	app.pool.New = func() any { return new(Ctx) }
	app.group = Group{app: app}
	return app
}

// Handler returns app as a qilat.Handler, for a custom qilat.Server or qilat middleware.
func (app *App) Handler() qilat.Handler { return app }

// ServeHTTP runs the handler chain for r. A panic propagates to qilat and its Ctx is not reused.
func (app *App) ServeHTTP(w qilat.ResponseWriter, r *qilat.Request) {
	c := app.acquireCtx(w, r)
	if err := c.Next(); err != nil {
		app.handleError(c, err)
	}
	c.flush()
	app.releaseCtx(c)
}

func (app *App) acquireCtx(w qilat.ResponseWriter, r *qilat.Request) *Ctx {
	c := app.pool.Get().(*Ctx)
	c.app, c.w, c.r = app, w, r
	c.m.reset(app.router, r.Method(), r.URI().RawPath())
	return c
}

func (app *App) releaseCtx(c *Ctx) {
	c.reset()
	if app.config.DebugPoison {
		c.dead = true
		return
	}
	app.pool.Put(c)
}

func (app *App) handleError(c *Ctx, err error) {
	if c.committed {
		app.logError(c, "error after the response was committed", err)
		return
	}
	c.body = c.body[:0]
	herr := app.config.ErrorHandler(c, err)
	if herr == nil {
		return
	}
	app.logError(c, "error handler failed", herr)
	if c.committed {
		return
	}
	c.body = c.body[:0]
	c.status = http.StatusInternalServerError
	c.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	c.body = append(c.body, http.StatusText(http.StatusInternalServerError)...)
}

// Test serves req in memory and returns the response, without a network or a qilat.Server. Server
// settings such as its body size limit and DebugPoison therefore do not apply; Config.DebugPoison does.
// A handler panic propagates to the caller.
func (app *App) Test(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("qinetic: Test called with a nil request")
	}
	rec := httptest.NewRecorder()
	qilat.ToStd(app).ServeHTTP(rec, req)
	return rec.Result(), nil
}
