// Package requestid gives each request an ID, kept from the client when valid, and returns it in a header.
package requestid

import (
	"crypto/rand"

	"github.com/erizeta/qinetic"
)

// DefaultHeader is the header the ID is read from and written to.
const DefaultHeader = "X-Request-ID"

// maxLen bounds an incoming ID, so clients cannot make every log line arbitrarily long.
const maxLen = 128

// Config configures the middleware.
type Config struct {
	// Next skips the middleware when it returns true.
	Next func(c *qinetic.Ctx) bool
	// Header is the request and response header holding the ID. If empty, DefaultHeader is used.
	Header string
	// Generator returns a new ID. If nil, IDs are 26 random base32 characters from crypto/rand.
	Generator func() string
}

type localsKey struct{}

// New returns middleware that keeps a valid incoming ID, at most 128 characters of letters, digits,
// '-', '_', '.', and ':', or else generates one. The ID is set on the response and stored for FromContext.
func New(config ...Config) qinetic.Handler {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.Header == "" {
		cfg.Header = DefaultHeader
	}
	if cfg.Generator == nil {
		cfg.Generator = rand.Text
	}
	return func(c *qinetic.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}
		id := c.Get(cfg.Header)
		if !valid(id) {
			id = cfg.Generator()
		}
		c.Set(cfg.Header, id)
		c.Locals(localsKey{}, id)
		return c.Next()
	}
}

func valid(id string) bool {
	if id == "" || len(id) > maxLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch b := id[i]; {
		case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9', b == '-', b == '_', b == '.', b == ':':
		default:
			return false
		}
	}
	return true
}

// FromContext returns the request's ID, or "" when the middleware did not run.
func FromContext(c *qinetic.Ctx) string {
	id, _ := c.Locals(localsKey{}).(string)
	return id
}
