# Probe report

`cpms probe` asks the station for its whole configuration (`GetConfiguration`) and reports
what cpms can rely on. It is read-only. Run it first against any new station.

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

Settings
  ReserveConnectorZeroSupported  false   whether the whole station can be reserved at once; ...
  NumberOfConnectors             2       how many connectors the station has; checked against the EVSEs in config
  HeartbeatInterval              60      seconds between heartbeats; cpms sets it at boot from charger.heartbeat_interval
  ...

Checks
  ok  2 connectors, matching the 2 EVSEs in config
  ok  heartbeat every 60s, as charger.heartbeat_interval says

Every capability cpms requires is present.
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Nothing required is missing. Warnings and unconfirmed features do not change this. |
| 3 | The station is not connected. The command prints the CSMS URL to configure, and when and why the station last disconnected. |
| 10 | The station reported that it lacks a required capability. |

## Features

Named as the station names them: OCPP 1.6 feature profiles.

| Profile | Needed | Used for | Without it |
|---|---|---|---|
| Core | required | start, stop, unlock | |
| Reservation | required | reserve, cancel | Exit 10. Set `reservable: false` on the EVSEs. |
| RemoteTrigger | optional | Asking for a status on demand | cpms only learns of a status change when the station reports it. |
| SmartCharging | optional | `cpms limit`, `cpms profile` | Those commands are refused. Nothing else is affected. |
| FirmwareManagement | | Not used by cpms | |

### Missing is not the same as not reported

| Symbol | Support | Meaning | Exit |
|---|---|---|---|
| `✓` | yes | The station lists the profile. | 0 |
| `✗` | **missing** | The station reported it as absent. | 10 if required |
| `?` | **unconfirmed** | The station said nothing either way. A warning suggests trying it. | 0 |

Two cases produce `?`:

- **No list at all.** Some stations report no `SupportedFeatureProfiles`. Core is mandatory
  in OCPP 1.6, so it is assumed present, and the probe says so.
- **A full list.** A station can list at most `SupportedFeatureProfilesMaxLength` profiles.
  When its list is exactly that long, a profile missing from it may only have been cut off.
  The Alpitronic HYC50 lists five and leaves out Reservation, which it supports.

!!! note "A workaround, not spec"
    OCPP 1.6 errata 3.81 withdraws `SupportedFeatureProfilesMaxLength`. The price of the
    workaround: a station with a complete, exactly full list that really lacks a profile
    shows it as unconfirmed instead of missing.

## Settings

The configuration keys that change how the station behaves towards cpms. A key the station
does not report shows as `not reported`. That is not an error.

| Key | Why it matters |
|---|---|
| `ReserveConnectorZeroSupported` | Informational: cpms always reserves one EVSE. |
| `AuthorizeRemoteTxRequests` | If true, the station asks cpms to `Authorize` the tag of a remote start. cpms accepts `auth.default_id_tag` and `auth.id_tags`. |
| `LocalAuthListEnabled`, `LocalPreAuthorize`, `LocalAuthorizeOffline` | Whether the station lets tags charge without asking cpms. |
| `NumberOfConnectors` | Checked against the EVSEs in config. |
| `HeartbeatInterval` | cpms sets it at boot from `charger.heartbeat_interval`. |
| `ConnectionTimeOut` | Seconds the station waits for a cable after a remote start. |
| `MeterValueSampleInterval`, `MeterValuesSampledData` | Whether and what [meter values](../guides/metering.md) arrive. |
| `ChargingScheduleAllowedChargingRateUnit` | Whether a limit can be set in kW (`Power`), in A (`Current`) or both. |
| `ChargeProfileMaxStackLevel`, `ChargingScheduleMaxPeriods`, `MaxChargingProfilesInstalled` | The station's [charging profile](../guides/smart-charging.md) limits. |

## Checks

Compare the station with config. A mismatch is a warning (`!`) and does not change the exit code.

- The connector count matches the EVSEs in config.
- The heartbeat interval is `charger.heartbeat_interval`.

## Flags

| Flag | Adds |
|---|---|
| `--raw` | Every key the station reported, with whether it is read-only. |
| `--json` | The whole report, always with every key. |

### JSON

```json
{
  "charge_point": { "id": "ALP-HYC-001", "ocpp_version": "1.6", "vendor": "Alpitronic", "model": "HYC300", "firmware": "simulator" },
  "features": [
    { "name": "Core", "support": "yes", "grants": ["remote_start_stop", "unlock"], "assumed": false, "note": "" },
    { "name": "Reservation", "support": "yes", "grants": ["reservation"], "assumed": false, "note": "" }
  ],
  "capabilities": [
    { "name": "remote_start_stop", "support": "yes" }
  ]
}
```

(Shortened.) For scripts:

| Field | |
|---|---|
| `ok` | `false` only when `missing` is not empty. |
| `missing`, `unconfirmed` | Required capabilities, computed from what cpms needs. |
| `warnings` | The `!` checks. |
| `features[].grants` | The capabilities a profile provides, whether or not cpms uses them. |
| `features[].note` | Why the support is what it is when the station's answer alone would mislead, such as a full profile list. Empty otherwise. |
