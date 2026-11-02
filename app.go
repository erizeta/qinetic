package qinetic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"

	"github.com/erizeta/qilat"
)

// App is a Qinetic application. It is a qilat.Handler; routes must be registered before it serves
// its first request, after which registering panics.
type App struct {
	config Config
	router *router
	pool   sync.Pool
	// group is the root Router that App's routing methods delegate to.
	group Group

	bodyLimit  int64
	proxies    []netip.Prefix
	jsonEncode func(v any) ([]byte, error)
	jsonDecode func(data []byte, v any) error

	srvMu      sync.Mutex
	srv        *qilat.Server
	active     int // Listen calls using srv
	closed     bool
	onShutdown []func(ctx context.Context) error
	hooksOnce  sync.Once
	stopped    chan struct{} // closed when Shutdown has finished
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
	app.bodyLimit = app.config.BodyLimit
	if app.bodyLimit == 0 {
		app.bodyLimit = DefaultBodyLimit
	} else if app.bodyLimit < 0 {
		app.bodyLimit = -1
	}
	for _, p := range app.config.TrustedProxies {
		prefix, err := parseProxy(p)
		if err != nil {
			panic(fmt.Sprintf("qinetic: invalid TrustedProxies entry %q: %v", p, err))
		}
		app.proxies = append(app.proxies, prefix)
	}
	app.jsonEncode, app.jsonDecode = app.config.JSONEncoder, app.config.JSONDecoder
	if app.jsonEncode == nil {
		app.jsonEncode = json.Marshal
	}
	if app.jsonDecode == nil {
		app.jsonDecode = json.Unmarshal
	}
	app.router = newRouter(app.config.CaseSensitive, app.config.StrictRouting)
	app.pool.New = func() any { return new(Ctx) }
	app.stopped = make(chan struct{})
	app.group = Group{app: app}
	return app
}

func parseProxy(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		// Client addresses are compared unmapped, so a ::ffff:a.b.c.d/n prefix must be too.
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return netip.Prefix{}, errors.New("IPv4-mapped prefix shorter than /96")
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// trusted reports whether ip is one of Config.TrustedProxies.
func (app *App) trusted(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	for _, p := range app.proxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
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
