// Package logger writes one slog record per request.
package logger

import (
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/middleware/requestid"
)

// Fields a record can hold, in this order.
const (
	FieldMethod    = "method"
	FieldPath      = "path"
	FieldStatus    = "status"
	FieldLatency   = "latency"
	FieldBytes     = "bytes"
	FieldProto     = "proto"
	FieldIP        = "ip"
	FieldRequestID = "request_id"
)

var allFields = []string{FieldMethod, FieldPath, FieldStatus, FieldLatency, FieldBytes, FieldProto, FieldIP, FieldRequestID}

// Config configures the middleware.
type Config struct {
	// Next skips the middleware when it returns true.
	Next func(c *qinetic.Ctx) bool
	// Logger receives the records. If nil, slog.Default is used.
	Logger *slog.Logger
	// Message is the record message. If empty, "request" is used.
	Message string
	// Fields selects the attributes, from the Field constants. If nil, all are logged. The path is the raw
	// request path without its query, so tokens in query strings are not logged. request_id is logged
	// when the requestid middleware ran before this one.
	Fields []string
}

// New returns middleware that logs each request after the handlers after it have run: Error for 5xx,
// Warn for 4xx, Info otherwise. An error from those handlers is passed to the ErrorHandler first, so the
// record shows the status the client gets. Put it before recovery, so recovered panics are logged as 500s.
func New(config ...Config) qinetic.Handler {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	msg := cfg.Message
	if msg == "" {
		msg = "request"
	}
	fields := cfg.Fields
	if fields == nil {
		fields = allFields
	}
	for _, f := range fields {
		if !slices.Contains(allFields, f) {
			panic(fmt.Sprintf("qinetic/logger: unknown field %q", f))
		}
	}
	return func(c *qinetic.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}
		start := time.Now()
		if err := c.Next(); err != nil {
			c.HandleError(err)
		}
		latency := time.Since(start)
		status := c.StatusCode()
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}
		attrs := make([]slog.Attr, 0, len(fields))
		for _, f := range fields {
			switch f {
			case FieldMethod:
				attrs = append(attrs, slog.String(f, c.Method()))
			case FieldPath:
				attrs = append(attrs, slog.String(f, string(c.Request().URI().RawPath())))
			case FieldStatus:
				attrs = append(attrs, slog.Int(f, status))
			case FieldLatency:
				attrs = append(attrs, slog.Duration(f, latency))
			case FieldBytes:
				attrs = append(attrs, slog.Int64(f, c.ResponseSize()))
			case FieldProto:
				attrs = append(attrs, slog.String(f, c.Protocol()))
			case FieldIP:
				attrs = append(attrs, slog.String(f, c.IP()))
			case FieldRequestID:
				if id := requestid.FromContext(c); id != "" {
					attrs = append(attrs, slog.String(f, id))
				}
			}
		}
		logger.LogAttrs(c.Context(), level, msg, attrs...)
		return nil
	}
}
