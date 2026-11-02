// Package recovery turns panics in later handlers into a 500 handled by the App's ErrorHandler.
package recovery

import (
	"log/slog"
	"runtime/debug"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
)

// Config configures the middleware.
type Config struct {
	// Next skips the middleware when it returns true.
	Next func(c *qinetic.Ctx) bool
	// EnableStackTrace adds the goroutine stack to the log record.
	EnableStackTrace bool
	// Logger receives a record for each recovered panic. If nil, slog.Default is used.
	Logger *slog.Logger
}

// New returns middleware that recovers a panic from the handlers after it, logs it, and returns
// qinetic.ErrInternalServerError, so the client gets a 500 from the ErrorHandler. qilat.ErrAbortHandler
// is re-panicked: it asks qilat to abort the response without logging.
func New(config ...Config) qinetic.Handler {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *qinetic.Ctx) (err error) {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == qilat.ErrAbortHandler { //nolint:errorlint // panic values are compared, not wrapped
				panic(v)
			}
			attrs := []slog.Attr{slog.Any("panic", v), slog.String("method", c.Method()),
				slog.String("path", string(c.Request().URI().RawPath()))}
			if cfg.EnableStackTrace {
				attrs = append(attrs, slog.String("stack", string(debug.Stack())))
			}
			logger.LogAttrs(c.Context(), slog.LevelError, "qinetic: panic recovered", attrs...)
			err = qinetic.ErrInternalServerError
		}()
		return c.Next()
	}
}
