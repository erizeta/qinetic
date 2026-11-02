package qinetic

import "log/slog"

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
}
