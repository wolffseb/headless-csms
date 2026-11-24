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
- shows the stations in a **terminal UI** (`cpms run`): select an EVSE, then reserve, start,
  stop, unlock or refresh it with one key;
- includes a **charge point simulator**, so all of it runs on a laptop without hardware;
- needs no database, no container and no web UI: one hand-edited config file, one binary.

Planned: OCPI commands (`RESERVE_NOW`) and OCPP 2.0.1. See the
[status](https://wolffseb.github.io/headless-csms/development/#status).

## Quickstart

Needs Go 1.25 or newer.

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

`cpms run` opens the terminal UI. Terminal 1 above looks like this:

```text
cpms · 1/2 chargers online · OCPI not registered
────────────────────────────────────────────────────────────────────────────────────────────────────
ALP-HYC-001  online  Alpitronic HYC300 fw 1.4.2
     EVSE           STATUS       DETAIL
 ▸   ALP-HYC-001-1  AVAILABLE    -
     ALP-HYC-001-2  CHARGING     session 12 · 04A1B2C3D4 · since 09:31 · 48.2 kW
ALP-HYC-002  offline  last seen 09:30:00 (heartbeat timeout)
     ALP-HYC-002-1  UNKNOWN      -
     ALP-HYC-002-2  UNKNOWN      -
── log ─────────────────────────────────────────────────────────────────────────────────────────────




09:30:00 ALP-HYC-001 connected
09:30:00 ALP-HYC-001 booted (Alpitronic HYC300 fw 1.4.2)
09:30:00 ALP-HYC-001-1 UNKNOWN→AVAILABLE
09:30:00 ALP-HYC-001-2 UNKNOWN→AVAILABLE
09:30:00 ALP-HYC-002 connected
09:30:00 ALP-HYC-002 booted (Alpitronic HYC300 fw 1.4.2)
09:30:00 ALP-HYC-002-1 UNKNOWN→AVAILABLE
09:30:00 ALP-HYC-002-2 UNKNOWN→AVAILABLE
09:31:00 ALP-HYC-002 disconnected: heartbeat timeout
09:31:00 ALP-HYC-002-1 AVAILABLE→UNKNOWN
09:31:00 ALP-HYC-002-2 AVAILABLE→UNKNOWN
09:31:00 ALP-HYC-001 session 12 started on ALP-HYC-001-2 (tag 04A1B2C3D4)
09:31:00 ALP-HYC-001-2 AVAILABLE→CHARGING


↑↓ select  r reserve  c cancel  s start  x stop  u unlock  t refresh  ? help  q quit
```

| Key | Acts on the selected EVSE |
|---|---|
| `↑` `↓` / `k` `j` | select |
| `r` `c` | reserve (default tag, 15 min) / cancel the reservation |
| `s` `x` | start a session (default tag) / stop it (asks `y`) |
| `u` `t` | unlock the cable / ask the station for its status |
| `?` `q` | help / quit (same shutdown as Ctrl-C) |

While the UI is up the log goes to `cpms.log` next to the state file. `cpms run --headless`
(or a stdout that is not a terminal) logs to stderr instead.
[More](https://wolffseb.github.io/headless-csms/guides/terminal-ui/).

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
