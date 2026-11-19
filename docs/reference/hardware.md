# Tested hardware

## Alpitronic HYC50

| | |
|---|---|
| Firmware | `hyc_v3.0.1` |
| Protocol | OCPP 1.6-J, plain `ws://`, security profile 0 |
| URL | Dials `<CSMS URL>/ocpp/<identity>`, adding its own identity |

### What it does, and what cpms does about it

| Topic | Seen on the station | What cpms does |
|---|---|---|
| Feature profiles | `SupportedFeatureProfiles` lists 5 of its 6 profiles (`SupportedFeatureProfilesMaxLength` = 5) and leaves out Reservation. | A missing profile on a full list is [unconfirmed](probe.md#missing-is-not-the-same-as-not-reported), not missing. `ReserveNow` and `CancelReservation` work. |
| Reconnect | After a reconnect without a reboot it sends no `StatusNotification`. | Asks for every EVSE's status with `TriggerMessage` after a [short grace](../architecture/sequences.md#the-station-reconnects). |
| Status timing | Reports a change only after `MinimumStatusDuration` (10 s). | Nothing. Expect the delay. |
| RFID | A tag presented with no remote command is sent as `Authorize`, as the UID in uppercase hex without separators. After `Accepted` it asks for the cable. | Answers `Authorize`. Tags compare without regard to case. |
| Remote start | Accepted, then it asks for the cable and waits `ConnectionTimeOut` (120 s). | Nothing. |
| Meter values | `MeterValuesSampledData` is energy only, and `ClockAlignedDataInterval` is 0. | [Turn power and offered values on](../guides/metering.md#turn-the-readings-on) with `cpms config set`. |
| Smart charging | Rate units `Power,Current`, stack levels up to 9, 10 periods, 100 profiles installed. | `cpms limit` stays within these. |

### Not yet checked

- Start, stop and unlock with a car connected
- Which connector id is which outlet
- Whether connector 0 reports a status
- Heartbeat spacing and timestamps
- The station's own offline behaviour
- Meter values during a session
- Charging profiles while a connector is reserved ([runbook](../guides/hyc50-runbook.md))

Results go in [issue #11](https://github.com/wolffseb/headless-csms/issues/11).

## Other stations

Untested. Start with [`cpms probe`](probe.md): it tells you what will work.
