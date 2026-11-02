package qinetic

import (
	"errors"
	"log/slog"
	"net/http"
)

// Error is an error with an HTTP status code. DefaultErrorHandler sends its Code and Message to the client.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

// NewError returns an Error with the given code and message, or the code's status text when message is empty.
func NewError(code int, message ...string) *Error {
	msg := http.StatusText(code)
	if len(message) > 0 {
		msg = message[0]
	}
	return &Error{Code: code, Message: msg}
}

// Errors for common client and server failures, sent as is by DefaultErrorHandler.
var (
	ErrBadRequest            = NewError(http.StatusBadRequest)
	ErrUnauthorized          = NewError(http.StatusUnauthorized)
	ErrForbidden             = NewError(http.StatusForbidden)
	ErrNotFound              = NewError(http.StatusNotFound)
	ErrMethodNotAllowed      = NewError(http.StatusMethodNotAllowed)
	ErrNotAcceptable         = NewError(http.StatusNotAcceptable)
	ErrRequestTimeout        = NewError(http.StatusRequestTimeout)
	ErrConflict              = NewError(http.StatusConflict)
	ErrRequestEntityTooLarge = NewError(http.StatusRequestEntityTooLarge)
	ErrUnsupportedMediaType  = NewError(http.StatusUnsupportedMediaType)
	ErrUnprocessableEntity   = NewError(http.StatusUnprocessableEntity)
	ErrTooManyRequests       = NewError(http.StatusTooManyRequests)
	ErrInternalServerError   = NewError(http.StatusInternalServerError)
	ErrNotImplemented        = NewError(http.StatusNotImplemented)
	ErrServiceUnavailable    = NewError(http.StatusServiceUnavailable)
)

// ErrorHandler handles an error returned by a handler. It may write a new response; the body written
// before the error has already been discarded.
type ErrorHandler = func(c *Ctx, err error) error

// DefaultErrorHandler sends an *Error's code and message. Any other error becomes a 500 with a generic
// message, so internal details never reach the client; the error itself goes to Config.Logger.
func DefaultErrorHandler(c *Ctx, err error) error {
	code, msg := http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError)
	if e, ok := errors.AsType[*Error](err); ok {
		code, msg = e.Code, e.Message
	} else {
		c.app.logError(c, "handler error", err)
	}
	c.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	return c.Status(code).SendString(msg)
}

func (app *App) logError(c *Ctx, msg string, err error) {
	app.config.Logger.LogAttrs(c.r.Context(), slog.LevelError, "qinetic: "+msg,
		slog.String("method", c.r.Method()), slog.String("path", string(c.r.URI().RawPath())),
		slog.Any("error", err))
}
