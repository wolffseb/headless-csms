# Connect a real station

Point a charger at `cpms` and find out what it supports.

## 1. Describe the station

In `config.yaml`, set the identity the station will send and one EVSE per outlet.

```yaml
charger:
  id: ALP-HYC-001            # the charge point identity
server:
  ocpp_bind: 0.0.0.0:9000    # where the station connects
location:
  evses:
    - uid: ALP-HYC-001-1
      ocpp_connector_id: 1   # the OCPP connector this EVSE is
      # ...
```

All fields are in the [configuration reference](../reference/configuration.md).

## 2. Get the CSMS URL

```console
$ cpms config validate
  ...
  Station CSMS URL    ws://<this-host>:9000/ocpp/ALP-HYC-001
```

## 3. Configure the station

Enter that URL as the station's CSMS (backend) URL, with the host's LAN address.

| Setting on the station | Value |
|---|---|
| CSMS URL | `ws://<host>:9000/ocpp/<charger.id>` |
| Protocol | OCPP 1.6-J |
| Security | Plain `ws://`, security profile 0. There is no TLS: `cpms` is a LAN tool by decision. |

!!! info "Stations build the URL differently"
    The identity is the **last** path segment, so `/ocpp/<id>`, `/<id>` and
    `/steve/websocket/CentralSystemService/<id>` all work. Some stations (the Alpitronic
    HYC50) append their identity to the configured URL themselves: leave it off there.

## 4. Run

```console
$ cpms run
cpms listening for ALP-HYC-001 on ws://127.0.0.1:9000/ocpp/ALP-HYC-001
waiting for the charge point to connect; press Ctrl-C to stop
INFO msg="charge point connected" charge_point=ALP-HYC-001 version=1.6
INFO msg="ALP-HYC-001 booted (Alpitronic HYC300)"
INFO msg="ALP-HYC-001-1 UNKNOWN→AVAILABLE"
```

`--log-level debug` shows the protocol messages underneath.

## 5. Probe

Not every station implements reservations. `cpms probe` reads the station's configuration
and reports what `cpms` can rely on. It changes nothing on the station.

```console
$ cpms probe
ALP-HYC-001  OCPP 1.6
  Alpitronic HYC300, firmware simulator

Features
  Core                ✓  required  start, stop, unlock
  Reservation         ✓  required  reserve, cancel, OCPI RESERVE_NOW
  RemoteTrigger       ✓  optional  status refresh on demand
  SmartCharging       ✓  optional  limit, profile
  FirmwareManagement  ✓            not used by cpms
...
Every capability cpms requires is present.
```

| Result | Exit code | What to do |
|---|---|---|
| Everything required is present | 0 | Carry on with [a charging session](charging-session.md). |
| A required feature shows `?` (unconfirmed) | 0 | The station did not say. Try it, for example `cpms reserve`. |
| A required feature shows `✗` (missing) | 10 | Reservation missing: set `reservable: false` on the EVSEs. |
| The station is not connected | 3 | See below. |

How to read the whole report: [Probe report](../reference/probe.md).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Nothing connects | The station's URL points elsewhere, or a firewall blocks port 9000. Check `charger.ip` is reachable. |
| The station is refused with HTTP 404 | Its identity is not `charger.id`. |
| Every EVSE stays `UNKNOWN` | The station reconnected without booting and has no RemoteTrigger profile. Its statuses arrive with the next change. |
| The station goes offline every few minutes | It is silent for longer than `charger.heartbeat_timeout`. |
| A status change shows up late | Stations hold a change back for `MinimumStatusDuration` (10 s on the HYC50). |

Quirks of the hardware this was tested on: [Tested hardware](../reference/hardware.md).
