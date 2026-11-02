# Contributing to qinetic

Thanks for your interest in qinetic. This document explains how to propose changes.

## Before you start

- For anything bigger than a small fix, open an issue first so the approach can be discussed.
- Check the [changelog](CHANGELOG.md) to see where the change fits.
- Security issues go through [SECURITY.md](SECURITY.md), not public issues.

## Development setup

Requirements:

- Go 1.26 or newer
- `make`

Common commands:

| Command | What it does |
|---|---|
| `make fmt` | Format code with `gofmt` |
| `make lint` | Run `gofmt` check, `go vet`, and golangci-lint |
| `make test` | Run unit tests and benchmark sanity checks |
| `make race` | Run the same with the race detector |
| `make cover` | Run tests with coverage and print a summary |
| `make bench` | Run benchmarks, including comparisons in `benchmarks/` |
| `make fuzz` | Run every fuzz target for `FUZZTIME` each (default `30s`) |
| `make vuln` | Run `govulncheck` |

golangci-lint and govulncheck are run with `go run` at pinned versions, so no global install is needed.

## Pull requests

- Keep each pull request focused on one change.
- Add or update tests for every behavior change.
- Performance claims must include benchmark results (`make bench`, ideally compared with
  [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat)).
- Do not use standard library APIs newer than the minimum Go version in `go.mod`; CI tests on it.
- Make sure `make lint race` passes locally.
- Update `README.md` when you add or change user-facing behavior.
- Add an entry under `[Unreleased]` in `CHANGELOG.md` for user-facing changes, with migration notes for
  breaking ones.

## Commit messages

Use a short imperative summary, optionally prefixed with the affected area:

```
router: reject duplicate parameter names
```

## License

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).
