# Development

## Build and test

**You need:** [Go](https://go.dev/dl/) 1.24 or newer and `make`.

| Command | Does |
|---|---|
| `make build` | Compiles `bin/cpms` |
| `make test` | Runs the test suite |
| `make race` | The same under the race detector |
| `make lint` | `golangci-lint` (see below) |
| `make ci` | Everything CI runs: gofmt check, `go vet`, lint, `go test -race` |

```sh
# the linter, once
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2
```

No hardware is needed: every test runs against the built-in
[simulator](guides/simulator.md).

!!! tip "Golden files"
    The exact OCPP 1.6 payloads are pinned in `internal/ocpp/v16/testdata/golden`.
    Regenerate them deliberately:

    ```sh
    go test ./internal/ocpp/v16 -run Golden -update
    ```

## Where things are

Start with the [architecture overview](architecture/overview.md). The short version:

| To change... | Look in |
|---|---|
| A CLI command or its output | `internal/cli` |
| What happens on reserve, start, stop | `internal/control` |
| An OCPP 1.6 message | `internal/ocpp/v16` |
| An OCPI endpoint, or the status push | `internal/ocpi/...` |
| A config field | `internal/config`, and `config.example.yaml` |
| The simulator | `internal/simulator` |

## These docs

The site is built with [Zensical](https://zensical.org). Pages are Markdown under `docs/`,
the navigation is in `zensical.toml`.

```sh
pip install -r requirements-docs.txt
zensical serve            # http://localhost:8000, reloads on save
zensical build --strict   # what CI runs: a broken link fails the build
```

A push to `main` that touches the docs publishes them to GitHub Pages
(`.github/workflows/docs.yml`).

## Status

Built in tracked steps. Each lands as its own pull request.

| # | Scope | State |
|---|---|---|
| 1 | Project skeleton, config loader, CI | :material-check: done |
| 2 | OCPP 1.6-J CSMS: WebSocket server and message routing | :material-check: done |
| 3 | Built-in charge point simulator | :material-check: done |
| 4 | Outbound OCPP commands, reservation lifecycle, one-shot CLI | in progress: commands and `cpms probe` done, [on-site hardware check](https://github.com/wolffseb/headless-csms/issues/11) next |
| 5 | OCPI foundation: versions + credentials handshake | :material-check: done |
| 6 | OCPI Locations module (sender) | :material-check: done |
| 7 | PATCH location on status change | :material-check: done |
| 8 | OCPI Commands module (receiver) | :material-check: done |
| 9 | Terminal UI | |
| 10 | OCPP 2.0.1 | |
| 11 | Docs and release builds | |

Also done, outside the numbered steps: meter readings, `cpms config set`, and
[smart charging](guides/smart-charging.md).

**Out of scope for now:** the OCPI Booking module, CDRs, sessions, tariffs, payment
terminals, multiple OCPI versions, and any web UI.

| | |
|---|---|
| Open work | [Issues](https://github.com/wolffseb/headless-csms/issues) |
| The original plan | [`docs/ROADMAP.md`](https://github.com/wolffseb/headless-csms/blob/main/docs/ROADMAP.md) |
| Ideas worth keeping but not planned | [Idea fridge](https://github.com/wolffseb/headless-csms/blob/main/docs/IDEAS.md) |

## License

[MIT](https://github.com/wolffseb/headless-csms/blob/main/LICENSE).
