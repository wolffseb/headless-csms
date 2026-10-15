# headless-csms

A terminal charge point management system for a single OCPP charging station.

`cpms` is one static binary that:

- speaks **OCPP 1.6-J** (2.0.1 later) to the station, so you can reserve a connector,
  redeem the reservation with a known RFID tag, stop the session, and unlock the cable;
- exposes a minimal **OCPI 2.3.0 CPO** interface on the LAN — `versions`, `credentials`,
  `locations` (sender) and `commands` (receiver) — so a backend can register, pull static
  location data, and send `RESERVE_NOW` that ends up at the real charger;
- **PATCHes** EVSE status to the registered counterparty whenever it changes, whether the
  change came from the terminal or from someone physically plugging in a car;
- needs no database, no container and no web UI — one hand-edited config file, one binary.

## How the charger connects

This trips people up, so it is worth stating plainly: **in OCPP-J the charger is the
WebSocket client.** It dials us; we do not dial it. Configure the station's CSMS URL as:

```
ws://<host running cpms>:9000/ocpp/<charge point id>
```

`charger.ip` in the config is only used for a reachability pre-check and for display.

## Quickstart

```sh
make build                                   # -> bin/cpms
cp config.example.yaml config.yaml           # then edit it
./bin/cpms config validate -c config.yaml
```

A valid config prints the facts you need to get the station and the counterparty
talking to you:

```
config.yaml is valid.

  Charge point        ALP-HYC-001  (OCPP 1.6)  at 192.168.1.42
  OCPP listener       0.0.0.0:9000
  Station CSMS URL    ws://<this-host>:9000/ocpp/ALP-HYC-001
  Heartbeat timeout   90s
  OCPI listener       0.0.0.0:8080
  OCPI advertised as  http://192.168.1.10:8080/ocpi
  OCPI party          DE*FYT
  Default RFID tag    04A1B2C3D4
  Location            OFFICE-01 "Fryte HQ", München (DEU) — 2 EVSEs, 2 connectors
  State file          state.json

  EVSE           EVSE ID        OCPP CONNECTOR  CONNECTORS
  ALP-HYC-001-1  DE*FYT*E001*1  1               IEC_62196_T2_COMBO/CABLE/DC 920V 500A
  ALP-HYC-001-2  DE*FYT*E001*2  2               IEC_62196_T2_COMBO/CABLE/DC 920V 500A
```

Then start the CSMS and point the station at the URL it prints:

```sh
./bin/cpms run -c config.yaml
```

```
cpms listening for ALP-HYC-001 on ws://127.0.0.1:9000/ocpp/ALP-HYC-001
waiting for the charge point to connect; press Ctrl-C to stop
INFO msg="charge point connected" charge_point=ALP-HYC-001 version=1.6
INFO msg="ALP-HYC-001 booted (Alpitronic HYC300)"
INFO msg="ALP-HYC-001-1 UNKNOWN→CHARGING"
```

`--log-level debug` shows the protocol chatter underneath.

## Testing without the station

A charge point simulator is built into the binary, so the whole thing runs on a laptop.
In a second terminal:

```sh
./bin/cpms simulate charger -c config.yaml
```

With no flags it reads the same config file and points itself at the CSMS `cpms run` is
serving, so the two commands are a working pair out of the box. The station appears in the
first terminal:

```
INFO msg="charge point connected" charge_point=ALP-HYC-001 version=1.6
INFO msg="ALP-HYC-001 booted (Alpitronic HYC300 fw simulator)"
INFO msg="ALP-HYC-001-1 UNKNOWN→AVAILABLE"
INFO msg="ALP-HYC-001-2 UNKNOWN→AVAILABLE"
```

Scenarios make it misbehave on purpose, which is how the unhappy paths get tested:

| `--scenario` | Behaviour |
|---|---|
| `normal` | A cooperative station (default) |
| `reject-reserve` | Refuses every `ReserveNow`, and stops advertising the Reservation feature profile — what a station that cannot reserve actually looks like |
| `occupied` | Answers `ReserveNow` with `Occupied` |
| `slow` | Stalls every answer past the call timeout |
| `unlock-fails` | Refuses to release the cable lock |
| `silent` | Boots, then stops sending anything while still answering pings — a station whose OCPP layer has hung but whose connection is fine. This is what `heartbeat_timeout` exists to catch, and the only way to exercise it by hand |

`--connectors`, `--id`, `--csms`, `--tag`, `--vendor` and `--model` override the config
when you want to point it somewhere else or fake a different station.

An invalid config exits 1 and names every problem by its field path, all in one pass:

```
config.yaml is not valid:

  auth.default_id_tag: "04A1 B2C3" is not a valid OCPP idTag; want 1-20 printable ASCII characters without spaces
  charger.id: is required
  location.evses[1].ocpp_connector_id: 1 is already mapped by location.evses[0]

3 problems found.
```

## Configuration

Everything lives in one YAML file — see [`config.example.yaml`](config.example.yaml),
which is commented field by field and is itself checked by the test suite.

Two files, one rule: **`config.yaml` is read-only to the tool.** It holds what you decide
(the charger, the static location data, and the two OCPI tokens that are ours to choose).
`state.json` holds what the tool learns at runtime (the counterparty's token and endpoints,
active reservations and transactions, and the id counters) and is the only file cpms writes.

`state.json` sits next to the config file by default, wherever cpms is started from;
`--state <path>` puts it elsewhere, and `cpms config validate` prints which file is in use.
It is created on the first write, not at startup, so a tool that has never been registered
leaves no trace. Writes are atomic (temp file, fsync, rename) and the file is `0600`, because it
holds the counterparty's bearer token. It is indented JSON, so `cat` works. A file cpms cannot
parse, or one written by a newer cpms, stops `cpms run` at startup with the file named, and is
never overwritten: move it aside to start from empty state.

What survives a restart: **active** reservations (with their expiry timers re-armed; one that
expired while cpms was down is dropped), **active** transactions (so the station can still stop
a session that began before the restart), and the counters behind reservation, transaction and
remote-start ids, which are never handed out twice. Reservations still waiting for the
station's answer, and finished sessions, are deliberately not kept: `state.json` is not a
history.

Any change to its schema bumps the `version` field. A cpms only reads its own version, so an
older build refuses a newer file instead of silently dropping fields it does not know.

That split is why `ocpi.token_c` is a fixed value in config rather than being generated
during the OCPI handshake as implementations usually do.

Two listener rules worth knowing, both enforced at validation time:
`heartbeat_timeout` must exceed `heartbeat_interval` (otherwise a punctual station gets
marked offline between two beats), and neither listener may use port 0 (a charger cannot
be pointed at a randomly chosen port).

## How it is put together

```
internal/core        domain state and the event bus — the single source of truth
internal/control     the one place that acts on the station: reserve, cancel, start, stop, unlock
internal/ocpp        version-agnostic RPC errors, versions, the Handler and ChargePoint interfaces
internal/ocpp/ocppj  the OCPP-J RPC layer: framing, correlation, connection goroutines
internal/ocpp/csms   the WebSocket server a charge point dials into
internal/ocpp/v16    OCPP 1.6-J payloads, the adapter that writes to core, and the command adapter
internal/simulator   a charge point: dials a CSMS and behaves like a station
internal/ocpptest    a raw OCPP-J client used by the tests
internal/state       state.json: the persisted DTOs and the atomic-write store
internal/statetest   a state store whose writes can be made to fail, for tests
internal/clocktest   a manually advanced clock, for tests that wait on expiry
```

`csms` never learns a message name: it routes `(charge point, action, raw payload)` to the
`ocpp.Handler` chosen by the negotiated WebSocket subprotocol. That is what lets OCPP 2.0.1
arrive later as a sibling of `v16` rather than as a rewrite.

Commands go the other way through `ocpp.ChargePoint`, which `csms.Server.Command(id)` hands
out: `ReserveNow`, `CancelReservation`, `RemoteStart`, `RemoteStop`, `UnlockConnector`,
`TriggerStatus` and `Capabilities`. Callers address EVSEs by their OCPI `uid` from config and
get back a version-agnostic status (`Accepted`, `Occupied`, `UnlockFailed`, ...) alongside the
station's literal answer. A station saying no is a result, not an error; errors are for the
call itself failing (`ocpp.ErrTimeout`, `ocpp.ErrNotConnected`, `ocpp.ErrUnknownEVSE`, or the
station's CALLERROR as `*ocpp.RPCError`). The exact 1.6 payloads are pinned by golden files in
`internal/ocpp/v16/testdata/golden`; regenerate them deliberately with
`go test ./internal/ocpp/v16 -run Golden -update`.

Nothing calls `ChargePoint` directly except `internal/control`. It is the single path for
"do something to the station": it validates the EVSE, allocates and persists the id, sends
the command, interprets the answer and records the outcome in core, and it returns typed
errors (`*control.RefusedError` for a station's no, `control.ErrAlreadyReserved`,
`control.ErrNoReservation`, `control.ErrNoActiveTransaction`, plus the `ocpp` sentinels above)
so the CLI can turn them into exit codes and OCPI into command results. A reservation becomes
active only once the station accepts it; on a refusal or a timeout it is dropped at once, so
nothing lingers that the station may not hold. Core still reports an EVSE as `RESERVED` only
when the station does: the reservation record sits next to the status, it does not overwrite it.

`ocppj` is direction-agnostic on purpose. A CSMS connection and a charge point connection
differ only in who performs the handshake, so both ends share one implementation of the
framing and correlation rather than keeping two that can drift apart.

The charge point identity is taken from the **last** path segment of the WebSocket URL, so
`/ocpp/<id>`, `/<id>` and `/steve/websocket/CentralSystemService/<id>` all work — vendors
disagree, and a charger that dials in and is silently rejected is miserable to debug on site.

## Development

```sh
make ci      # gofmt check, go vet, golangci-lint, go test -race
make test
make lint    # needs: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2
```

## Status

Built in tracked steps; each lands as its own PR.

| # | Scope | State |
|---|---|---|
| 1 | Project skeleton, config loader, CI | done |
| 2 | OCPP 1.6-J CSMS: WebSocket server and message routing | done |
| 3 | Built-in charge point simulator | done |
| 4 | Outbound OCPP commands, reservation lifecycle, one-shot CLI | in progress |
| 5 | OCPI foundation: versions + credentials handshake | |
| 6 | OCPI Locations module (sender) | |
| 7 | PATCH location on status change | |
| 8 | OCPI Commands module (receiver) | |
| 9 | Terminal UI | |
| 10 | OCPP 2.0.1 | |
| 11 | Docs and release builds | |

Out of scope for now: the OCPI Booking module, CDRs, sessions, tariffs, payment terminals,
multiple OCPI versions, and any web UI.

Ideas that are worth keeping but not planned, among them CDRs, an API-only mode and a fuller
terminal UI, live in the [idea fridge](docs/IDEAS.md).

## License

[MIT](LICENSE).

This is a personal hobby project. It is used as an internal test environment at Fryte,
but it is not an official Fryte product and Fryte does not maintain or support it.
