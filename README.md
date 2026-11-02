# qinetic

qinetic is a Go web framework with a [Fiber](https://github.com/gofiber/fiber)-style API, built on the
[qilat](https://github.com/erizeta/qilat) HTTP engine instead of fasthttp.

From qilat it inherits:

- HTTP/1.1, HTTP/2, and HTTP/3 served by one app;
- a request context that is canceled when the client disconnects;
- an allocation-free hot path and a `net/http` adapter.

qinetic adds routing with `:param` and `*` patterns, groups, a middleware chain with `c.Next()`, and ready-made
middleware.

## Status

qinetic is in early development and has no release yet; the API will change until v1.0.0. Routing, groups,
the `c.Next()` chain, error handling, and in-memory testing with `app.Test` work today. An `App` is a
`qilat.Handler`, so it can already be served by a `qilat.Server`; `app.Listen` and the request/response helpers
come next. See the [changelog](CHANGELOG.md).

## Installation

Requires Go 1.26 or newer.

```sh
go get github.com/erizeta/qinetic
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues through [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
