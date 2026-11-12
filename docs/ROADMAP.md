# cli-cpms — a terminal Charge Point Management System

## Context

We have an OCPP-capable Alpitronic charging station on the office LAN and no way to drive it
without a full CPMS. We want a single self-contained binary that acts as a minimal CPMS:

- speaks **OCPP 1.6-J** (and later 2.0.1) to the station so we can reserve a connector, redeem
  the reservation with a known RFID tag, stop, and unlock — from a terminal;
- exposes a minimal **OCPI 2.3.0 CPO** interface on the LAN so the **Fryte backend** can register,
  pull static location data, and push `RESERVE_NOW` that ends up at the real charger;
- **PATCHes** the EVSE status to Fryte whenever it changes — whether the change came from our
  terminal or from someone physically plugging in a car;
- is configured by one hand-edited file and needs no database, no container, no web UI.

The value is a fast local loop for integration-testing the Fryte OCPI client against real charger
behaviour, plus a usable tool for driving the office station day to day.

## Decisions (settled)

| Topic | Decision |
|---|---|
| Language | **Go**. Single static binary, cross-compiles, no runtime on the office machine. Code kept deliberately plain and commented. |
| Process model | **One process**. `cpms run` starts OCPP CSMS + OCPI HTTP server + TUI. `--headless` for no-TTY. One-shot subcommands attach to a running instance over a unix socket. |
| OCPP direction | The charger is the WebSocket **client**; we are the **CSMS server**. The Alpitronic must be configured with `ws://<our-host>:9000/ocpp/<chargePointId>`. `charger.ip` in config is only used for a reachability pre-check and for display. |
| OCPP versions | **1.6-J first**, end-to-end and proven against the real station. 2.0.1 lands later as its own issue, behind a version-agnostic interface designed for two implementations from day one. |
| Unlock flow | Both commands ship. The demo flow is `ReserveNow(idTag)` → `RemoteStartTransaction(idTag)` (redeems the reservation and starts charging) → `RemoteStopTransaction`. `UnlockConnector` is a separate command for freeing a stuck cable. |
| OCPI scope | `versions`, `credentials`, `locations` (Sender), `commands` (Receiver). **No** Booking module, no CDRs, no sessions, no tariffs. Single version: 2.3.0. |
| OCPI auth | Full Token A/B/C credentials handshake, plain HTTP (LAN only). Tokens are base64-encoded in the `Authorization: Token …` header, per OCPI ≥2.2. |
| Testing | A **charge point simulator is built into the binary** (`cpms simulate charger`). Every issue gets automated acceptance tests that run in CI with no hardware. |
| State | `config.yaml` is **read-only** to the tool. `state.json` (tool-owned, atomic writes) holds what the tool cannot know upfront. |

### Token placement (from the state decision)

| Token | Who owns it | Lives in |
|---|---|---|
| `TOKEN_A` | us — handed to Fryte out-of-band to bootstrap | `config.yaml` |
| `TOKEN_C` | us — Fryte authenticates with it after registration | `config.yaml` (fixed value, not randomly generated) |
| `TOKEN_B` + Fryte's `versions` URL and endpoint list | Fryte — received during handshake | `state.json` |

`state.json` also holds: registration status, active reservations (id, connector, expiry), active
transaction ids, and last-known EVSE status, so a restart does not lose the Fryte pairing or
orphan a reservation.

## Architecture

```
cmd/cpms/main.go             cobra root: run | simulate | probe | config validate | version
internal/config              load + validate config.yaml (never written)
internal/state               state.json store, atomic write, single writer
internal/core                domain model, single source of truth, event bus
internal/ocpp                version-agnostic ChargePoint interface + shared types
internal/ocpp/csms           WS server, subprotocol negotiation, Call/CallResult/CallError routing
internal/ocpp/v16            OCPP 1.6-J messages + adapter
internal/ocpp/v201           OCPP 2.0.1 messages + adapter        (issue 10)
internal/ocpi                versions, credentials, locations (sender), commands (receiver)
internal/ocpi/push           debounced PATCH client → Fryte
internal/tui                 Bubble Tea model, panes, spinners
internal/simulator           charge point client used by tests and for demos
```

`core.Service` is the single source of truth (connected charge points, EVSE status, reservations,
transactions) and publishes `core.Event` on an in-process bus. Subscribers: the TUI (re-render),
the OCPI push client (debounced PATCH), the log pane. Nothing else holds mutable charger state.

The version-agnostic seam — the one thing that must be right before 2.0.1 exists:

```go
type ChargePoint interface {
    ID() string
    Version() Version
    ReserveNow(ctx context.Context, r ReserveRequest) (ReserveResult, error)
    CancelReservation(ctx context.Context, reservationID int) (Result, error)
    RemoteStart(ctx context.Context, r RemoteStartRequest) (Result, error)
    RemoteStop(ctx context.Context, transactionID string) (Result, error)
    UnlockConnector(ctx context.Context, evseUID string) (Result, error)
    TriggerStatus(ctx context.Context, evseUID string) error
}
```

Inbound messages are translated by the version adapter into `core.Event` — 1.6's
`StartTransaction`/`StopTransaction` and 2.0.1's `TransactionEvent` both collapse to the same
core transaction events.

### Identity mapping

OCPP 1.6 addresses an integer `connectorId`; OCPI addresses `location_id` / `evse_uid` /
`connector_id`. The mapping is explicit in config so nothing is guessed:

```yaml
charger:
  id: ALP-HYC-001            # must match the OCPP charge point identity in the WS path
  ip: 192.168.1.42           # reachability pre-check only
  ocpp_version: "1.6"
  heartbeat_timeout: 90s

server:
  ocpp_bind: 0.0.0.0:9000    # charger dials ws://<host>:9000/ocpp/ALP-HYC-001
  ocpi_bind: 0.0.0.0:8080

auth:
  default_id_tag: "04A1B2C3D4"    # the office RFID tag

ocpi:
  country_code: DE
  party_id: FRY
  public_base_url: http://192.168.1.10:8080/ocpi   # what we advertise to Fryte
  token_a: "…"     # we hand this to Fryte out-of-band
  token_c: "…"     # Fryte uses this after registration
  push:
    debounce: 500ms
    max_retries: 5

location:                    # static OCPI Location data, verbatim from config
  id: OFFICE-01
  name: Fryte HQ
  address: "…"
  city: "…"
  country: DEU
  coordinates: { latitude: "52.520008", longitude: "13.404954" }
  evses:
    - uid: ALP-HYC-001-1
      evse_id: DE*FRY*E001*1
      ocpp_connector_id: 1
      connectors:
        - id: "1"
          standard: IEC_62196_T2_COMBO
          format: CABLE
          power_type: DC
          max_voltage: 920
          max_amperage: 500
          max_electric_power: 400000
```

### Status mapping

OCPP 1.6 `ChargePointStatus` → OCPI 2.3 `Status`, in one table with its own unit test:

| OCPP 1.6 | OCPI |
|---|---|
| Available, Preparing | `AVAILABLE` |
| Finishing | `BLOCKED` |
| Charging, SuspendedEV, SuspendedEVSE | `CHARGING` |
| Reserved | `RESERVED` |
| Unavailable | `INOPERATIVE` |
| Faulted | `OUTOFORDER` |
| (no connection / unknown) | `UNKNOWN` |

### OCPI command semantics

OCPI commands are asynchronous and this drives the handler design: the POST returns
`CommandResponse{result: ACCEPTED, timeout: N}` immediately, and the real outcome is POSTed later
as a `CommandResult` to the caller's `response_url`. Tests must assert both halves.

## Issues → PRs

Dependency order: 1 → 2 → 3 → 4 → {5 → 6 → 7, 8} → 9 → 10 → 11.
Milestones: after **4** the terminal→charger loop works; after **8** the Fryte→charger loop works;
after **9** it is pleasant to use; after **10** it speaks 2.0.1.

---

### Issue 1 — Project skeleton, config loader, CI

Go module, `cobra` root with `version` and `config validate`, `internal/config` with strict
validation (unknown YAML keys rejected), `config.example.yaml`, Makefile, `golangci-lint`,
GitHub Actions running `go vet` + `go test -race ./...` on linux and macOS.

**Acceptance**
- `go build ./...` and `go test -race ./...` green in CI on both OSes.
- `cpms config validate -c testdata/valid.yaml` exits 0 and prints a summary (charger id, bind
  addresses, EVSE count).
- Table test: each of ~10 invalid fixtures (missing `charger.id`, bad duration, duplicate
  `ocpp_connector_id`, `evse_uid` referenced twice, malformed coordinates, unknown key) exits 1
  with a message naming the offending field path.
- `config.example.yaml` itself passes validation — asserted by a test, so docs cannot rot.

### Issue 2 — OCPP 1.6-J CSMS: WebSocket server and message routing

WS server on `server.ocpp_bind`, path `/ocpp/{chargePointId}`, subprotocol negotiation for
`ocpp1.6`. Call/CallResult/CallError framing with message-id correlation, per-charge-point send
queue, request timeouts. Inbound handlers: `BootNotification`, `Heartbeat`, `StatusNotification`,
`Authorize`, `StartTransaction`, `StopTransaction`, `MeterValues`, `DataTransfer` (rejected).
`core.Service` registry and event bus.

**Acceptance**
- Unit tests for framing: valid Call round-trip; unknown action → `CallError` with
  `NotImplemented`; malformed JSON → `CallError` with `RpcFrameworkError`; wrong message-id in a
  CallResult is dropped without crashing; concurrent calls to one CP are correlated correctly.
- Integration test with a raw WS client on a dynamic port: connection with subprotocol `ocpp1.6`
  is accepted, one without is rejected (HTTP 400); `BootNotification` gets
  `{status: Accepted, interval, currentTime}`.
- `StatusNotification(connectorId=1, status=Charging)` is observable as `AVAILABLE→CHARGING` on
  `core.Service` and emits exactly one event.
- No heartbeat for `heartbeat_timeout` marks the CP offline and emits an event; reconnection
  restores it.
- `go test -race` clean with 20 simulated concurrent charge points.

### Issue 3 — Built-in charge point simulator

`cpms simulate charger --csms ws://127.0.0.1:9000 --id ALP-HYC-001 --ocpp 1.6 --connectors 2`.
Sends BootNotification, periodic Heartbeats, StatusNotification on every state change; handles
`ReserveNow`, `CancelReservation`, `RemoteStartTransaction`, `RemoteStopTransaction`,
`UnlockConnector`, `TriggerMessage`, `GetConfiguration`. Deterministic scenarios
(`--scenario reject-reserve`, `--scenario occupied`, `--scenario slow`) for negative-path tests.
Exposed as a Go package so tests drive it in-process.

**Acceptance**
- In-process test: simulator connects to a test CSMS; within 2s `core.Service` reports the CP
  connected with N connectors `AVAILABLE`.
- `--scenario reject-reserve` makes `ReserveNow` return `Rejected`; `--scenario occupied` returns
  `Occupied`; `--scenario slow` delays responses past the request timeout.
- Manual two-terminal run documented in the README and verified: `cpms run` in one, `cpms simulate
  charger` in the other, charger appears in the CSMS.

### Issue 4 — Outbound OCPP commands, reservation lifecycle, one-shot CLI

Implement `ReserveNow`, `CancelReservation`, `RemoteStartTransaction`, `RemoteStopTransaction`,
`UnlockConnector`, `TriggerMessage`, `GetConfiguration` on the `ChargePoint` interface. Reservation
lifecycle in core: pending → accepted → expiry timer → cleared on transaction start or cancel,
persisted to `state.json`. One-shot subcommands over the unix socket (`cpms reserve --evse … [--tag
…]`, `cpms start`, `cpms stop`, `cpms unlock`, `cpms status`). Plus `cpms probe`, which runs
`GetConfiguration` and reports `SupportedFeatureProfiles` — this is how we find out early whether
the Alpitronic actually supports the Reservation profile.

**Acceptance**
- Golden-file tests: the JSON emitted for each outbound message matches the OCPP 1.6-J schema
  (field names, casing, ISO-8601 UTC `expiryDate`).
- E2E against the simulator: `cpms reserve --evse ALP-HYC-001-1` → connector reports `RESERVED`
  in `core` and `cpms status` shows the reservation with its expiry.
- E2E: `cpms start --evse ALP-HYC-001-1` with the configured `default_id_tag` starts a transaction,
  the reservation is cleared, status becomes `CHARGING`; `cpms stop` ends it.
- Reservation expiry fires without a charger message and clears core state.
- `Rejected` / `Occupied` / `Faulted` responses surface as distinct non-zero CLI exit codes with
  readable messages; a timeout surfaces as such and does not leak a goroutine (`-race`, leak check).
- `state.json` survives a restart: reserve, kill the process, restart, `cpms status` still shows
  the reservation.

### Issue 5 — OCPI foundation: versions + credentials handshake

`GET /ocpi/versions`, `GET /ocpi/2.3.0` (endpoint list), and the `credentials` module
(GET/POST/PUT/DELETE). Shared OCPI response envelope (`status_code`, `status_message`,
`timestamp`), the standard error codes, and `Authorization: Token <base64>` middleware.
`TOKEN_A`/`TOKEN_C` read from config; Fryte's `TOKEN_B` and endpoint list written to `state.json`.

**Acceptance** (all via `httptest`, no network)
- Unauthenticated request → HTTP 401 with OCPI `2001`.
- `GET /ocpi/versions` with `TOKEN_A` lists exactly one version, `2.3.0`, with a URL derived from
  `ocpi.public_base_url`.
- `POST /credentials` with `TOKEN_A` → `1000`, body contains our URL and `TOKEN_C`; `state.json`
  now holds Fryte's token and fetched endpoints; the tool has called Fryte's versions endpoint
  during the handshake (asserted against a fake eMSP server).
- Second `POST /credentials` → `2002` (already registered). `PUT` re-registers. `DELETE`
  unregisters and clears the tokens from `state.json`.
- After registration, `TOKEN_A` is rejected and `TOKEN_C` is accepted.
- Restart with the same `state.json`: registration is still in effect, `TOKEN_C` still works.
- Base64 header handling is tested both ways (encoded and — rejected — raw).

### Issue 6 — OCPI Locations module (Sender / CPO)

`GET /ocpi/2.3.0/locations` plus `/{location_id}`, `/{location_id}/{evse_uid}`,
`/{location_id}/{evse_uid}/{connector_id}`. Locations are assembled from the static `location:`
config block plus live status from `core`. Pagination via `Link`, `X-Total-Count`, `X-Limit`;
`date_from` / `date_to` filters.

**Acceptance**
- Golden-file test of the full Location JSON, asserted field-by-field against the OCPI 2.3.0
  schema; every `last_updated` is RFC3339 in UTC.
- The `evse.status` in the response changes after the simulator sends a StatusNotification —
  asserted in one test that drives sim → CSMS → HTTP.
- Sub-object endpoints return the correct nested object; unknown id → `2003`.
- Pagination headers correct for `?offset=1&limit=1` with two EVSEs configured.
- Requests without a valid token → `2001`.

### Issue 7 — Push: PATCH location on every status change

Subscriber on the core event bus that PATCHes
`{fryte_endpoint}/locations/{country_code}/{party_id}/{location_id}/{evse_uid}` with
`{status, last_updated}`. Per-EVSE ordered queue, configurable debounce (default 500ms), retry with
exponential backoff up to `max_retries`, failures logged and surfaced in the TUI but never fatal.
No-ops cleanly when Fryte is not registered.

**Acceptance**
- Fake eMSP receiver (`httptest`): simulator flips connector `Available→Charging`; exactly one
  PATCH arrives within 1s with the correct path, `Authorization: Token <base64 TOKEN_B>`, and body.
- Rapid flapping (5 changes in 100ms) coalesces into one PATCH carrying the final status.
- A CLI-triggered `cpms reserve` produces a PATCH with `RESERVED` — proving both change sources
  (terminal and charger) reach Fryte.
- Receiver returning 500 → retried with backoff, then dropped with a logged error; the process
  stays healthy and later changes still push.
- Not registered → no HTTP calls, no errors, no goroutine growth.

### Issue 8 — OCPI Commands module (Receiver / CPO)

`POST /commands/{RESERVE_NOW,CANCEL_RESERVATION,START_SESSION,STOP_SESSION,UNLOCK_CONNECTOR}`.
Each returns `CommandResponse{ACCEPTED, timeout}` synchronously, then POSTs a `CommandResult` to
the request's `response_url` once the charger answers. Maps the OCPI `Token` to an OCPP `idTag`
and `location_id`/`evse_uid` to the OCPP connector id via config.

**Acceptance**
- E2E, the headline test: `POST /commands/RESERVE_NOW` → HTTP 200 `ACCEPTED` immediately →
  simulator connector goes `RESERVED` → `CommandResult{ACCEPTED}` arrives at the fake sender's
  `response_url` → a PATCH with `RESERVED` arrives at the fake eMSP. One test, all four assertions.
- Simulator scenario `reject-reserve` → `CommandResult{REJECTED}`.
- Unknown `evse_uid`, or charger offline → synchronous `CommandResponse{REJECTED}`, no async result.
- Charger silent past the timeout → `CommandResult{TIMEOUT}` and the reservation is not left
  dangling in core.
- `START_SESSION` with a token maps to `RemoteStartTransaction`; `UNLOCK_CONNECTOR` maps to
  `UnlockConnector`; `CANCEL_RESERVATION` clears the reservation in core.
- `response_url` unreachable → logged, retried, and it does not block further commands.

### Issue 9 — Terminal UI

Bubble Tea + Lipgloss + Bubbles. Layout: header (charger id, OCPP version, connection state,
uptime, OCPI registration state), an arrow-key-selectable EVSE list with live status, an action
menu (reserve / cancel / start / stop / unlock / trigger status), and a scrolling event log.
A spinner runs while an OCPP call is in flight, since replies are asynchronous. `--headless`
skips the UI entirely and logs to stdout.

**Acceptance**
- `teatest` golden-frame tests: initial render; render after a status event; render with a
  reservation active.
- ↑/↓ moves the EVSE selection; `r` on the selected EVSE dispatches a reserve; `q` quits cleanly,
  closing the WS server and HTTP listener (asserted: ports free afterwards).
- The spinner is present while a call is pending and gone once the result or timeout lands.
- `cpms run --headless` starts, serves, and exits cleanly with no TTY (runs in CI).
- Terminal narrower than 60 columns degrades to a single-column layout instead of corrupting.

### Issue 10 — OCPP 2.0.1

`internal/ocpp/v201` implementing the same `ChargePoint` interface: `BootNotification`,
`Heartbeat`, `StatusNotification`, `Authorize`, `TransactionEvent` inbound; `ReserveNow`,
`RequestStartTransaction`, `RequestStopTransaction`, `UnlockConnector`, `TriggerMessage` outbound.
Subprotocol negotiation selects the adapter (`ocpp2.0.1` vs `ocpp1.6`). Simulator gains
`--ocpp 2.0.1`. Config gains the EVSE-id/connector-id pair 2.0.1 needs.

**Acceptance**
- The issue 3/4/8 E2E suites are parameterised over `{1.6, 2.0.1}` and green for both.
- A 2.0.1 charge point and a 1.6 charge point can be connected to the same running instance
  simultaneously and both appear correctly.
- `TransactionEvent{Started/Updated/Ended}` maps to the same core transaction events as 1.6's
  `StartTransaction`/`StopTransaction` — asserted by comparing the emitted event streams.
- OCPI output is byte-identical for equivalent states regardless of OCPP version.

### Issue 11 — Docs and release

README with a quickstart, a config reference, and a runbook for pointing the real Alpitronic at
the tool. `goreleaser` build matrix.

**Acceptance**
- `make release-snapshot` produces linux/darwin/windows × amd64/arm64 binaries.
- A clean checkout following only the README quickstart reaches a working simulator demo —
  verified by a scripted smoke test in CI that executes the documented commands.

## Verification (end to end, no hardware)

Three terminals:

```
cpms run -c config.yaml                                  # CSMS + OCPI + TUI
cpms simulate charger --csms ws://127.0.0.1:9000 --id ALP-HYC-001
# in a third: drive it as Fryte would
curl -H "Authorization: Token $(printf 'TOKEN_A' | base64)" http://127.0.0.1:8080/ocpi/versions
curl -X POST -H "Authorization: Token …" -d @creds.json http://127.0.0.1:8080/ocpi/2.3.0/credentials
curl -X POST -H "Authorization: Token …" -d @reserve.json \
     http://127.0.0.1:8080/ocpi/2.3.0/commands/RESERVE_NOW
```

Expected: the TUI shows the connector flip to `RESERVED`, the simulator logs the `ReserveNow`, the
`CommandResult` lands on the `response_url`, and a `PATCH …/locations/DE/FRY/OFFICE-01/…` reaches
the registered receiver. The same sequence runs unattended as `go test ./internal/e2e -race`.

Against the real charger: point its CSMS URL at `ws://<host>:9000/ocpp/<id>`, run `cpms probe`
first to confirm the Reservation feature profile, then repeat the flow with `cpms simulate` swapped
for the station.

## Open items to confirm against the real station

These do not block starting — issues 1–3 are hardware-independent — but they should be checked
before issue 4 is closed:

1. **Does the Alpitronic accept `ReserveNow` at all?** Some units omit the Reservation feature
   profile. `cpms probe` (issue 4) answers this. If it is missing, the OCPI `RESERVE_NOW` path has
   to degrade to a locally-held soft reservation, which is a design change worth knowing early.
2. **Which OCPP version is the station currently provisioned for**, and can we change the CSMS URL
   ourselves or does Alpitronic service have to do it?
3. **Connector numbering** — how many EVSEs/connectors it reports and which `connectorId` maps to
   which physical outlet.
4. **RFID tag behaviour** — whether the station will `Authorize` our tag against us, or has a local
   whitelist that would shortcut the flow.
