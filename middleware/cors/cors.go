// Package cors implements Cross-Origin Resource Sharing: it answers preflight requests and adds
// Access-Control-* headers for allowed origins.
package cors

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/erizeta/qinetic"
)

// Config configures the middleware.
type Config struct {
	// Next skips the middleware when it returns true.
	Next func(c *qinetic.Ctx) bool
	// AllowOrigins lists allowed origins: "*", exact origins such as "https://example.com", or one
	// wildcard subdomain level such as "https://*.example.com", which also matches a.b.example.com but
	// not example.com. If empty and AllowOriginsFunc is nil, "*" is used. New panics on a malformed entry.
	AllowOrigins []string
	// AllowOriginsFunc allows further origins, checked after AllowOrigins. The origin it gets is as sent.
	AllowOriginsFunc func(origin string) bool
	// AllowMethods are sent in preflight responses. If nil, GET, POST, HEAD, PUT, DELETE, and PATCH.
	AllowMethods []string
	// AllowHeaders are sent in preflight responses. If nil, the headers the browser asked for are allowed.
	AllowHeaders []string
	// AllowCredentials lets browsers send cookies and read the response. It cannot be combined with "*".
	AllowCredentials bool
	// ExposeHeaders lists response headers scripts may read, beyond the CORS-safelisted ones.
	ExposeHeaders []string
	// MaxAge is how long, in seconds, browsers may cache a preflight result. Zero omits the header and a
	// negative value sends 0, disabling the cache.
	MaxAge int
}

type wildcard struct{ prefix, suffix string }

// New returns the middleware. It panics when AllowCredentials is combined with the "*" origin, since
// browsers refuse that combination and echoing every origin instead would allow any site.
func New(config ...Config) qinetic.Handler {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	if len(cfg.AllowOrigins) == 0 && cfg.AllowOriginsFunc == nil {
		cfg.AllowOrigins = []string{"*"}
	}
	anyOrigin := false
	exact := map[string]bool{}
	var wildcards []wildcard
	for _, o := range cfg.AllowOrigins {
		switch {
		case o == "*":
			anyOrigin = true
		case strings.Contains(o, "*"):
			w, err := parseWildcard(o)
			if err != nil {
				panic(fmt.Sprintf("qinetic/cors: invalid origin %q: %v", o, err))
			}
			wildcards = append(wildcards, w)
		default:
			norm, err := normalizeOrigin(o)
			if err != nil {
				panic(fmt.Sprintf("qinetic/cors: invalid origin %q: %v", o, err))
			}
			exact[norm] = true
		}
	}
	if anyOrigin && cfg.AllowCredentials {
		panic(`qinetic/cors: AllowCredentials cannot be used with the "*" origin`)
	}
	methods := strings.Join(cfg.AllowMethods, ", ")
	if cfg.AllowMethods == nil {
		methods = "GET, POST, HEAD, PUT, DELETE, PATCH"
	}
	headers := strings.Join(cfg.AllowHeaders, ", ")
	expose := strings.Join(cfg.ExposeHeaders, ", ")
	maxAge := ""
	if cfg.MaxAge > 0 {
		maxAge = strconv.Itoa(cfg.MaxAge)
	} else if cfg.MaxAge < 0 {
		maxAge = "0"
	}
	allowed := func(origin string) bool {
		o := strings.ToLower(origin)
		if exact[o] {
			return true
		}
		for _, w := range wildcards {
			if w.match(o) {
				return true
			}
		}
		return cfg.AllowOriginsFunc != nil && cfg.AllowOriginsFunc(origin)
	}

	return func(c *qinetic.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}
		// Responses differ by Origin, so caches must key on it even when no CORS header is added.
		c.Vary("Origin")
		origin := c.Get("Origin")
		preflight := c.Method() == http.MethodOptions && c.Get("Access-Control-Request-Method") != ""
		if origin == "" {
			return c.Next()
		}
		if preflight {
			c.Vary("Access-Control-Request-Method", "Access-Control-Request-Headers")
		}
		var allowOrigin string
		switch {
		case anyOrigin:
			allowOrigin = "*"
		case allowed(origin):
			allowOrigin = origin
		default:
			// A refused preflight gets no CORS headers, so the browser blocks the request it announces.
			if preflight {
				return c.SendStatus(http.StatusNoContent)
			}
			return c.Next()
		}
		c.Set("Access-Control-Allow-Origin", allowOrigin)
		if cfg.AllowCredentials {
			c.Set("Access-Control-Allow-Credentials", "true")
		}
		if !preflight {
			if expose != "" {
				c.Set("Access-Control-Expose-Headers", expose)
			}
			return c.Next()
		}
		c.Set("Access-Control-Allow-Methods", methods)
		if h := headers; h != "" {
			c.Set("Access-Control-Allow-Headers", h)
		} else if req := c.Get("Access-Control-Request-Headers"); req != "" {
			c.Set("Access-Control-Allow-Headers", req)
		}
		if maxAge != "" {
			c.Set("Access-Control-Max-Age", maxAge)
		}
		return c.SendStatus(http.StatusNoContent)
	}
}

// normalizeOrigin checks that o is scheme://host[:port] with nothing else and returns it in lower case.
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

func parseWildcard(o string) (wildcard, error) {
	scheme, rest, ok := strings.Cut(o, "://*.")
	if !ok || strings.Contains(rest, "*") {
		return wildcard{}, fmt.Errorf(`a wildcard must be one "*." right after the scheme, as in https://*.example.com`)
	}
	if _, err := normalizeOrigin(scheme + "://x." + rest); err != nil {
		return wildcard{}, err
	}
	return wildcard{prefix: strings.ToLower(scheme + "://"), suffix: strings.ToLower("." + rest)}, nil
}

// match reports whether the lower-case origin o is a subdomain the wildcard covers. The part the "*"
// stands for may only hold host characters, so "https://evil.com/.example.com" does not match.
func (w wildcard) match(o string) bool {
	if len(o) <= len(w.prefix)+len(w.suffix) || !strings.HasPrefix(o, w.prefix) || !strings.HasSuffix(o, w.suffix) {
		return false
	}
	sub := o[len(w.prefix) : len(o)-len(w.suffix)]
	for i := 0; i < len(sub); i++ {
		if b := sub[i]; (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '-' && b != '.' {
			return false
		}
	}
	return sub[0] != '.' && sub[len(sub)-1] != '.'
}
