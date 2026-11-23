# CLI reference

```
cpms [command] [flags]
```

## Overview

| Command | Does | Needs `cpms run` |
|---|---|---|
| [`run`](#cpms-run) | Runs the CSMS and the OCPI server | |
| [`status`](#cpms-status) | Shows the stations, EVSEs, reservations and sessions | yes |
| [`reserve`](#cpms-reserve) | Reserves an EVSE for an RFID tag | yes |
| [`cancel`](#cpms-cancel) | Cancels the reservation on an EVSE | yes |
| [`start`](#cpms-start) | Starts a charging session | yes |
| [`stop`](#cpms-stop) | Stops the session on an EVSE | yes |
| [`unlock`](#cpms-unlock) | Releases the cable lock | yes |
| [`probe`](#cpms-probe) | Reports what the station supports | yes |
| [`limit set`](#cpms-limit-set), [`show`](#cpms-limit-show), [`clear`](#cpms-limit-clear) | Charging limits | yes |
| [`profile set`](#cpms-profile-set) | Installs a full charging profile | yes |
| [`config validate`](#cpms-config-validate) | Checks `config.yaml` | |
| [`config set`](#cpms-config-set) | Sets a configuration key **on the station** | yes |
| [`ocpi unregister`](#cpms-ocpi-unregister) | Forgets the OCPI registration | yes |
| [`simulate charger`](#cpms-simulate-charger) | Pretends to be a station | |
| `version`, `completion` | Version, shell completion | |

## Global flags

| Flag | Default | |
|---|---|---|
| `-c`, `--config` | `config.yaml` | The configuration file. |
| `--state` | `state.json` next to the config file | The state file. |
| `--socket` | `cpms.sock` next to the state file | The control socket. Use the same value for `run` and the commands. |

`--evse UID` takes an EVSE `uid` from config. It may be left out when only one EVSE is
configured. A typo is caught before anything is sent, and the error lists the valid uids.

### Several chargers

With [`chargers:`](configuration.md#charger-and-chargers) in config, a command that names an
EVSE (`reserve`, `cancel`, `start`, `stop`, `unlock`, and `limit` or `profile` with `--evse`)
goes to the charger that EVSE sits on. You do not name the charger.

A command about a station rather than an EVSE takes **`--charger ID`**: `probe`,
`config set`, and `limit` / `profile` with `--station` (or `limit clear` with no `--evse`).
`--charger` is required when more than one charger is configured, and may be left out
otherwise. Without it the command exits 1 and lists the ids:

```
cpms: --charger is required with 2 chargers configured; one of: PRUESTEL_000, PRUESTEL_001
```

An unknown id exits 1 too, and so does `--evse` together with a `--charger` that does not
own it.

## Exit codes

Stable, so scripts can branch on them.

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Usage, config, or unexpected error (including an unknown `--evse`) |
| 2 | `cpms run` is not reachable |
| 3 | The charge point is not connected to cpms, or the connection dropped while the command was queued or waiting for its answer (it may or may not have reached the station) |
| 4 | The station rejected the command (`config set`: also `NotSupported`) |
| 5 | The station is occupied |
| 6 | The station is faulted or unavailable |
| 7 | The station did not answer within `charger.call_timeout` |
| 8 | Unlock failed, or the station does not support the command |
| 9 | Refused by cpms before sending: already reserved, no reservation to cancel, nothing to stop, a `tx` limit with no session (without `--force`), shutting down, nothing registered |
| 10 | `probe`: the station reported that it lacks a capability cpms requires |
| 11 | `limit` / `profile`: the station does not support this charging profile or request |
| 12 | The command was **not sent**: the station stayed busy with an earlier command for the whole of `charger.call_timeout`. Nothing happened, so repeating it is safe |

!!! tip "7 or 12?"
    One command is in flight at a time. **12** means yours never went out: repeat it.
    **7** means it went out and got no answer: the station may have acted, so check
    `cpms status` first.

## Output

| Stream | Carries |
|---|---|
| stdout | Reports and confirmations: `status`, `probe`, `config validate`, `version`, the startup lines of `run`. |
| stderr | Logs, validation problems, `warning:` lines and `cpms: …` errors. |

So piping a report works:

```sh
cpms probe --json | jq .ok
```

---

## Server

### `cpms run`

Starts the OCPP and OCPI servers and logs what the charge point does.

| Flag | Default | |
|---|---|---|
| `--log-level` | `info` | `debug`, `info`, `warn` or `error`. `debug` shows the protocol messages and one line per OCPI request. |

It claims the [control socket](#the-control-socket). A second `cpms run` on the same state
exits with "another cpms is already running".

---

## Station commands

### `cpms status`

The stations, per EVSE its status, reservation, session and [meter reading](../guides/metering.md),
and the OCPI registration.

| Flag | |
|---|---|
| `--json` | A stable format for scripts. |
| `--charger ID` | Show only this charger, in the single-charger layout. |

With one charger the output is as below. With several, the table has one section per
charger, in config order, with that charger's EVSEs, reservations and sessions under it, and
the OCPI line once at the end. The JSON has a `chargers` array instead of the top-level
`charge_point` and `evses`; each element holds those two keys for one charger. Those two
top-level keys are absent, so `jq .charge_point.online` prints `null` rather than failing,
and a check like `[ "$(... | jq .charge_point.online)" = true ]` reports offline. Scripts
should read `.chargers[]`, or pass `--charger ID` to get the single-charger shape.

```
PRUESTEL_000  online  last seen 10:04:03

EVSE            STATUS     RESERVATION  TRANSACTION                          METER
PRUESTEL_000-1  CHARGING   -            1 RFID_PRUESTEL_001 since 10:03:59   -

PRUESTEL_001  offline  never seen

EVSE            STATUS   RESERVATION  TRANSACTION  METER
PRUESTEL_001-1  UNKNOWN  -            -            -

OCPI  not registered
```

=== "Table"

    ```
    ALP-HYC-001  online  Alpitronic HYC300 fw simulator  last seen 10:04:03

    EVSE           STATUS     RESERVATION  TRANSACTION                  METER
    ALP-HYC-001-1  CHARGING   -            1 04A1B2C3D4 since 10:03:59  20.0 kW 50.0 A, 1s ago
    ALP-HYC-001-2  AVAILABLE  -            -                            -

    Charging profiles, as sent (the station may have dropped them; `cpms limit show` asks it):
      ALP-HYC-001-1  #1  tx-default stack 0, 20.0 kW

    OCPI  not registered
    ```

=== "JSON"

    ```json
    {
      "charge_point": {
        "id": "ALP-HYC-001",
        "online": true,
        "last_seen": "2026-11-19T09:04:03.045923Z",
        "vendor": "Alpitronic",
        "model": "HYC300",
        "firmware": "simulator",
        "station_status": "AVAILABLE",
        "last_disconnect": null,
        "charging_profiles": []
      },
      "evses": [
        {
          "uid": "ALP-HYC-001-1",
          "status": "CHARGING",
          "error_code": "NoError",
          "reservation": null,
          "transaction": {
            "id": "1",
            "id_tag": "04A1B2C3D4",
            "started_at": "2026-11-19T09:03:59Z"
          },
          "power_w": 20000,
          "current_a": 50,
          "reading_at": "2026-11-19T09:04:03Z",
          "power_at": "2026-11-19T09:04:03Z",
          "current_at": "2026-11-19T09:04:03Z",
          "charging_profiles": [
            {
              "id": 1,
              "purpose": "tx-default",
              "stack_level": 0,
              "unit": "W",
              "periods": [{ "start_s": 0, "limit": 20000 }],
              "set_at": "2026-11-19T09:03:58.953533Z",
              "source": "cli"
            }
          ]
        }
      ],
      "ocpi": { "registered": false }
    }
    ```

### `cpms reserve`

Holds an EVSE for a tag (`ReserveNow`).

| Flag | Default | |
|---|---|---|
| `--evse` | the only EVSE | |
| `--tag` | `auth.default_id_tag` | The RFID tag to hold it for. |
| `--expires` | `15m` | How long the reservation holds. |

### `cpms cancel`

Releases the reservation on an EVSE (`CancelReservation`). Flag: `--evse`.

### `cpms start`

Asks the station to start a session (`RemoteStartTransaction`). It returns once the station
accepts. The session shows up in `status` when the station reports it.

| Flag | Default | |
|---|---|---|
| `--evse` | the only EVSE | |
| `--tag` | `auth.default_id_tag` | The RFID tag to start with. |

### `cpms stop`

Stops the session running on an EVSE (`RemoteStopTransaction`).

| Flag | Default | |
|---|---|---|
| `--evse` | the only EVSE | |
| `--tx` | the one running on the EVSE | Only needed when cpms holds two sessions on one EVSE. Must name a session active there (exit 9 otherwise). |

### `cpms unlock`

Releases the cable lock (`UnlockConnector`). Flag: `--evse`.

### `cpms probe`

Reports what the connected station supports. Read-only. See [Probe report](probe.md).

| Flag | |
|---|---|
| `--charger ID` | The station to probe. Required with more than one charger. |
| `--raw` | Also lists every configuration key the station reported. |
| `--json` | The whole report as JSON, always with every key. |

---

## Smart charging

Background and examples: [Limit charging power](../guides/smart-charging.md).

### `cpms limit set`

Installs a one-period charging profile from now on.

| Flag | Default | |
|---|---|---|
| `--kw` | | Power limit in kW, sent as W. Exactly one of `--kw` and `--amps`. |
| `--amps` | | Current limit in A per phase. |
| `--evse` / `--station` | the only EVSE | `--station` addresses the whole station (connector 0). |
| `--charger` | the only charger | With `--station`: which station. Required with more than one charger. |
| `--purpose` | `tx-default` | `tx-default`, `station-max` or `tx`. |
| `--stack` | `0` | Stack level. The highest of one purpose wins. |
| `--for` | until cleared | How long the limit holds, for example `1h`. |
| `--tx` | the running session | With `--purpose tx`: the transaction to limit. |
| `--force` | | With `--purpose tx`: send it even with no session running. |

### `cpms limit show`

The limits the station applies (`GetCompositeSchedule`), as a period table.

| Flag | Default | |
|---|---|---|
| `--evse` / `--station` | the only EVSE | |
| `--charger` | the only charger | With `--station`: which station. Required with more than one charger. |
| `--duration` | `1h` | How far ahead to look. |
| `--unit` | the station's choice | `W` or `A`. |

### `cpms limit clear`

Clears profiles (`ClearChargingProfile`). One of `--id`, `--purpose` or `--all` is required.

| Flag | |
|---|---|
| `--id N` | The profile id to clear. |
| `--purpose P` | Every profile of this purpose. |
| `--all` | Every profile. |
| `--evse` / `--station` | Narrows `--purpose` and `--all`, which otherwise clear on every EVSE and the station. |
| `--charger` | Which station, when not narrowed by `--evse`. Required with more than one charger: clearing is per charger. |

### `cpms profile set`

Installs a full profile from a JSON file. The shape is in
[Profile file](../guides/smart-charging.md#profile-file).

| Flag | |
|---|---|
| `-f`, `--file` | The profile, as JSON. |
| `--evse` / `--station` | |
| `--charger` | With `--station`: which station. Required with more than one charger. |

---

## Configuration

### `cpms config validate`

Loads `config.yaml`, applies defaults and validates it. Does not need `cpms run`.

- Valid: exits 0 and prints a summary, including the station's CSMS URL.
- Invalid: exits 1 and names **every** problem by its field path, in one pass.

```
config.yaml is not valid:

  auth.default_id_tag: "04A1 B2C3" is not a valid OCPP idTag; want 1-20 printable ASCII characters without spaces
  charger.id: is required
  location.evses[1].ocpp_connector_id: 1 is already mapped by location.evses[0]

3 problems found.
```

### `cpms config set`

```
cpms config set <key> <value>
```

Sets one of the **station's** OCPP configuration keys (`ChangeConfiguration`). It does not
touch `config.yaml`. Answers and exit codes: [Read power and current](../guides/metering.md#cpms-config-set).
With more than one charger, add `--charger ID`.

---

## OCPI

### `cpms ocpi unregister`

Clears the OCPI registration from `state.json` and tells the partner with a `DELETE` on its
credentials endpoint. `TOKEN_A` works again, so the partner can redo the handshake. See
[Register an OCPI partner](../guides/ocpi-partner.md#change-or-end-a-registration).

---

## Simulator

### `cpms simulate charger`

Dials a CSMS and behaves like a charging station. Does not need `cpms run` on the same
host: point `--csms` anywhere. With several chargers in config, `--id` is required and picks
that charger: its connectors default to that charger's EVSEs, for example
`cpms simulate charger --id PRUESTEL_001`. Flags and scenarios:
[Test with the simulator](../guides/simulator.md).

---

## The control socket

One-shot commands reach `cpms run` through a Unix socket.

| Property | Value |
|---|---|
| Default path | `cpms.sock` next to the state file |
| Permissions | `0600`, removed on shutdown |
| Stale socket | One left by a killed `cpms run` is detected and replaced. |
| Long paths | Socket paths are limited to 103 bytes (a kernel limit on macOS). A longer default falls back to `cpms-<hash>.sock` in the temp directory, which `run` and the commands derive on their own. `cpms run` prints the path it uses. |
