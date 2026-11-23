# Architecture

One Go process, `cpms run`, holds everything. The message-level view is in
[Sequences](sequences.md).

## The big picture

```mermaid
flowchart LR
    cli["One-shot commands<br/>cpms reserve / start / stop<br/>unlock / status"]
    partner["OCPI partner<br/>(eMSP backend, OCPI client)"]
    station["Charging station<br/>(Alpitronic, or<br/>cpms simulate charger)"]

    subgraph run ["cpms run (one process)"]
        sock["Control socket<br/>control/socket"]
        ocpi["OCPI server + push<br/>ocpi (versions, credentials,<br/>locations, push)"]
        ctrl["control<br/>the one way to act<br/>on the station"]
        csms["OCPP server<br/>ocpp/csms + ocpp/v16"]
        core["core<br/>state + event bus"]
        store[("state.json")]
        log["event log"]
    end

    cli -- "Unix socket" --> sock
    partner <-- "OCPI 2.3.0<br/>(requests in, status PATCHes out)" --> ocpi
    sock --> ctrl
    ocpi -.-> ctrl
    ctrl -- "commands" --> csms
    csms <-- "OCPP 1.6-J over WebSocket<br/>(the station dials in)" --> station
    ctrl -- "reservations, ids" --> core
    csms -- "station reports" --> core
    ocpi -- "reads status, subscribes to changes" --> core
    core <--> store
    core --> log
```

The dotted line is planned: OCPI commands, which will end in the same `control` layer as
the terminal.

## Four rules

| Rule | Meaning |
|---|---|
| **`core` is the single source of truth.** | It holds the station, every EVSE's status, reservations and sessions, and publishes an event for every change. The event log, the OCPI push and the TUI subscribe. Nothing else keeps charger state. |
| **`control` is the single way to act on the station.** | It validates the EVSE, allocates and persists ids, sends the command, interprets the answer and records the outcome in `core`. The socket and the TUI (and later OCPI) are thin transports in front of it. |
| **Protocol versions stay in their adapters.** | `ocpp/csms` routes raw messages by the negotiated subprotocol and never learns a message name. `ocpp/v16` translates between 1.6 messages and the version-agnostic `core`. OCPP 2.0.1 will be a sibling adapter, not a rewrite. |
| **`config.yaml` is read, `state.json` is written.** | See [Configuration](../reference/configuration.md#statejson). |

## Packages

All under [`internal/`](https://github.com/wolffseb/headless-csms/tree/main/internal).

| Package | Role |
|---|---|
| `core` | Domain state and the event bus |
| `control` | The one place that acts on the station: reserve, cancel, start, stop, unlock, probe |
| `control/socket` | The Unix socket the one-shot commands reach `cpms run` through |
| `ocpp` | Version-agnostic RPC errors, the `Handler` and `ChargePoint` interfaces, `ProbeReport` |
| `ocpp/ocppj` | The OCPP-J RPC layer: framing, correlation, connection goroutines |
| `ocpp/csms` | The WebSocket server a charge point dials into |
| `ocpp/v16` | OCPP 1.6-J payloads, the adapter that writes to `core`, and the command adapter |
| `ocpi` | The OCPI transport: HTTP server, envelope, token auth, module registry, versions |
| `ocpi/client` | Calls to the partner's OCPI API |
| `ocpi/credentials` | The credentials module and registration handshake |
| `ocpi/locations` | The Locations module (sender): config plus live status, paginated |
| `ocpi/push` | Pushes EVSE status changes to the partner's Locations receiver: dirty flag per EVSE, debounce, retries |
| `config` | Loads and validates `config.yaml` |
| `state` | `state.json`: the persisted DTOs and the atomic-write store |
| `simulator` | A charge point: dials a CSMS and behaves like a station |
| `cli` | The command tree |
| `ocpptest`, `statetest`, `clocktest` | Test helpers: a raw OCPP-J client, a store whose writes can fail, a manual clock |

## Design notes

### Inbound: `csms` never learns a message name

It routes `(charge point, action, raw payload)` to the `ocpp.Handler` chosen by the
negotiated WebSocket subprotocol. That is what lets OCPP 2.0.1 arrive as a sibling of `v16`.

### Outbound: one interface, one caller

Commands go through `ocpp.ChargePoint`, which `csms.Server.Command(id)` hands out:

| Group | Methods |
|---|---|
| Sessions | `ReserveNow`, `CancelReservation`, `RemoteStart`, `RemoteStop`, `UnlockConnector` |
| Station | `TriggerStatus`, `Capabilities`, `Probe` |
| Smart charging | `SetChargingProfile`, `ClearChargingProfile`, `GetCompositeSchedule` |

Callers address EVSEs by their OCPI `uid` and get back a version-agnostic status
(`Accepted`, `Occupied`, `UnlockFailed`, ...) alongside the station's literal answer.

Nothing calls `ChargePoint` directly except `internal/control`.

### A station saying no is a result, not an error

| Kind | Examples | Becomes |
|---|---|---|
| The station's answer | `Rejected`, `Occupied` | `*control.RefusedError` |
| cpms refusing | `control.ErrAlreadyReserved`, `ErrNoReservation`, `ErrNoActiveTransaction` | Exit 9 |
| The call failing | `ocpp.ErrTimeout`, `ocpp.ErrNotConnected`, `ocpp.ErrNotSent`, `ocpp.ErrUnknownEVSE`, `*ocpp.RPCError` | Exit 7, 3, 12, 1, ... |

Typed errors let the CLI turn them into [exit codes](../reference/cli.md#exit-codes), and
later OCPI into command results.

### Reservations

A reservation becomes active only once the station accepts it. On a refusal or a timeout
it is dropped at once, so nothing lingers that the station may not hold. `core` reports an
EVSE as `RESERVED` only when the station does.

### Charging profiles are validated before sending

Periods from 0 and strictly increasing, limits with at most one decimal place, only a `Tx`
profile takes a transaction, only `StationMax` may address the whole station. A bad one
fails with `ocpp.ErrInvalidProfile`.

### One CALL in flight per connection

`ocppj.Conn.Call` keeps one CALL in flight, as OCPP-J 1.6 §4.1.1 asks of a CSMS. A second
caller (two socket commands, or a command and the on-connect `TriggerMessage`) waits until
the first has an answer, a CALLERROR or a timeout.

| Outcome | Error | Exit |
|---|---|---|
| Never got its turn within `charger.call_timeout` | `ErrNotSent` | 12, safe to repeat |
| Sent and answered | | 0, 4-6, 8 or 11 |
| Sent, no answer within `charger.call_timeout` | `ErrTimeout` | 7, the station may have acted |
| The connection dropped | `ErrClosedUnsent` (queued) or `ErrConnClosed` (on the wire) | 3, either may be the case |

The call timeout starts once the frame is sent, so a call lives at most twice
`charger.call_timeout`. The CLI waits for `cpms run` for `2 × call_timeout + 5 s`, so the
daemon always answers first. Calls from the station are not affected.

### `ocppj` is direction-agnostic

A CSMS connection and a charge point connection differ only in who performs the handshake.
Both ends (server and simulator) share one implementation of framing and correlation.

### The identity is the last path segment

`/ocpp/<id>`, `/<id>` and `/steve/websocket/CentralSystemService/<id>` all work. Vendors
disagree, and a charger that is silently rejected is miserable to debug on site.
