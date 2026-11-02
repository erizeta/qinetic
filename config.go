package qinetic

import "log/slog"

// DefaultBodyLimit is the request body size c.Body reads when Config.BodyLimit is zero.
const DefaultBodyLimit = 4 << 20

// Config configures an App. The zero value is ready to use.
type Config struct {
	// ErrorHandler handles errors returned by handlers. If nil, DefaultErrorHandler is used.
	ErrorHandler ErrorHandler

	// Logger receives errors that are not sent to the client. If nil, slog.Default is used.
	Logger *slog.Logger

	// CaseSensitive makes "/Users" and "/users" different routes.
	CaseSensitive bool

	// StrictRouting makes "/users" and "/users/" different routes.
	StrictRouting bool

	// DebugPoison makes any use of a Ctx after its handler returned panic, instead of reusing it.
	// It helps find handlers that keep c in a goroutine; it disables Ctx pooling.
	DebugPoison bool

	// BodyLimit is the largest request body, in bytes, that c.Body and form parsing read; larger bodies
	// fail with ErrRequestEntityTooLarge. Zero means DefaultBodyLimit and a negative value means no limit.
	BodyLimit int64

	// ProxyHeader names the header holding the client IP set by a reverse proxy, such as X-Forwarded-For.
	// It is used by c.IP only when the connection comes from one of TrustedProxies.
	ProxyHeader string

	// TrustedProxies lists the IPs or CIDR prefixes of reverse proxies whose ProxyHeader and
	// X-Forwarded-Proto are believed. New panics on an invalid entry.
	TrustedProxies []string

	// JSONEncoder and JSONDecoder replace encoding/json for c.JSON and c.Bind().JSON.
	JSONEncoder func(v any) ([]byte, error)
	JSONDecoder func(data []byte, v any) error
}
