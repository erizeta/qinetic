// Package compress compresses response bodies with qilat.CompressWith.
package compress

import (
	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
)

// Config configures the middleware; see qilat.CompressConfig for Level, MinSize, and Encoders.
type Config struct {
	// Next skips the middleware when it returns true.
	Next func(c *qinetic.Ctx) bool
	// Level is the gzip level. Zero means gzip.DefaultCompression.
	Level int
	// MinSize is the smallest body compressed. Zero means qilat.DefaultCompressMinSize.
	MinSize int
	// Encoders adds content codings such as zstd or br, preferred in order over gzip.
	Encoders []qilat.Encoder
}

// New returns middleware that compresses the responses of the handlers after it, with the coding the
// client prefers. Small bodies, Range requests, and types such as images are sent as is. Errors from
// those handlers are rendered by the ErrorHandler and compressed like any response. It panics on an
// invalid Level.
func New(config ...Config) qinetic.Handler {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	mw := qinetic.FromQilatMiddleware(qilat.CompressWith(qilat.CompressConfig{
		Level: cfg.Level, MinSize: cfg.MinSize, Encoders: cfg.Encoders,
	}))
	return func(c *qinetic.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}
		return mw(c)
	}
}
