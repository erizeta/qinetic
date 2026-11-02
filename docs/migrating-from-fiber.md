# Migrating from Fiber

Qinetic follows Fiber's API closely, so most handlers move over by changing imports and a few signatures.
This guide lists what differs, and why. It assumes Fiber v3.

## The engine

Fiber runs on fasthttp; Qinetic runs on [qilat](https://github.com/erizeta/qilat). The visible effects:

- One `App` serves HTTP/1.1, HTTP/2, and HTTP/3. `app.ListenTLS` opens TCP and UDP on the same port and
  advertises HTTP/3 with `Alt-Svc`.
- `c.Context()` is canceled when the client disconnects, not only at shutdown, so database calls and other
  work stop when nobody is waiting for the answer.
- The request body is streamed, not read up front. That is why `c.Body()` returns an error (below).

## Handlers and the Ctx

| Fiber v3 | Qinetic | Notes |
|---|---|---|
| `fiber.Ctx` (interface) | `*qinetic.Ctx` (struct) | Handlers are `func(c *qinetic.Ctx) error`. |
| `c.Params("id")` and friends return unsafe strings unless `Immutable` | Always copies | No `Immutable` setting is needed. `PeekParam`, `PeekQuery`, and `PeekHeader` return `[]byte` without copying, valid until the handler returns. |
| `c.Body() []byte` | `c.Body() ([]byte, error)` | Fails with `ErrRequestEntityTooLarge` past `Config.BodyLimit` (4 MiB by default), or a 400 when reading fails. |
| `c.Redirect().To(loc)` | `c.Redirect(loc, code...)` | 303 by default. Locations with control characters are refused. |
| `c.SendFile(file, cfg)` | `c.SendFile(file)` | A name with a `..` element, as a decoded `%2F..%2F` parameter gives, fails with 400. |
| `c.Locals(key, value...)` | Same | Keys are `any`. |
| `c.Route()` returns `*fiber.Route` | `c.Route()` returns a `qinetic.Route` copy | Changing it cannot change the route for other requests. |
| `c.Bind().JSON(&v)`, `.Form`, `.Query`, `.Body` | Same | Errors are 400s that never include decoder output. |
| `c.Cookie(&fiber.Cookie{...})` | `c.Cookie(&qinetic.Cookie{...})` | A cookie net/http considers invalid is not sent, and the reason is logged. |
| `c.Hostname()`, `c.IP()`, `c.Secure()` with `TrustProxy` | Same, with `Config.TrustedProxies` | `c.IP()` takes the rightmost untrusted address of `Config.ProxyHeader`; `c.Host()` the last `X-Forwarded-Host`. |
| `app.Test(req, cfg)` | `app.Test(req)` | In memory, without a server; a handler panic reaches the caller. |

## Routing

The route stack works as in Fiber: handlers run in registration order, `Use` matches a prefix at segment
boundaries, `Get` also registers HEAD, and `c.Next()` moves to the next match. The same order is checked
against Fiber in the repository's tests. The pattern syntax is narrower:

- Parameters and wildcards must be whole segments: `/users/:id`, `/files/*`, `/api/+`. Fiber's in-segment
  forms, `/:from-:to`, `/:file.:ext`, and `/api*`, panic at registration with the reason. Split them into
  segments, or parse the segment in the handler.
- `:name?` is only allowed as the last segment.
- Constraints such as `:id<int>` are not supported; parse with `c.ParamsInt` and return `ErrBadRequest`.
- A literal `:`, `*`, or `+` in a static segment is written percent-encoded: `/v1/users%3Abatch`.
- Routing uses the raw path, so `%2F` never splits a segment; `c.Params` decodes it.
- Paths with empty or dot segments (`//`, `/./`, `/../`, encoded or not) are not routed: only middleware
  registered on `/` runs, then 404. Fiber normalizes them instead.
- Methods outside the standard list are routed like any other; Fiber answers them with 501.

## Errors

A handler error goes to `Config.ErrorHandler`, as in Fiber. The default differs for errors that are not a
`*qinetic.Error`: Fiber sends `err.Error()` to the client, Qinetic sends a generic 500 and logs the error to
`Config.Logger`, so internal details never leak. Return `qinetic.NewError(code, message)` for messages the
client should see.

## Serving and shutdown

| Fiber v3 | Qinetic |
|---|---|
| `app.Listen(":3000", fiber.ListenConfig{...})` | `app.Listen(":3000")`; options go in `qinetic.Config` |
| TLS through `ListenConfig.CertFile` | `app.ListenTLS(addr, cert, key)` or `app.ListenTLSWithCertificate(addr, cert)`, HTTP/3 included |
| `app.Listener(ln)` | `app.Listener(ln)`, `app.ListenerTLS(ln)`, and `app.ListenQUIC(pc)` |
| `app.Shutdown()` and shutdown hooks | `app.Shutdown(ctx)` and `app.OnShutdown(fn)` |

After `Shutdown`, a running `Listen` returns `qilat.ErrServerClosed` only once in-flight requests have
finished and the hooks have run, so returning from `main` right after it is safe.

## Middleware

Import paths move from `github.com/gofiber/fiber/v3/middleware/...` to
`github.com/erizeta/qinetic/middleware/...`. Each package keeps `New(config ...Config)` and a `Next` option.

| Fiber v3 | Qinetic | Notes |
|---|---|---|
| `recover` | `recovery` | Renamed so importing it does not shadow the builtin. |
| `logger` | `logger` | Writes `slog` records; the path is logged without its query. |
| `requestid` | `requestid` | Incoming IDs are kept only when at most 128 safe characters. |
| `cors` | `cors` | `*` with credentials panics at startup instead of echoing every origin. |
| `compress` | `compress` | Built on `qilat.CompressWith`. |
| `static` and `app.Static` | `static` | `app.Use("/assets", static.New("./public"))`; there is no `app.Static`. Files are opened through `os.Root`, so symlinks cannot leave the root. |
| `limiter` | `limiter` | Rejected requests do not use up the quota. |
| `csrf` | `csrf` | The token is required on every unsafe request, Origin is compared on scheme and host, and over HTTPS the cookie is `__Host-csrf_`. The cookie is HttpOnly by default; use `CookieScriptAccess` for SPAs. |
| `cache` | `cache` | One copy per `Vary` value; requests with `Cookie` or `Authorization` bypass it by default. Register it after `compress`. |

Fiber middleware can also be replaced by net/http or qilat middleware through `qinetic.FromStdMiddleware`
and `qinetic.FromQilatMiddleware`.
