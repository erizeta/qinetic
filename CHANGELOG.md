# Changelog

All notable changes to qinetic are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and qinetic follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html): before v1.0.0, minor versions may contain breaking
changes, each listed with what to change.

## [Unreleased]

### Added

- `New` and `App`, a `qilat.Handler` with `Handler()` and `Test(*http.Request)` for in-memory tests.
- Routing with `Use` (prefix middleware), `Get` (also registers HEAD), `Head`, `Post`, `Put`, `Patch`, `Delete`,
  `Options`, `Connect`, `Trace`, `All`, `Add`, `Group`, and `Route`. Patterns support `:name`, `:name?`, `*`,
  and `+` as whole segments; handlers run in registration order through `c.Next()`, as in Fiber.
- 404 for unmatched paths and 405 with an `Allow` header when other methods match. Paths with empty or dot
  segments, encoded or not, are never routed; only middleware registered on `/` runs for them.
- `Ctx` with `Next`, `Context`, `Protocol`, `Status`, `Send`, `SendString`, and `JSON`. Responses are buffered
  and sent with a `Content-Length`, so an `ErrorHandler` can replace them.
- `Error`, `NewError`, common `Err*` values, and `DefaultErrorHandler`, which sends only `*Error` messages to
  clients and logs other errors to `Config.Logger`.
- `Config` with `ErrorHandler`, `Logger`, `CaseSensitive`, `StrictRouting`, and `DebugPoison`.
