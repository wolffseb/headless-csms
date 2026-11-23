# Take commands from the partner

Once a partner is [registered](ocpi-partner.md), it can reserve an EVSE, start and stop a
session, and unlock a cable over OCPI. cpms passes each command to the station through the
same `control` layer the terminal uses, so a command from the partner and `cpms reserve`
do the same thing.

OCPI commands are **asynchronous**, and that shapes everything below:

1. The `POST` is answered **at once** with a `CommandResponse`: "I can pass this on"
   (`ACCEPTED`, with a `timeout`), or a refusal that needs no station.
2. What the station did is `POST`ed later, as a `CommandResult`, to the `response_url` of the
   request.

```mermaid
sequenceDiagram
    autonumber
    participant P as Partner
    participant C as cpms run
    participant St as Station

    P->>C: POST /ocpi/2.3.0/commands/RESERVE_NOW<br/>{ response_url, token, expiry_date, ... }
    C-->>P: 200, 1000: { result: ACCEPTED, timeout: 65 }
    C->>St: ReserveNow
    St-->>C: Accepted
    C->>P: POST response_url: { result: ACCEPTED }
    St->>C: StatusNotification Reserved
    C->>P: PATCH .../locations/.../{evse_uid} { status: RESERVED }
```

Spec: [Commands module](https://github.com/ocpi/ocpi/blob/release-2.3.0-bugfixes/mod_commands.asciidoc).

## Send a command

`POST {ocpi.public_base_url}/2.3.0/commands/{command}`, with the partner's `TOKEN_C`
([how to build the header](../reference/ocpi-api.md#authentication)). `reserve.json`:

```json
{
  "response_url": "https://emsp.example/ocpi/commands/RESERVE_NOW/9a41",
  "token": { "uid": "NL-ABC-C00012345", "type": "RFID", "contract_id": "NL-ABC-C00012345" },
  "expiry_date": "2099-12-31T23:00:00Z",
  "reservation_id": "RES-0001",
  "location_id": "SITE-01",
  "evse_uid": "ALP-HYC-001-1"
}
```

The `expiry_date` is a placeholder far in the future, which cpms accepts but which holds the
EVSE for a very long time. Put a real one in, at least a minute ahead (anything sooner is
`REJECTED`), for example 30 minutes from now:

```sh
date -u -v+30M +%Y-%m-%dT%H:%M:%SZ        # macOS
date -u -d '+30 minutes' +%Y-%m-%dT%H:%M:%SZ   # Linux
```

```sh
TOKEN_C=change-me-token-c   # ocpi.token_c from config.yaml
auth="Authorization: Token $(printf '%s' "$TOKEN_C" | base64 | tr -d '\n')"
curl -s -H "$auth" -H 'Content-Type: application/json' -d @reserve.json \
  http://127.0.0.1:8080/ocpi/2.3.0/commands/RESERVE_NOW
```

```json
{
  "data": { "result": "ACCEPTED", "timeout": 65 },
  "status_code": 1000,
  "status_message": "Success",
  "timestamp": "2026-11-23T14:58:12Z"
}
```

`timeout` is how long the partner should wait for the `CommandResult`: the longest the
command can take (twice the charger's `call_timeout`, see
[one call in flight](../architecture/overview.md#one-call-in-flight-per-connection))
plus 5 s, using the `call_timeout` of the charger that owns the EVSE.

## The commands

| Command | Does | Notes |
|---|---|---|
| `RESERVE_NOW` | `ReserveNow` | The token's `uid` is the OCPP `idTag`. `expiry_date` must be at least a minute ahead. Without `evse_uid`, the first `AVAILABLE` reservable EVSE of the location is taken. |
| `CANCEL_RESERVATION` | `CancelReservation` | Finds the reservation by `reservation_id`. If its `RESERVE_NOW` is still with the station, cpms waits for the answer and then cancels it. |
| `START_SESSION` | `RemoteStartTransaction` | The token's `uid` is the `idTag`. Without `evse_uid`, the first `AVAILABLE` EVSE is taken. |
| `STOP_SESSION` | `RemoteStopTransaction` | `session_id` is [our transaction id](#stop_session-and-the-session_id). |
| `UNLOCK_CONNECTOR` | `UnlockConnector` | `evse_uid` and `connector_id` are required. |

Ids (`location_id`, `evse_uid`, `reservation_id`, ...) are `CiString`s, so case does not
matter. "The first available EVSE" is searched across the **whole location**, whatever
charger an EVSE is on.

### Answered at once

These need no station, so they are answered in the `CommandResponse` and **nothing** is
sent to the station or posted to `response_url`:

| Condition | HTTP | `status_code` | `result` |
|---|---|---|---|
| Body is not valid JSON, or lacks a field the command needs, or `response_url` is not an `http(s)` URL | 400 | `2001` | |
| `response_url` is not on a host of the registered partner (see below) | 400 | `2001` | |
| `location_id` is not ours | 200 | `2003` | `REJECTED` |
| `evse_uid` or `connector_id` is unknown | 200 | `1000` | `REJECTED` |
| The charger that owns the EVSE is not connected | 200 | `1000` | `REJECTED`, "charge point offline" |
| `token.uid` is longer than 20 characters, or not printable ASCII (it has to be an OCPP 1.6 `idTag`) | 200 | `1000` | `REJECTED` |
| `RESERVE_NOW` with an `expiry_date` less than a minute ahead | 200 | `1000` | `REJECTED` |
| `RESERVE_NOW` for an EVSE with `reservable: false` | 200 | `1000` | `NOT_SUPPORTED` |
| `RESERVE_NOW` for an EVSE that holds another reservation, or with no `AVAILABLE` EVSE to pick | 200 | `1000` | `REJECTED` |
| `STOP_SESSION` for a `session_id` that is not an active transaction | 200 | `1000` | `UNKNOWN_SESSION` |
| A command type cpms does not know (the type is an open enum) | 200 | `1000` | `NOT_SUPPORTED` |

A response that is not `ACCEPTED` has `timeout: 0`: no result is coming.

!!! note "`START_SESSION` without `evse_uid`"
    The spec makes `evse_uid` optional here, and so does cpms: it picks the first
    `AVAILABLE` EVSE. Name the EVSE if the order of `location.evses` is not what you want.

!!! note "`START_SESSION` on an EVSE reserved for another tag"
    cpms does not check reservations before it passes a `START_SESSION` on, so naming an EVSE
    that is reserved for another tag is answered `ACCEPTED` like any other. The station then
    decides: a 1.6 station refuses a remote start for another tag on a reserved connector, and
    the `CommandResult` is `REJECTED`. The reserved tag itself can start. Without an `evse_uid`
    a reserved EVSE is never picked, because it does not read `AVAILABLE`.

### The result

Everything else is `ACCEPTED`, and the command then runs in the background. Its outcome is
`POST`ed to `response_url` as `{ "result": "...", "message": [{ "language": "en", "text": "..." }] }`:

| What happened | `result` |
|---|---|
| The station accepted | `ACCEPTED` |
| `Rejected` | `REJECTED` |
| `Occupied`, or cpms already holds a reservation on the EVSE | `EVSE_OCCUPIED` |
| `Faulted` or `Unavailable` | `EVSE_INOPERATIVE` |
| `UnlockFailed` | `FAILED` |
| `NotSupported`, `NotImplemented`, or a CALLERROR saying so | `NOT_SUPPORTED` |
| No answer within `charger.call_timeout` | `TIMEOUT` |
| `CANCEL_RESERVATION` for a `reservation_id` cpms does not hold | `UNKNOWN_RESERVATION` |
| Anything else: the connection dropped, the station was too busy to be sent the command | `FAILED` |

The `message` is cpms's own wording of what happened.

### Delivery

A `CommandResult` the partner does not take is sent again after 1 s, 2 s, 4 s ... (at most
30 s), up to `ocpi.push.max_retries` retries. The same rules as for the
[status push](ocpi-push.md#retries) decide what is retried: a `5xx`, `408`, `429`, or no
answer is, any other `4xx` is not. Then it is logged as an error and dropped.

Each result is delivered on its own, so a partner that is slow to take one never holds up
the next command. A result still waiting when cpms stops is lost.

!!! warning "TOKEN_B goes to the `response_url`"
    cpms authenticates the `CommandResult` with the partner's `TOKEN_B`, as OCPI has it. It
    therefore accepts a command only if its `response_url` has the same scheme and host
    (with port) as the partner's `versions` URL or one of its endpoints, as the partner gave
    them when it registered. Any other URL is answered at once with HTTP 400 and `2001`;
    nothing is sent to the station and nothing is posted.

    The check is needed because `TOKEN_C` is the same for every registration: a former
    partner that still holds it could otherwise send a command naming its own URL and be
    sent the current partner's `TOKEN_B`. **Rotate `ocpi.token_c` whenever the partner
    changes**, so that the former one is locked out altogether (see
    [OCPI partner](ocpi-partner.md)).

A `response_url` belongs to the registration its command arrived under. If that partner is
unregistered, and another registers (or the same one registers again from the start), a
result still to be sent, and the `CANCELED_RESERVATION` of a reservation made before, are
**dropped** and logged: they would otherwise go to the former partner's URL with the new
partner's `TOKEN_B`. A credentials `PUT`, which only rotates `TOKEN_B`, keeps the
registration, and results go on with the new token.

## Replace a reservation

A `RESERVE_NOW` with a `reservation_id` cpms already holds **replaces** that reservation:
there is still one reservation, under the same OCPP reservation id, with the new request's
tag, expiry and `response_url`. A 1.6 `ReserveNow` with an id the station holds replaces
it there too. The replacement must be for the same EVSE (or name none); moving a
reservation is refused at once.

If the replacement fails, cpms does **not** send `CancelReservation`, which would cancel the
original at the station too, and the failure is the result of the new command. What happens
to the original depends on what cpms knows:

- The station never saw the replacement (it was busy) or answered no: it still holds the
  original, so cpms keeps it, with its tag and expiry.
- The outcome is unknown (the station did not answer, or the connection went while the
  request was out): the station may hold either, so the original ends in cpms. The tags of
  both stay accepted at the station until their own expiry, so whichever it holds can still
  be redeemed.

## When cpms cancels a reservation

A reservation made over OCPI that **ends by the CPO's doing** is reported to its
`response_url` as `CANCELED_RESERVATION`: the spec's
"reservation canceled by the CPO" flow. That is the case when

- it was cancelled with `cpms cancel` (or the TUI later), and
- another tag started a session on the reserved EVSE, so the reservation failed.

Nothing is sent when it **expires** or is **redeemed** by its own tag: the partner sees both
in the [status push](ocpi-push.md), where the EVSE goes from `RESERVED` to `AVAILABLE` or
`CHARGING`. Nor when the partner cancelled it with `CANCEL_RESERVATION`: that already has
its own result.

## Which tags the station accepts

The station only starts a session for a tag it is told is valid. cpms answers `Authorize`
and `StartTransaction` with `Accepted` for

- `auth.default_id_tag` and `auth.id_tags`;
- the token `uid` of an **active reservation**, so that it can be redeemed;
- the token `uid` of a `START_SESSION`, for **15 minutes** after the command (the window OCPI
  gives a CPO to skip real-time authorisation). It is kept in memory: after a restart of cpms
  the window is gone.

The temporary tags come with an `expiryDate` in the answer's `idTagInfo`: the end of the
15-minute window for a `START_SESSION`, the reservation's expiry for a reserved tag. A
station keeps what it is told in its Authorization Cache, and an `Accepted` without an
`expiryDate` has no end. The configured tags have none.

Tags compare case-insensitively. cpms does not authorise tokens with the partner
(`POST tokens/{uid}/authorize`); the spec lets a CPO trust the eMSP to send valid ones.

## `STOP_SESSION` and the `session_id`

OCPI identifies a session by the `session_id` it learns from the Sessions module, which cpms
does not have. cpms therefore reads `session_id` as **its own transaction id**: the one
`cpms status` shows for the running session, and the one in the log. A partner learns it only
out of band. A `session_id` that is not an active transaction gets `UNKNOWN_SESSION` at once.

## Several chargers

"Offline" means the charger that owns the target EVSE: a command for an EVSE on a connected
charger works while another charger of the location is down. The `timeout` is that
charger's too.

## Limits

- A `ReserveNow` lost to a dropped connection is not cancelled when the station reconnects:
  it holds the EVSE until it expires, and cpms answers `EVSE_OCCUPIED` meanwhile. See
  [`control`](../architecture/overview.md#reservations).
- Results are not kept across a restart of cpms.
- No Sessions module, CDRs or tariffs.

## Try it without hardware

```sh
./bin/cpms run                                              # terminal 1
./bin/cpms simulate charger --id ALP-HYC-001                # terminal 2
# register a partner, then POST reserve.json as above      # terminal 3
./bin/cpms status
```

Two things the block leaves out:

- **Register a partner first.** Commands need `TOKEN_C`, which only a registered partner
  has, and results go to the partner's `response_url`. The `curl` that registers one is in
  [OCPI partner](ocpi-partner.md#steps) ("Try it by hand with curl"); its `url` must
  answer with OCPI versions, or the registration fails with `3001`.
- **The simulator uses the same `config.yaml`** as `cpms run` (the one in the working
  directory, or the one `-c` names): it takes the charger, its EVSEs and the CSMS address
  from it, and `reserve.json` has to use that file's `location_id` and EVSE uids. To redeem
  the reservation at the simulator with the partner's token, start it with `--tag` set to
  that token's `uid`.

`cpms simulate charger --scenario reject-reserve` makes the result `REJECTED`, `occupied`
gives `EVSE_OCCUPIED`, `slow` gives `TIMEOUT`, and `unlock-fails` turns an
`UNLOCK_CONNECTOR` into `FAILED`. See [Test with the simulator](simulator.md).
