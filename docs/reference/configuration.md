# Configuration

Everything lives in one YAML file. Start from
[`config.example.yaml`](https://github.com/wolffseb/headless-csms/blob/main/config.example.yaml),
which is commented field by field and checked by the test suite.

```sh
cp config.example.yaml config.yaml
cpms config validate
```

!!! info "Two files, one rule"
    **`config.yaml` is read-only to cpms.** It holds what you decide. What cpms learns at
    runtime goes to [`state.json`](#statejson), the only file it writes.

Durations are written like `60s`, `500ms` or `15m`.

## `charger` and `chargers`

Use `charger:` for one station, or `chargers:` (a list of the same entries) for several on
the same OCPP listener. Exactly one of the two; both or neither is an error. Every entry
has its own heartbeat and call timeouts. Charger ids must be unique, and each station
connects to `ws://<host>:<port>/ocpp/<its id>`.

| Field | Required | Default | |
|---|---|---|---|
| `id` | yes | | The charge point identity the station sends in its WebSocket URL. Letters, digits and `. _ : ~ -`, at most 48 characters. |
| `ip` | | | An IP or hostname. Only used for a reachability pre-check and for display. |
| `ocpp_version` | | `1.6` | `1.6`. (`2.0.1` is planned and not served yet.) |
| `heartbeat_interval` | | `60s` | Handed to the station in the `BootNotification` response. |
| `heartbeat_timeout` | | `90s` | Silence longer than this marks the station offline. Must exceed `heartbeat_interval`. |
| `call_timeout` | | `30s` | How long a command waits for the station to answer. |

## `server`

| Field | Default | |
|---|---|---|
| `ocpp_bind` | `0.0.0.0:9000` | Where the station connects. |
| `ocpi_bind` | `0.0.0.0:8080` | Where OCPI is served. |

The two cannot share a port, and port `0` is refused: a charger cannot be pointed at a
randomly chosen port.

## `auth`

| Field | Required | |
|---|---|---|
| `default_id_tag` | yes | The RFID tag used when a command names none. 1-20 printable ASCII characters without spaces. |
| `id_tags` | | A list of further tags, each valid like `default_id_tag`. |

Every tag in `default_id_tag` and `id_tags` is accepted on every charger, in `Authorize` and
`StartTransaction`. OCPP idTags are case-insensitive, so a duplicate that differs only in
case is an error.

Tags an OCPI partner vouches for are accepted too, without being listed here: the token of
an active reservation, and the token of a `START_SESSION` for 15 minutes. See
[Take commands from the partner](../guides/ocpi-commands.md#which-tags-the-station-accepts).

## `ocpi`

| Field | Required | Default | |
|---|---|---|---|
| `country_code` | yes | | Two uppercase letters, for example `DE`. |
| `party_id` | yes | | Three uppercase letters, for example `EXA`. |
| `public_base_url` | yes | | The OCPI root advertised to the partner. Its host must be reachable from the partner. Its **path** is where cpms serves: `http://192.168.1.10:8080/ocpi` serves under `/ocpi`. |
| `token_a` | yes | | Handed to the partner out of band to start [registration](../guides/ocpi-partner.md). |
| `token_c` | yes | | What the partner authenticates with once registered. |
| `business_name` | | `location.name` | The name in our OCPI credentials. At most 100 characters. |
| `push.debounce` | | `500ms` | How long to wait after an EVSE's status changes before [pushing it](../guides/ocpi-push.md). Changes inside the wait become one `PATCH`. |
| `push.max_retries` | | `5` | Retries after the first attempt, with backoff, before the change is dropped. |

Both tokens must be 8 to 64 printable ASCII characters without whitespace, and they must differ.

??? question "Why is `token_c` fixed in config?"
    OCPI implementations usually generate it during the handshake. cpms takes it from
    config so that `config.yaml` stays read-only and there is one source of truth for our
    own tokens.

## `location`

Static [OCPI Location](https://github.com/ocpi/ocpi/blob/release-2.3.0-bugfixes/mod_locations.asciidoc)
data. It is served verbatim. Only the per-EVSE status is filled in live.

| Field | Required | Default | |
|---|---|---|---|
| `id` | yes | | At most 36 characters, printable ASCII. |
| `name` | | | At most 255 characters. |
| `address` | yes | | At most 255 characters. |
| `city` | yes | | At most 45 characters. |
| `postal_code` | | | At most 10 characters. |
| `country` | yes | | ISO 3166-1 alpha-3, for example `DEU`. |
| `coordinates.latitude`, `.longitude` | yes | | Decimal degrees as strings with 5 to 7 decimal places: `"52.520000"`. |
| `time_zone` | yes | | An IANA name such as `Europe/Berlin`. The zone database is built into the binary. |
| `publish` | | `true` | OCPI's publish flag. `false` hides the location from every driver, because cpms sends no `publish_allowed_to`. |
| `evses` | yes | | At least one. |

### `location.evses[]`

| Field | Required | Default | |
|---|---|---|---|
| `uid` | yes | | The EVSE's OCPI `uid`, and what `--evse` takes. Unique, at most 36 characters, printable ASCII. |
| `evse_id` | yes | | An eMI3 EVSE ID, for example `DE*EXA*E001*1`. Unique, at most 38 ASCII characters. |
| `charger` | with several chargers | the only charger | The id of the charger this EVSE sits on. Must name a configured charger. |
| `ocpp_connector_id` | yes | | The OCPP 1.6 connector this EVSE is. `1` or greater (`0` is the station itself). Unique per charger. |
| `reservable` | | `true` | Advertises the OCPI `RESERVABLE` capability. Set it to `false` if [`cpms probe`](probe.md) reports no Reservation profile. |
| `connectors` | yes | | At least one. |

### `location.evses[].connectors[]`

| Field | Required | |
|---|---|---|
| `id` | yes | Unique within the EVSE. At most 36 characters, printable ASCII. |
| `standard` | yes | An OCPI [`ConnectorType`](https://github.com/ocpi/ocpi/blob/release-2.3.0-bugfixes/mod_locations.asciidoc), for example `IEC_62196_T2_COMBO`. |
| `format` | yes | `SOCKET` or `CABLE`. |
| `power_type` | yes | `AC_1_PHASE`, `AC_2_PHASE`, `AC_2_PHASE_SPLIT`, `AC_3_PHASE` or `DC`. |
| `max_voltage` | yes | V, greater than zero. |
| `max_amperage` | yes | A, greater than zero. |
| `max_electric_power` | | W. |

## Text limits

Everything under `location` goes out over OCPI as it is, so it must fit OCPI 2.3.0's
limits. A partner may reject a whole Location over one bad field, so
`cpms config validate` names the key that does not fit.

- Lengths count characters, not bytes: `Müller` is 6.
- Text fields take printable characters only: no newline, tab, zero-width or no-break space.
- Ids (`location.id`, `uid`, connector `id`) take printable ASCII only, so no umlauts.
- `ocpi.business_name` is sent trimmed.

## A minimal file

```yaml
charger:
  id: ALP-HYC-001
auth:
  default_id_tag: "04A1B2C3D4"
ocpi:
  country_code: DE
  party_id: EXA
  public_base_url: http://192.168.1.10:8080/ocpi
  token_a: "change-me-token-a"
  token_c: "change-me-token-c"
location:
  id: SITE-01
  address: Example Street 1
  city: Berlin
  country: DEU
  coordinates: { latitude: "52.520000", longitude: "13.405000" }
  time_zone: Europe/Berlin
  evses:
    - uid: ALP-HYC-001-1
      evse_id: DE*EXA*E001*1
      ocpp_connector_id: 1
      connectors:
        - { id: "1", standard: IEC_62196_T2_COMBO, format: CABLE, power_type: DC, max_voltage: 920, max_amperage: 500 }
```

## `state.json`

| Property | Value |
|---|---|
| Location | Next to the config file. `--state <path>` puts it elsewhere. `cpms config validate` prints which file is in use. |
| Created | On the first write, not at startup. |
| Format | Indented JSON, so `cat` works. |
| Permissions | `0600`: it holds the partner's bearer token. |
| Writes | Atomic: temp file, fsync, rename. |

### What survives a restart

| Kept | Not kept |
|---|---|
| The OCPI registration (partner token, endpoints, roles) | Reservations still waiting for the station's answer |
| **Active** reservations, with their expiry timers re-armed | Finished sessions: `state.json` is not a history |
| **Active** sessions | Meter readings |
| Charging profiles the station accepted, as sent | EVSE status (the station reports it again) |
| The id counters (reservation, transaction, remote start, profile) | |

A reservation that expired while cpms was down is dropped on load, as is a `tx` profile
whose session ended meanwhile.

### Versions

The file carries a `version`. cpms migrates older versions on load. A file it cannot parse,
or one written by a **newer** cpms, stops `cpms run` at startup with the file named, and is
never overwritten. Move it aside to start from empty state.
