// Package csrf protects state-changing requests from cross-site request forgery with two checks: the
// request's origin, from Sec-Fetch-Site or Origin, and a token stored server side that the client must
// send back in both a cookie and a header or form field.
package csrf

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/internal/memory"
)

// Errors returned for refused requests; each is a 403 *qinetic.Error.
var (
	ErrCrossOrigin  = qinetic.NewError(http.StatusForbidden, "cross-origin request refused")
	ErrMissingToken = qinetic.NewError(http.StatusForbidden, "missing CSRF token")
	ErrInvalidToken = qinetic.NewError(http.StatusForbidden, "invalid CSRF token")
)

// Config configures the middleware.
type Config struct {
	// Next skips the middleware when it returns true.
	Next func(c *qinetic.Ctx) bool
	// CookieName names the token cookie. If empty, "csrf_".
	CookieName string
	// CookieDomain and CookiePath scope the cookie. CookiePath defaults to "/".
	CookieDomain string
	CookiePath   string
	// CookieSameSite is qinetic.CookieSameSiteLax (the default), Strict, or None.
	CookieSameSite string
	// CookieScriptAccess lets JavaScript read the cookie, leaving out HttpOnly, for pages that copy it into
	// the X-Csrf-Token header. By default the cookie is HttpOnly and the token reaches the page through
	// TokenFromContext, so a script injected into the page cannot read it from the cookie.
	CookieScriptAccess bool
	// Expiration is how long a token stays valid. It is extended on safe requests once half of it has
	// passed. If zero, one hour.
	Expiration time.Duration
	// Extractor returns the token the request sent. If nil, the X-Csrf-Token header, then the _csrf form
	// field. Avoid FromQuery: URLs end up in logs and Referer headers.
	Extractor func(c *qinetic.Ctx) string
	// TrustedOrigins are other origins, such as "https://admin.example.com", allowed to send requests.
	// New panics on a malformed entry.
	TrustedOrigins []string
	// Storage holds the issued tokens. If nil, an in-memory storage per middleware is used; share an
	// external one between instances so a token issued by one is accepted by the others.
	Storage qinetic.Storage
}

// now is the clock, replaced in tests.
var now = time.Now

type (
	localsKey     struct{}
	middlewareKey struct{}
)

type middleware struct {
	cfg     Config
	trusted map[string]bool
}

// New returns the middleware. Safe methods (GET, HEAD, OPTIONS, TRACE) get a token, issued or refreshed.
// Other methods are refused with ErrCrossOrigin when they come from another origin, and with
// ErrMissingToken or ErrInvalidToken unless the token from Extractor matches the cookie and is still
// stored. Both checks apply to every unsafe request, over HTTP as well as HTTPS.
func New(config ...Config) qinetic.Handler {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.CookieName == "" {
		cfg.CookieName = "csrf_"
	}
	if cfg.CookiePath == "" {
		cfg.CookiePath = "/"
	}
	if cfg.CookieSameSite == "" {
		cfg.CookieSameSite = qinetic.CookieSameSiteLax
	}
	if cfg.Expiration <= 0 {
		cfg.Expiration = time.Hour
	}
	if cfg.Extractor == nil {
		cfg.Extractor = Chain(FromHeader("X-Csrf-Token"), FromForm("_csrf"))
	}
	if cfg.Storage == nil {
		cfg.Storage = memory.New(memory.Config{Clock: func() time.Time { return now() }})
	}
	m := &middleware{cfg: cfg, trusted: map[string]bool{}}
	for _, o := range cfg.TrustedOrigins {
		norm, err := normalizeOrigin(o)
		if err != nil {
			panic(fmt.Sprintf("qinetic/csrf: invalid trusted origin %q: %v", o, err))
		}
		m.trusted[norm] = true
	}
	return m.handle
}

// Extractors for Config.Extractor.

// FromHeader reads the token from a request header.
func FromHeader(name string) func(c *qinetic.Ctx) string {
	return func(c *qinetic.Ctx) string { return c.Get(name) }
}

// FromForm reads the token from an urlencoded or multipart form field.
func FromForm(name string) func(c *qinetic.Ctx) string {
	return func(c *qinetic.Ctx) string { return c.FormValue(name) }
}

// FromQuery reads the token from a query argument.
func FromQuery(name string) func(c *qinetic.Ctx) string {
	return func(c *qinetic.Ctx) string { return c.Query(name) }
}

// Chain returns the first non-empty token of extractors.
func Chain(extractors ...func(c *qinetic.Ctx) string) func(c *qinetic.Ctx) string {
	return func(c *qinetic.Ctx) string {
		for _, ex := range extractors {
			if t := ex(c); t != "" {
				return t
			}
		}
		return ""
	}
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions || m == http.MethodTrace
}

func (m *middleware) handle(c *qinetic.Ctx) error {
	if m.cfg.Next != nil && m.cfg.Next(c) {
		return c.Next()
	}
	c.Locals(middlewareKey{}, m)
	cookie := c.Cookies(m.cfg.CookieName)
	if safeMethod(c.Method()) {
		token, err := m.issue(c, cookie)
		if err != nil {
			return err
		}
		c.Locals(localsKey{}, token)
		return c.Next()
	}
	if err := m.checkOrigin(c); err != nil {
		return err
	}
	sent := m.cfg.Extractor(c)
	if cookie == "" || sent == "" {
		return ErrMissingToken
	}
	if subtle.ConstantTimeCompare([]byte(cookie), []byte(sent)) != 1 {
		return ErrInvalidToken
	}
	if _, ok, err := m.lookup(c, cookie); err != nil {
		return err
	} else if !ok {
		return ErrInvalidToken
	}
	c.Locals(localsKey{}, cookie)
	return c.Next()
}

// checkOrigin refuses a request a browser marked as coming from another site, as
// net/http.CrossOriginProtection does: Sec-Fetch-Site when sent, else Origin compared with Host. Requests
// with neither, from older browsers or non-browser clients, are left to the token check.
func (m *middleware) checkOrigin(c *qinetic.Ctx) error {
	origin := c.Get("Origin")
	if site := c.Get("Sec-Fetch-Site"); site != "" {
		if site == "same-origin" || site == "none" || m.trustedOrigin(origin) {
			return nil
		}
		return ErrCrossOrigin
	}
	if origin == "" {
		return nil
	}
	if m.trustedOrigin(origin) {
		return nil
	}
	if u, err := url.Parse(origin); err == nil && u.Host != "" && strings.EqualFold(u.Host, c.Host()) {
		return nil
	}
	return ErrCrossOrigin
}

func (m *middleware) trustedOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	norm, err := normalizeOrigin(origin)
	return err == nil && m.trusted[norm]
}

// issue returns the request's token if it is still stored, extending it past half its life, or a new one.
func (m *middleware) issue(c *qinetic.Ctx, cookie string) (string, error) {
	if cookie != "" {
		exp, ok, err := m.lookup(c, cookie)
		if err != nil {
			return "", err
		}
		if ok && exp.Sub(now()) > m.cfg.Expiration/2 {
			return cookie, nil
		}
		if ok {
			return cookie, m.store(c, cookie)
		}
	}
	var b [32]byte
	_, _ = rand.Read(b[:]) // never fails; crypto/rand panics if the OS source is broken
	token := base64.RawURLEncoding.EncodeToString(b[:])
	return token, m.store(c, token)
}

func (m *middleware) store(c *qinetic.Ctx, token string) error {
	exp := now().Add(m.cfg.Expiration)
	if err := m.cfg.Storage.Set(c.Context(), storageKey(token), []byte(strconv.FormatInt(exp.UnixNano(), 10)), m.cfg.Expiration); err != nil {
		return err
	}
	c.Cookie(m.cookie(c, token, int(m.cfg.Expiration/time.Second)))
	return nil
}

// lookup reports whether token is stored and when it expires.
func (m *middleware) lookup(c *qinetic.Ctx, token string) (time.Time, bool, error) {
	b, err := m.cfg.Storage.Get(c.Context(), storageKey(token))
	if err != nil || b == nil {
		return time.Time{}, false, err
	}
	ns, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return time.Time{}, false, nil
	}
	exp := time.Unix(0, ns)
	return exp, now().Before(exp), nil
}

func (m *middleware) cookie(c *qinetic.Ctx, value string, maxAge int) *qinetic.Cookie {
	return &qinetic.Cookie{
		Name: m.cfg.CookieName, Value: value, Path: m.cfg.CookiePath, Domain: m.cfg.CookieDomain,
		MaxAge: maxAge, Secure: c.Secure(), HTTPOnly: !m.cfg.CookieScriptAccess, SameSite: m.cfg.CookieSameSite,
	}
}

// storageKey hashes the token, so the storage never holds a usable token, such as in a dump of Redis.
func storageKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "csrf:" + hex.EncodeToString(sum[:])
}

// TokenFromContext returns the request's token, to embed in a form field or meta tag, or "" when the
// middleware did not run.
func TokenFromContext(c *qinetic.Ctx) string {
	t, _ := c.Locals(localsKey{}).(string)
	return t
}

// ErrNoMiddleware is returned by DeleteToken for a request the middleware did not handle.
var ErrNoMiddleware = errors.New("qinetic/csrf: DeleteToken called on a request the csrf middleware did not handle")

// DeleteToken revokes the request's token and expires its cookie, as after login or logout, so the next
// safe request issues a new one. It must run after the middleware, in the same request.
func DeleteToken(c *qinetic.Ctx) error {
	m, ok := c.Locals(middlewareKey{}).(*middleware)
	if !ok {
		return ErrNoMiddleware
	}
	if token := c.Cookies(m.cfg.CookieName); token != "" {
		if err := m.cfg.Storage.Delete(c.Context(), storageKey(token)); err != nil {
			return err
		}
	}
	c.Cookie(m.cookie(c, "", -1))
	c.Locals(localsKey{}, "")
	return nil
}

func normalizeOrigin(o string) (string, error) {
	u, err := url.Parse(o)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("want scheme://host[:port]")
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}
