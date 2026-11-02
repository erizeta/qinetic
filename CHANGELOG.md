# Changelog

All notable changes to qinetic are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and qinetic follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html): before v1.0.0, minor versions may contain breaking
changes, each listed with what to change.

## [Unreleased]

## [0.1.0] - 2026-11-02

The first release: a Fiber-style framework on qilat, serving HTTP/1.1, HTTP/2, and HTTP/3 from one `App`.
Minimum Go version: 1.26; it requires qilat v0.2.1. [docs/migrating-from-fiber.md](docs/migrating-from-fiber.md)
lists what differs from Fiber v3.

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
- `Config` with `ErrorHandler`, `Logger`, `CaseSensitive`, `StrictRouting`, `DebugPoison`, `BodyLimit`,
  `ProxyHeader`, `TrustedProxies`, `JSONEncoder`, and `JSONDecoder`.
- Request API: `Method`, `Path`, `OriginalURL`, `Host`, `Hostname`, `IP`, `IPs`, `Secure`, `Params`, `ParamsInt`,
  `Query`, `QueryInt`, `QueryBool`, `Queries`, `Get`, `Cookies`, `Body`, `BodyReader`, `FormValue`,
  `MultipartForm`, `FormFile`, `Locals`, `Route`, and `Request`. String accessors return copies; `PeekParam`,
  `PeekQuery`, and `PeekHeader` return `[]byte` without copying or allocating. `Body` returns an error, with
  `ErrRequestEntityTooLarge` past `BodyLimit` (4 MiB by default).
- `IP`, `Secure`, `Host`, and `Hostname` believe `ProxyHeader`, `X-Forwarded-Proto`, and `X-Forwarded-Host`
  only from `TrustedProxies`. `IP` takes the rightmost untrusted address and `Host` the last forwarded host, the
  one the nearest proxy set, so clients cannot spoof them.
- Response API: `Set`, `GetRespHeader`, `Append`, `Vary`, `Type`, `SendStatus`, `XML`, `Redirect` (303 by default;
  refuses control characters), `Cookie`, `ClearCookie`, `Attachment`, `Download`, `SendFile` (refuses `..`),
  `SendStream`, `Writer`, `Flush`, and `Trailer`. `SendString` defaults Content-Type to
  `text/plain; charset=utf-8`.
- Serving: `Listen`, `ListenTLS` (HTTP/1.1 and HTTP/2 over TCP, HTTP/3 over UDP on the same port),
  `ListenTLSWithCertificate`, `Listener`, `ListenerTLS`, and `ListenQUIC`, with `Shutdown`,
  `ShutdownWithTimeout`, and `OnShutdown` hooks. Concurrent Listen calls share one `qilat.Server`, and after
  `Shutdown` they return only once in-flight requests and hooks are done. `Config` gains `TLSConfig`,
  `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, `H2C`, `DisableHTTP3`, and `Server` for any other
  `qilat.Server` setting; `BodyLimit`, `Logger`, and `DebugPoison` also apply to the server.
- Middleware packages `recovery`, `logger`, `requestid`, `cors`, `compress`, and `static`, each with a `Next`
  option. Static files are mounted with `app.Use(prefix, static.New(root))` or a wildcard route, and opened
  through `os.Root`, so a symlink cannot lead outside the root.
- `Storage` and `Incrementer` interfaces for middleware state, with an in-memory default that starts no
  goroutine and holds at most 100,000 entries.
- `limiter` middleware: fixed or sliding window, `X-RateLimit-*` headers, 429 with `Retry-After`,
  `SkipFailedRequests`/`SkipSuccessfulRequests`, atomic with an `Incrementer` storage. Counters are keyed by
  algorithm and a hash of the client key, so limiters can share a storage.
- `csrf` middleware: origin check from `Sec-Fetch-Site`, `Origin` (scheme and host), and, over HTTPS without
  `Origin`, `Referer`, plus a stored token required for every unsafe request; `TokenFromContext` and
  `DeleteToken`. Over HTTPS the cookie is `__Host-csrf_`, which a subdomain cannot set.
- `cache` middleware: stores GET responses with one copy per `Vary` value, `MaxVariants` and `MaxBytes`
  bounds, versioned entries, and storage outages served uncached. Requests with `Authorization` or `Cookie`
  bypass it unless `CacheCookieRequests` is set, so a personal page is never served to another user.
- `c.ResponseHeader`, `c.ResponseBody`, and `c.Committed` for middleware that read the response.
- Adapters `FromQilatMiddleware`, `FromStdMiddleware`, `FromQilat`, and `FromStd`, and `c.StatusCode`,
  `c.ResponseSize`, and `c.HandleError` for middleware that report on the response. Once an adapted qilat
  handler or middleware has finished the response, further body writes return `ErrResponseSent` instead of
  appending raw bytes to a body it framed or compressed.
- `c.Bind()` with `JSON`, `XML`, `Form`, `Query`, and `Body`. Form and query keys match `form`/`query` tags,
  or field names, case-insensitively as in Fiber; malformed input is a 400 that never includes decoder output.

[Unreleased]: https://github.com/erizeta/qinetic/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/erizeta/qinetic/releases/tag/v0.1.0
