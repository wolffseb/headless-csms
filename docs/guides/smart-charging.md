# Limit charging power

`cpms limit` and `cpms profile` set OCPP 1.6 charging profiles: limits on the power or
current an EVSE, or the whole station, may draw. The station needs the SmartCharging
feature profile ([`cpms probe`](../reference/probe.md) tells you).

## Quick examples

```console
$ cpms limit set --evse ALP-HYC-001-1 --kw 20
ALP-HYC-001-1: charging profile #1 accepted: tx-default stack 0, 20.0 kW

$ cpms limit show --evse ALP-HYC-001-1
ALP-HYC-001-1: composite schedule from 10:03:58 for 1h0m0s
FROM      UNTIL     LIMIT
10:03:58  11:03:58  20.0 kW

$ cpms limit clear --all
```

## Commands

| Command | OCPP message | Does |
|---|---|---|
| [`cpms limit set`](../reference/cli.md#cpms-limit-set) | `SetChargingProfile` | Installs a one-period limit from now on. Exactly one of `--kw` (sent in W) or `--amps` (per phase). |
| [`cpms limit show`](../reference/cli.md#cpms-limit-show) | `GetCompositeSchedule` | The limits the station actually applies, all profiles combined. |
| [`cpms limit clear`](../reference/cli.md#cpms-limit-clear) | `ClearChargingProfile` | Clears by `--id`, by `--purpose`, or `--all`. |
| [`cpms profile set -f`](../reference/cli.md#cpms-profile-set) | `SetChargingProfile` | Installs a full profile from a [JSON file](#profile-file). |

## Purposes

| `--purpose` | OCPP purpose | Applies to | Notes |
|---|---|---|---|
| `tx-default` (default) | `TxDefaultProfile` | Sessions that start after it is set | Can be set while an EVSE is reserved and idle. |
| `tx` | `TxProfile` | The running session | Overrides `tx-default` and ends with the session. With no session cpms refuses it (exit 9) unless `--force`. |
| `station-max` | `ChargePointMaxProfile` | The whole station | Needs `--station`, and `--station` takes nothing else. |

- Within one purpose the highest `--stack` level wins.
- `--for 1h` sets how long the limit holds. Without it the limit holds until cleared.
- cpms allocates the profile ids. They are never reused, also across restarts.
- A profile with the same id, or the same EVSE, purpose and stack level, replaces the one before.

## What cpms remembers

A profile is recorded in `state.json` once the station answers `Accepted`. `cpms status`
lists the records, marked **as sent**:

```
Charging profiles, as sent (the station may have dropped them; `cpms limit show` asks it):
  ALP-HYC-001-1  #1  tx-default stack 0, 20.0 kW
```

The station is the source of truth and may have dropped them, for instance on a reboot.
`cpms limit show` asks it.

| Record | Goes when |
|---|---|
| `tx` | Its session stops. |
| `tx` sent with `--force` and no session | The next session on its EVSE stops. |
| Any | A clear matches it, also one the station answers `Unknown`. |

In `status --json` they are `charging_profiles`, on `charge_point` and on each EVSE.

## Answers and exit codes

| Station answers | Exit | Meaning |
|---|---|---|
| `Accepted` | 0 | Done. |
| `Rejected` | 4 | It will not take this profile: a `tx` one without a session, a unit it does not allow, a stack level or period count beyond its limits, too many profiles. |
| `Unknown` (on clear) | 4 | The station holds no matching profile. cpms drops its records anyway. |
| `NotSupported`, or a `NotImplemented` CALLERROR | 11 | The station does not support this request. |
| (no answer) | 7 | See the warning below. |
| (not sent) | 12 | The station was busy. Nothing went out, so there is no id to look for. |

Mistakes in the flags or the file exit 1 before anything is sent.

!!! warning "No answer does not mean no profile"
    When a profile went out but no answer came back (exit 7, or 3 if the connection
    dropped), the station may hold it although cpms records nothing. The message names the
    id it was sent with. Check with `cpms limit show`, remove with `cpms limit clear --id N`.

## Profile file

For `cpms profile set -f profile.json`. The id is not part of it: cpms allocates one.

```json
{
  "purpose": "tx-default",
  "stack_level": 1,
  "kind": "absolute",
  "valid_from": "2026-11-17T08:00:00Z",
  "valid_to": "2026-11-17T18:00:00Z",
  "schedule": {
    "unit": "W",
    "start": "2026-11-17T08:00:00Z",
    "duration_s": 36000,
    "min_rate": 4000,
    "periods": [
      {"start_s": 0, "limit": 22000},
      {"start_s": 3600, "limit": 11000, "phases": 3}
    ]
  }
}
```

| Field | Required | Values |
|---|---|---|
| `purpose` | yes | `tx-default`, `station-max` or `tx` |
| `transaction_id` | | Only with `tx` |
| `stack_level` | | Integer, default 0 |
| `kind` | | `absolute` (default), `relative` or `recurring` |
| `recurrency` | with `recurring` | `daily` or `weekly`; also needs `schedule.start` |
| `valid_from`, `valid_to` | | RFC 3339 times |
| `schedule.unit` | yes | `W` or `A` |
| `schedule.periods` | yes | `start_s` from 0 and increasing; `limit` with at most one decimal place; optional `phases` |
| `schedule.start`, `duration_s`, `min_rate` | | |

Unknown fields are refused.

## See also

- [Runbook: a limit while reserved on an Alpitronic HYC50](hyc50-runbook.md)
- [Read power and current](metering.md), to see a limit take effect
- The simulator's [charging profile model](simulator.md#charging-profiles)
