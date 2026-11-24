# Sequences

The important flows, message by message. Message names are OCPP 1.6
([specification](https://openchargealliance.org/protocols/open-charge-point-protocol/)).

| Flow | Starts with |
|---|---|
| [Startup](#startup-and-the-station-connecting) | `cpms run` |
| [Reserve](#reserving-from-the-terminal) | `cpms reserve` |
| [Start and stop](#redeeming-the-reservation-start-and-stop) | `cpms start`, `cpms stop` |
| [Expiry and restart](#expiry-and-restarting-cpms) | A timer, or a restart |
| [Silence](#the-station-goes-silent) | A missing heartbeat |
| [Reconnect](#the-station-reconnects) | The station dialling in again |
| [OCPI reserve](#an-ocpi-partner-reserves) | A partner's `RESERVE_NOW` |
| [The CPO cancels](#the-cpo-cancels) | `cpms cancel` of a reservation made over OCPI |

## Startup, and the station connecting

```mermaid
sequenceDiagram
    autonumber
    participant Op as Operator
    participant Run as cpms run
    participant Core as core
    participant St as Station

    Op->>Run: cpms run -c config.yaml
    Run->>Run: load config, open state.json
    Run->>Run: claim control socket (refuses if another cpms answers)
    Run->>Core: build from state.json<br/>(reservations re-armed, transactions and counters restored)
    Run->>Run: listen for OCPP on server.ocpp_bind
    Note over Run: prints the CSMS URL and the socket path

    St->>Run: WebSocket upgrade GET /ocpp/ALP-HYC-001<br/>Sec-WebSocket-Protocol: ocpp1.6
    Run->>Core: Connected
    St->>Run: BootNotification
    Run->>Core: Booted (vendor, model, firmware)
    Run-->>St: Accepted, interval = charger.heartbeat_interval
    St->>Run: StatusNotification per connector
    Run->>Core: SetConnectorStatus (OCPP status → OCPI status)
    Core-->>Run: event: EVSE UNKNOWN→AVAILABLE (logged)
    loop every heartbeat_interval
        St->>Run: Heartbeat
    end
```

The identity is the **last** segment of the URL path, so `/ocpp/<id>`, `/<id>` and longer
vendor prefixes all work. An identity other than `charger.id` is rejected with HTTP 404.

## Reserving from the terminal

```mermaid
sequenceDiagram
    autonumber
    participant CLI as cpms reserve
    participant Sock as control socket
    participant Ctl as control
    participant Core as core
    participant V16 as ocpp/v16
    participant St as Station

    CLI->>CLI: check --evse against config (exit 1 if unknown, nothing sent)
    CLI->>Sock: {"v":2,"op":"reserve","evse":"ALP-HYC-001-1"}
    Sock->>Ctl: Reserve
    Ctl->>Core: BeginReservation: allocate id, persist counter, pending
    Ctl->>V16: ChargePoint.ReserveNow
    V16->>St: [2,"7","ReserveNow",{connectorId, expiryDate, idTag, reservationId}]
    St-->>V16: [3,"7",{"status":"Accepted"}]
    V16-->>Ctl: Result Accepted
    Ctl->>Core: ActivateReservation: persist, arm expiry timer
    Core-->>Core: event: reservation created
    Ctl-->>Sock: reservation id + expiry
    Sock-->>CLI: {"ok":true,"result":{...}}
    CLI->>CLI: print, exit 0
    St->>V16: StatusNotification Reserved
    V16->>Core: EVSE AVAILABLE→RESERVED
```

If the station answers anything but `Accepted`, or does not answer within
`charger.call_timeout`, the pending reservation is ended at once and the CLI exits with the
matching code (4–7). After a timeout or a failure the station may still have received the
request, so `cpms run` sends a `CancelReservation` in the background and the reservation shows
as `state: cancelling` in `cpms status` until the station has answered it; the EVSE refuses a
new reservation (exit 9) meanwhile. The cancel waits for its turn behind any command on the
wire, and a cancel that does not get its turn within `charger.call_timeout` is queued again,
not dropped. The record ends when the station answers, when the connection drops, when the
cancel itself goes unanswered for `charger.call_timeout`, or when `cpms run` shuts down. In the
last three cases the station may still hold the reservation until its expiry, and answers a new
reserve on that EVSE `Occupied` until then. A
reservation that was never sent (exit 12) has nothing to
cancel: it ends at once and no `CancelReservation` follows. It is not persisted, like a pending one. `core` only shows `RESERVED` when the station reports it; the
reservation record sits next to the status rather than overwriting it.

## Redeeming the reservation: start and stop

```mermaid
sequenceDiagram
    autonumber
    participant CLI as cpms start / stop
    participant Ctl as control
    participant Core as core
    participant V16 as ocpp/v16
    participant St as Station

    CLI->>Ctl: Start (via socket), tag = auth.default_id_tag
    Ctl->>Core: allocate remote start id
    Ctl->>V16: RemoteStart
    V16->>St: RemoteStartTransaction {connectorId, idTag}
    St-->>V16: Accepted
    Ctl-->>CLI: exit 0, "remote start accepted"
    Note over St: may first send Authorize for the tag,<br/>and may wait for a car to be plugged in
    St->>V16: StartTransaction {connectorId, idTag, meterStart, reservationId}
    V16->>Core: next transaction id (persisted)
    V16->>Core: StartTransaction: consumes the reservation
    V16-->>St: {transactionId, idTagInfo: Accepted}
    St->>V16: StatusNotification Charging
    V16->>Core: EVSE RESERVED→CHARGING
    loop every MeterValueSampleInterval, if not 0
        St->>V16: MeterValues {connectorId, transactionId, energy, power, ...}
    end

    CLI->>Ctl: Stop (via socket)
    Ctl->>Core: find the active transaction on the EVSE
    Ctl->>V16: RemoteStop
    V16->>St: RemoteStopTransaction {transactionId}
    St-->>V16: Accepted
    St->>V16: StopTransaction {transactionId, meterStop, reason}
    V16->>Core: StopTransaction (persisted)
    St->>V16: StatusNotification Finishing / Available
```

A session that starts **without** `reservationId` on the same connector with the same tag
also consumes the reservation, because some stations leave the field out. A different tag
leaves the reservation in place.

## Expiry, and restarting cpms

```mermaid
sequenceDiagram
    autonumber
    participant Core as core
    participant Store as state.json
    participant St as Station

    Note over Core: expiry timer fires at ExpiresAt
    Core->>Core: end reservation as "expired" (no station message needed)
    Core->>Store: write active reservations, transactions, counters
    St->>Core: StatusNotification Available (the station expires its own copy)

    Note over Core,Store: cpms run is restarted
    Store->>Core: load
    Core->>Core: drop reservations that expired while down,<br/>re-arm the rest, restore active transactions,<br/>continue the id counters
```

Ids are never handed out twice, even after a restart: the counters are persisted with every
allocation, and on load they are also kept ahead of every id still in the file.

## The station goes silent

```mermaid
sequenceDiagram
    autonumber
    participant St as Station
    participant Conn as ocppj connection
    participant Core as core

    St->>Conn: last Heartbeat / StatusNotification
    Note over Conn: pings keep checking the TCP connection,<br/>but only OCPP messages count as a sign of life
    Conn->>Conn: nothing for charger.heartbeat_timeout
    Conn->>Core: Disconnected ("heartbeat timeout")
    Core->>Core: every EVSE → UNKNOWN
    Note over St,Core: a real station reconnects on its own;<br/>see the next diagram for how the statuses come back
```

## The station reconnects

OCPP 1.6 asks for `StatusNotification`s only after a `BootNotification`. A station that
reconnects without rebooting (the Alpitronic HYC50 does) may send nothing but heartbeats,
and until a connector changes state every EVSE would read `UNKNOWN` after a restart of
cpms, or keep the status from before the outage, in `cpms status` and over OCPI. So every
new connection gets a `TriggerMessage(StatusNotification)` per configured EVSE, unless the
station boots within 5 seconds of connecting, in which case it reports by itself.

```mermaid
sequenceDiagram
    autonumber
    participant St as Station
    participant Srv as csms server
    participant Ctl as control
    participant Core as core

    St->>Srv: WebSocket connect
    Srv->>Core: Connected
    Srv->>Ctl: RefreshStatusOnConnect (own goroutine, ends with the connection)
    Note over Ctl: waits 5 s for a BootNotification
    alt the station booted
        St->>Core: BootNotification, then a StatusNotification per connector
        Note over Ctl: nothing to ask
    else it did not
        loop each configured EVSE, one at a time
            Ctl->>St: TriggerMessage(StatusNotification, connectorId)
            St-->>Ctl: Accepted
            St->>Core: StatusNotification
        end
    end
```

Each request is bounded by `charger.call_timeout` and nothing is retried. A station
without the RemoteTrigger profile (`Rejected`, `NotImplemented`, or a CALLERROR) gets one
INFO line in the log and stays connected; its EVSEs then wait for its next status change.

## An OCPI partner reserves

The OCPI side ends in the same `control.Reserve` as the terminal. The command is answered
before the station is asked, and the result is posted afterwards. See
[Take commands from the partner](../guides/ocpi-commands.md). The status `PATCH` at the end
is [the push](../guides/ocpi-push.md).

```mermaid
sequenceDiagram
    autonumber
    participant P as OCPI partner
    participant O as ocpi/commands
    participant Ctl as control
    participant Core as core
    participant V16 as ocpp/v16
    participant St as Station
    participant Push as ocpi/push

    P->>O: POST /ocpi/2.3.0/commands/RESERVE_NOW<br/>{token, expiry_date, reservation_id, location_id, evse_uid, response_url}
    O->>O: checks that need no station: body, response_url host,<br/>EVSE, charger online, token, expiry
    O-->>P: 200, 1000: CommandResponse { ACCEPTED, timeout: 65 }
    Note over O: from here in its own goroutine,<br/>bound by the request's timeout
    O->>Ctl: Reserve (same path as the terminal)
    Ctl->>Core: BeginReservation: id, pending, response_url
    Ctl->>V16: ChargePoint.ReserveNow
    V16->>St: [2,"7","ReserveNow",{connectorId, expiryDate, idTag, reservationId}]
    St-->>V16: [3,"7",{"status":"Accepted"}]
    V16-->>Ctl: Result Accepted
    Ctl->>Core: ActivateReservation: persist, arm expiry timer
    Ctl-->>O: reservation (no error)
    O->>P: POST response_url (TOKEN_B)<br/>CommandResult { ACCEPTED }
    St->>V16: StatusNotification Reserved
    V16->>Core: EVSE AVAILABLE→RESERVED
    Core-->>Push: event: EVSE status changed
    Push->>P: PATCH /locations/.../{evse_uid} { status: RESERVED }
```

Anything the station refuses or leaves unanswered ends in the same place, with another
`result`: `Rejected` is `REJECTED`, `Occupied` is `EVSE_OCCUPIED`, no answer is `TIMEOUT`
([the table](../guides/ocpi-commands.md#the-result)). A `CommandResult` the partner does not
take is sent again after a backoff, in its own goroutine, so the next command is not held up.

### The CPO cancels

A reservation that was active and ends because the operator cancelled it is reported to the
partner without being asked, as `CANCELED_RESERVATION`. `ocpi/commands` finds out from the
event `core` publishes.

```mermaid
sequenceDiagram
    autonumber
    participant Op as cpms cancel
    participant Ctl as control
    participant V16 as ocpp/v16
    participant St as Station
    participant Core as core
    participant O as ocpi/commands
    participant P as OCPI partner

    Op->>Ctl: Cancel (EVSE)
    Ctl->>V16: ChargePoint.CancelReservation
    V16->>St: [2,"8","CancelReservation",{reservationId}]
    St-->>V16: [3,"8",{"status":"Accepted"}]
    V16-->>Ctl: Result Accepted
    Ctl->>Core: EndReservation (cancelled)
    Core-->>O: event: reservation ended, with its response_url
    O->>P: POST response_url (TOKEN_B)<br/>CommandResult { CANCELED_RESERVATION }
    St->>V16: StatusNotification Available
    Note over P: the status PATCH AVAILABLE follows, as for any change
```

The event carries the `response_url` only for a reservation that was **active**, and only
the reasons `cancelled` and `failed` (another tag started a session on the EVSE) are
reported. Expiry and redemption tell the partner nothing, and neither does the partner's own
`CANCEL_RESERVATION`, which has its result. The post goes out only while the registration
the `response_url` arrived under is still the current one.
