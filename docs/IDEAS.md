# Idea fridge

Ideas that are worth keeping but are **not planned**. Nothing here is scheduled, and no agent
should pick anything up from this file. The planned work is tracked in issue #5 and its
sub-issues.

An idea leaves the fridge when it gets written up as issues in the same shape as #6–#21:
self-contained, with scope, out of scope, and testable acceptance criteria. At that point,
delete the entry here and link the tracking issue instead.

Each entry answers the same questions:

- **What:** the idea in one or two sentences.
- **Why:** what it would be good for.
- **Starting point:** what already exists to build on.
- **Open questions:** what needs deciding before it can be planned.
- **Needs first:** the roadmap steps it builds on.
- **Size:** a rough feel, not an estimate.
- **Go you'd learn:** since learning Go is half the point of this project.

---

## 1. A real terminal UI

**What.** A full-screen console for power users, beyond the minimal operator view in #17.
Think k9s for a charging station.

**Why.** The most useful thing about this tool as a test environment is *seeing what the
charger actually does*. #17 shows state. This would show traffic and history:

- **OCPP frame inspector:** every CALL, CALLRESULT and CALLERROR in both directions, with
  filtering by action, pretty-printed payloads, and timing. This is the view that answers
  "why did the Alpitronic do that?" on site.
- **OCPI traffic log:** every request Fryte sends us and every PATCH or CommandResult we
  send them, with status codes.
- **Meter values** as a sparkline per active session.
- **Prompts for custom input:** reserve with a different tag or expiry, start with any tag.
- **Simulator control from the UI:** switch scenarios, flip connector states, pull the
  cable, all without a second terminal.
- **Several stations side by side**, once there is more than one (see idea 2).

**Starting point.** #17 builds the Bubble Tea skeleton, and `core`'s event bus already feeds
everything it would show except raw frames. Raw frames would need a tap in
`internal/ocpp/ocppj` (one hook where frames are read and written).

**Open questions.**
- Does it stay in-process in `cpms run`, or become a **client of the API from idea 2**,
  the way k9s is a client of the Kubernetes API? The client model is cleaner: the daemon
  runs headless on the office machine, and the UI attaches from any laptop. It also means
  idea 2 comes first.
- How much frame history is kept, and where: in memory with a ring buffer, or in the store
  from idea 3?

**Needs first.** #17. Ideally idea 2.

**Size.** Medium, most of it UI work. The frame tap is small.

**Go you'd learn.** Elm-style state machines (Bubble Tea), channel fan-out, ring buffers,
and rendering under concurrency without data races.

---

## 2. An API-only CSMS

**What.** Run cpms as a headless daemon with a documented, versioned **management API**:
REST for commands and queries, plus an event stream (SSE or WebSocket) for live status.
The CLI, the TUI and any script all become clients of it.

**Why.**
- **Test automation for the Fryte backend:** a CI job starts cpms plus the simulator,
  drives scenarios through the API, and asserts on what Fryte's backend did. That turns
  this from a manual test environment into an automated one.
- It separates "the thing that talks to chargers" from "the thing a human looks at", which
  is the architecture most real CSMSs end up with.
- It is the natural home for running on a server rather than a laptop.

**Starting point.** More than it looks:
- `internal/control` (#8) is already the single place every action goes through;
- the unix socket (#9) is already a small internal API with a request/response format and
  error codes;
- `core.Subscribe` is already an event stream.

This idea mostly means putting HTTP in front of those, writing an OpenAPI spec, and adding
authentication.

**Open questions.**
- **Is the management API separate from OCPI?** It should be: OCPI is a roaming protocol
  with its own rules, and bending it into an admin API ends badly. Two listeners, or two
  path prefixes on one listener.
- **Multiple chargers.** Today the config describes exactly one station, by design. An
  API-first daemon probably wants a charger registry. That collides with the principle that
  `config.yaml` is read-only and hand-edited, so chargers would be added through the API
  and stored by idea 3. This is the biggest design question in the fridge.
- Authentication: a static bearer token from config is enough for a LAN, but mTLS if it
  ever leaves the LAN.
- Packaging: a container image becomes worth having at this point (the roadmap has none).

**Needs first.** #9, and #16 so the OCPI side is stable. Multi-charger support needs idea 3.

**Size.** Small for single-charger (a thin HTTP layer over `control`). Large with
multi-charger, because `run`, config, core and OCPI Locations all assume one station today.

**Go you'd learn.** `net/http` in depth (middleware, graceful shutdown, streaming responses),
API versioning, OpenAPI generation (e.g. `oapi-codegen`), and context propagation across
request boundaries.

---

## 3. Real data storage, enough for CDRs

**What.** Replace or extend `state.json` with a real store, then use it to record sessions
and meter values and produce **CDRs** (charge detail records). Eventually serve them over
OCPI's `sessions` and `cdrs` modules, which are out of scope today.

**Why.** A CSMS that can't tell you what was charged is only half a CSMS. For the Fryte test
environment, it means the backend's CDR ingestion could be tested against a real station's
numbers, not hand-written fixtures.

**Starting point.** `state.json` (#6) deliberately holds **active** state only. It has no
history. Everything needed to build history already flows through the `core` event bus:
transaction started and stopped, meter values, and status changes.

**Storage options**, roughly from recommended to least:

| Option | For | Against |
|---|---|---|
| **SQLite via `modernc.org/sqlite`** | One file, SQL, transactions, ad-hoc queries with the `sqlite3` CLI. **Pure Go**, so the `CGO_ENABLED=0` release builds from #21 keep working. | Some care needed with concurrent writers (WAL mode, one writer goroutine). |
| SQLite via `mattn/go-sqlite3` | The most widely used Go driver. | **Needs CGO**, which breaks the static cross-compiled builds. Rules it out here. |
| Append-only event log (JSONL) plus projections | Every `core.Event` goes to disk, and CDRs are *derived* from it. Replayable, auditable, and very little code. A lovely way to learn event sourcing. | Queries mean replaying or keeping projections. It gets awkward once there is a lot of history. |
| `bbolt` (embedded key/value) | Pure Go, simple, fast. | No SQL. Every query is hand-written iteration. |
| PostgreSQL | The right answer for a multi-user server deployment (idea 2 at scale). | A second process to run, which is against the "one binary" principle for a LAN tool. |

**Recommendation if it gets picked up:** SQLite on the pure-Go driver, **replacing**
`state.json` rather than living beside it, so there is still one store. Include a one-time
import of an existing `state.json`, and schema migrations embedded in the binary
(`embed` plus a small migration runner, or `goose`). Consider also keeping the raw event log
as a table: it costs little and doubles as the frame history idea 1 wants.

**Open questions.**
- **What goes into a CDR without tariffs?** OCPI requires a `total_cost`. Options are a flat
  configured tariff (price per kWh), zero, or implementing the OCPI `tariffs` module too.
  A flat tariff from config is the smallest honest answer.
- **Meter data quality.** OCPP 1.6 gives `meterStart`/`meterStop` in Wh for sure. Periodic
  values depend on the station's `MeterValuesSampledData` configuration, which `cpms probe`
  (#10) already reports. OCPI `charging_periods` want more than start and stop.
- **Eichrecht (German calibration law).** Alpitronic stations can sign meter values
  (OCMF). That only matters if CDRs are ever used for real billing, but if they are, the
  signed data has to be stored and passed through unchanged. Worth knowing before designing
  the schema.
- **Immutability.** A CDR that has been sent must never change. Corrections are separate
  credit CDRs. The schema should make editing a sent CDR impossible, not just discouraged.
- **Retention:** how long history is kept, and whether old rows are pruned.

**Needs first.** #8 (the transaction lifecycle), #16 (sessions have to exist before CDRs
mean anything). For the 2.0.1 transaction model, #18.

**Size.** Medium for storage and CDR generation. Large with the OCPI `sessions`, `cdrs` and
`tariffs` modules, because each is a module the size of Locations or Commands.

**Go you'd learn.** `database/sql`, transactions, migrations, `embed`, writing SQL-backed
repositories with tests (an in-memory SQLite per test is fast), and money arithmetic
without floats.

---

## Also parked

Smaller things that came up while planning and were deliberately left out. Each links to
where it was decided.

- **Cancel orphaned reservations on reconnect.** If a ReserveNow fails because the connection
  dropped, the CancelReservation in `control` cannot reach the station, which keeps the
  reservation until it expires and answers Occupied meanwhile. Fix: persist the orphan ids
  in `state.json` and send the cancel when the station next connects. Left out of #26 and
  its follow-ups, see the review on #29.
- **Soft reservations.** Only for stations without the Reservation profile, such as the
  third-party rey: OCPI `RESERVE_NOW` would need a reservation held in cpms that refuses
  other tags at start. The office Alpitronic is known to support ReserveNow (tested with
  another CSMS, chargecloud), so it does not need this; the hardware check, #11, confirms
  it against cpms.
- **OCPI Sessions module**, minimal, so Fryte can learn the `session_id` it needs for
  `STOP_SESSION`. See the decision in #16. It overlaps with idea 3.
- **Recording and replaying OCPP traffic.** Record a real Alpitronic session to a file, and
  replay it through the simulator later: a regression test built from real hardware
  behaviour. It shares the frame tap with idea 1.
- **`TOKEN_C` rotation** as the OCPI spec intends on `PUT /credentials`. See the decision
  in #13.
- **TLS and OCPP security profiles** (WSS, basic auth, client certificates). Only relevant
  if the tool leaves the LAN.
- **Publishing releases** on tags. See the decision in #21.
