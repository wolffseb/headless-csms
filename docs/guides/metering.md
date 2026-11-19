# Read power and current

`cpms status` shows the newest meter reading per EVSE: what the car draws, the most the
station offers it, and how old the reading is.

```console
$ cpms status
EVSE           STATUS    RESERVATION  TRANSACTION                 METER
ALP-HYC-001-1  CHARGING  -            1 04A1B2C3D4 since 14:28:32  50.0 kW 125.0 A, offered 150.0 kW, 3s ago
```

**Offered** is the limit the station applies. A [charging profile](smart-charging.md) changes it.

## Turn the readings on

A station only reports the measurands listed in its configuration. Many report energy only
out of the box (the Alpitronic HYC50 does). Turn the rest on once:

```sh
cpms config set MeterValuesSampledData \
  Energy.Active.Import.Register,Power.Active.Import,Power.Offered,Current.Import,Current.Offered
```

`cpms probe --raw` shows the current values.

## Two kinds of meter data

| | Sampled | Clock-aligned |
|---|---|---|
| Sent | During a session only | Always, also when idle or reserved |
| Measurands | `MeterValuesSampledData` | `MeterValuesAlignedData` |
| Interval | `MeterValueSampleInterval` | `ClockAlignedDataInterval` (`0` = off) |

So on an `Available` or `Reserved` connector the `METER` column stays `-` unless aligned
data is on. To see the offered limit on a reserved connector:

```sh
cpms config set MeterValuesAlignedData Power.Offered,Current.Offered
cpms config set ClockAlignedDataInterval 60
```

!!! note
    An aligned value is sent at the **end** of each interval, stamped with its start, and
    may be an average. With 60 s its age reads between one and two minutes. The HYC50
    takes up to 7 aligned measurands (`MeterValuesAlignedDataMaxLength`).

## `cpms config set`

Sets one of the **station's** OCPP configuration keys (`ChangeConfiguration`). It does not
touch `config.yaml`. The station keeps the value, and cpms does not set it again after a reboot.

| Station answers | Exit | Meaning |
|---|---|---|
| `Accepted` | 0 | Set. |
| `RebootRequired` | 0 | Set, but applies after the station restarts. Printed as a `warning:` on stderr. |
| `Rejected` | 4 | A read-only key, or a value the station does not take. |
| `NotSupported` | 4 | The station does not know the key. |

## Fields in `status --json`

All are omitted when absent.

| Field | Unit | From measurand |
|---|---|---|
| `power_w` | W | `Power.Active.Import` |
| `current_a` | A | `Current.Import` |
| `power_offered_w` | W | `Power.Offered` |
| `current_offered_a` | A | `Current.Offered` |
| `reading_at` | time | When the station took the newest `MeterValues` |
| `power_at`, `current_at`, `power_offered_at`, `current_offered_at` | time | When each value was taken |

The station's own meter (connector `0`) is `charge_point.meter`, with the same fields. In
the table it is on the station's line: `ALP-HYC-001  online  …  meter offered 50.0 kW, 3s ago`.

## How readings are kept

- Stations mix messages with different measurands. A message without a value keeps the one
  before, so values can differ in age: `50.0 kW 125.0 A 3s ago, offered 22.0 kW 1m0s ago`.
- A station that reports only phases: power is the sum of the phases, current the highest phase.
- `kW` is converted to W. A value cpms cannot read is logged and left out.
- Readings live in memory only. They are dropped when the session stops or the station disconnects.
- A `MeterValues` for a transaction that is not running on that EVSE (late, or replayed) is
  logged but not kept. One without a transaction is kept: that is clock-aligned data.
