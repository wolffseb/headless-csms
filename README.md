# headless-csms

A terminal charge point management system for a single OCPP charging station.

**Documentation: <https://wolffseb.github.io/headless-csms/>**

`cpms` is one static binary that:

- speaks **OCPP 1.6-J** to the station, so you can reserve a connector, redeem the
  reservation with a known RFID tag, stop the session, unlock the cable, read power and
  current, and set charging limits;
- exposes a minimal **OCPI 2.3.0 CPO** interface on the LAN (`versions`, `credentials`,
  `locations`), so a backend can register and pull the location with live EVSE status;
- **PATCHes** EVSE status to the registered partner whenever it changes, whether the
  change came from the terminal or from someone physically plugging in a car;
- includes a **charge point simulator**, so all of it runs on a laptop without hardware;
- needs no database, no container and no web UI: one hand-edited config file, one binary.

Planned: OCPI commands (`RESERVE_NOW`), a terminal UI and OCPP 2.0.1. See the
[status](https://wolffseb.github.io/headless-csms/development/#status).

## Quickstart

Needs Go 1.24 or newer.

```sh
make build                            # -> bin/cpms
cp config.example.yaml config.yaml    # works as is
./bin/cpms config validate
```

Three terminals:

```sh
./bin/cpms run                                  # 1: the CSMS
./bin/cpms simulate charger                     # 2: a simulated station
./bin/cpms reserve --evse ALP-HYC-001-1         # 3: drive it
./bin/cpms status
./bin/cpms start --evse ALP-HYC-001-1
./bin/cpms stop --evse ALP-HYC-001-1
```

With a real station, set its CSMS URL to `ws://<host running cpms>:9000/ocpp/<charge point id>`.
**The charger dials us**; we never dial it.

## Documentation

| | |
|---|---|
| [Quickstart](https://wolffseb.github.io/headless-csms/quickstart/) | The steps above, with output |
| [Concepts](https://wolffseb.github.io/headless-csms/concepts/) | Who connects to whom, vocabulary, status mapping |
| [Connect a real station](https://wolffseb.github.io/headless-csms/guides/connect-station/) | Station setup and `cpms probe` |
| [Guides](https://wolffseb.github.io/headless-csms/guides/charging-session/) | Sessions, simulator, metering, smart charging, OCPI registration and status push |
| [CLI reference](https://wolffseb.github.io/headless-csms/reference/cli/) | Every command, flag and exit code |
| [Configuration](https://wolffseb.github.io/headless-csms/reference/configuration/) | `config.yaml` and `state.json` |
| [OCPI API](https://wolffseb.github.io/headless-csms/reference/ocpi-api/) | Endpoints, auth, examples |
| [Architecture](https://wolffseb.github.io/headless-csms/architecture/overview/) | Packages, rules, sequence diagrams |

The sources are Markdown under [`docs/`](docs/), built with [Zensical](https://zensical.org):

```sh
pip install -r requirements-docs.txt && zensical serve
```

## Development

```sh
make ci      # gofmt check, go vet, golangci-lint, go test -race
make test
```

More in [Development](https://wolffseb.github.io/headless-csms/development/). The original
plan is in [docs/ROADMAP.md](docs/ROADMAP.md), ideas that are not planned in the
[idea fridge](docs/IDEAS.md).

## License

[MIT](LICENSE).

This is a personal hobby project. It is used as an internal test environment at Fryte,
but it is not an official Fryte product and Fryte does not maintain or support it.
