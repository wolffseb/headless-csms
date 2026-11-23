# OCPI API

`cpms run` serves [OCPI 2.3.0](https://github.com/ocpi/ocpi/tree/release-2.3.0-bugfixes)
as a **CPO**, over plain HTTP on the LAN.

## Base URL

```
{ocpi.public_base_url}        e.g. http://192.168.1.10:8080/ocpi
```

The path of `public_base_url` is where cpms serves, so what is advertised and what is
served cannot disagree. There is no TLS: this is a LAN tool, by decision.

## Endpoints

| Method | Path | Token | |
|---|---|---|---|
| `GET` | [`/versions`](#list-versions) | A or C | Supported versions |
| `GET` | [`/2.3.0`](#version-details) | A or C | Modules of 2.3.0 |
| `POST` | [`/2.3.0/credentials`](#credentials) | A | Register |
| `GET` `PUT` `DELETE` | [`/2.3.0/credentials`](#credentials) | C | Read, update, unregister |
| `GET` | [`/2.3.0/locations`](#list-locations) | C | The location list |
| `GET` | [`/2.3.0/locations/{location_id}`](#get-a-location) | C | One Location |
| `GET` | [`/2.3.0/locations/{location_id}/{evse_uid}`](#get-an-evse) | C | One EVSE |
| `GET` | [`/2.3.0/locations/{location_id}/{evse_uid}/{connector_id}`](#get-an-evse) | C | One Connector |
| `POST` | [`/2.3.0/commands/{command}`](#commands) | C | `RESERVE_NOW`, `CANCEL_RESERVATION`, `START_SESSION`, `STOP_SESSION`, `UNLOCK_CONNECTOR` |

cpms also **calls the partner**: it sends every EVSE status change as a `PATCH` to the
partner's Locations receiver, and it posts the result of every command to the command's
`response_url`. See [Push status to the partner](../guides/ocpi-push.md) and
[Take commands from the partner](../guides/ocpi-commands.md).

## Authentication

Every request needs the token, **Base64-encoded**
([RFC 4648 §4](https://datatracker.ietf.org/doc/html/rfc4648#section-4), padded), as OCPI
requires since 2.2:

```
Authorization: Token <base64 of the token>
```

```sh
TOKEN_C=change-me-token-c   # ocpi.token_c from config.yaml
auth="Authorization: Token $(printf '%s' "$TOKEN_C" | base64 | tr -d '\n')"
curl -s -H "$auth" http://127.0.0.1:8080/ocpi/2.3.0/locations
```

!!! warning "Two ways to get the header wrong"
    - `echo` adds a newline that becomes part of the token. Use `printf '%s'`.
    - GNU `base64` wraps long output at 76 columns. Strip it with `tr -d '\n'`.

| State | `TOKEN_A` | `TOKEN_C` |
|---|---|---|
| Not registered | versions and credentials | HTTP 401 |
| Registered | HTTP 401 | everything |

A missing header, bad Base64, a raw unencoded token and an unknown token all get the same
HTTP 401, so the answer gives no hint which one it was. Tokens are never logged.

How a partner gets from A to C: [Register an OCPI partner](../guides/ocpi-partner.md).

## Responses

Every answer is an [OCPI envelope](https://github.com/ocpi/ocpi/blob/release-2.3.0-bugfixes/transport_and_format.asciidoc):

```json
{
  "data": { },
  "status_code": 1000,
  "status_message": "Success",
  "timestamp": "2026-11-19T09:05:28Z"
}
```

`timestamp` is UTC and ends in `Z`. Every answer carries `X-Request-ID` and
`X-Correlation-ID`: the ones the request sent, or fresh UUIDs.

### Errors

| HTTP | `status_code` | When |
|---|---|---|
| 200 | `1000` | Success |
| 400 | `2001` | JSON that does not parse, invalid credentials, a malformed `date_from`/`date_to`, a negative `offset`, a `limit` below 1 |
| 401 | `2000` | Missing or wrong token |
| 404 | `2003` | Unknown location, EVSE or connector, on a locations request |
| 200 | `2003` | A command for a location that is not ours (the `CommandResponse` is in `data`) |
| 405 | | Method not allowed in the current registration state |
| 200 | `2000` | Credentials: another registration change is in progress; retry shortly |
| 200 | `3001` | Credentials: cpms could not use the partner's API |
| 200 | `3002` | Credentials: the partner offers no OCPI 2.3.0 |

All codes: [OCPI status codes](https://github.com/ocpi/ocpi/blob/release-2.3.0-bugfixes/status_codes.asciidoc).

---

## Versions

Spec: [Version information endpoint](https://github.com/ocpi/ocpi/blob/release-2.3.0-bugfixes/version_information_endpoint.asciidoc)

### List versions

```
GET /versions
```

```json
{
  "data": [{ "version": "2.3.0", "url": "http://192.168.1.10:8080/ocpi/2.3.0" }],
  "status_code": 1000,
  "status_message": "Success",
  "timestamp": "2026-11-19T09:04:05Z"
}
```

### Version details

```
GET /2.3.0
```

```json
{
  "data": {
    "version": "2.3.0",
    "endpoints": [
      { "identifier": "credentials", "role": "SENDER", "url": "http://192.168.1.10:8080/ocpi/2.3.0/credentials" },
      { "identifier": "locations", "role": "SENDER", "url": "http://192.168.1.10:8080/ocpi/2.3.0/locations" }
    ]
  },
  "status_code": 1000,
  "status_message": "Success",
  "timestamp": "2026-11-19T09:04:05Z"
}
```

---

## Credentials

Spec: [Credentials module](https://github.com/ocpi/ocpi/blob/release-2.3.0-bugfixes/credentials.asciidoc)

```
POST   /2.3.0/credentials     register            (TOKEN_A)
GET    /2.3.0/credentials     read ours           (TOKEN_C)
PUT    /2.3.0/credentials     replace the partner's token and endpoints   (TOKEN_C)
DELETE /2.3.0/credentials     unregister          (TOKEN_C)
```

=== "Request (POST, PUT)"

    ```json
    {
      "token": "partner-token-b",
      "url": "http://emsp.example/ocpi/versions",
      "roles": [
        {
          "role": "EMSP",
          "party_id": "ABC",
          "country_code": "NL",
          "business_details": { "name": "Example eMSP" }
        }
      ]
    }
    ```

=== "Response"

    ```json
    {
      "data": {
        "token": "change-me-token-c",
        "url": "http://192.168.1.10:8080/ocpi/versions",
        "roles": [
          {
            "role": "CPO",
            "business_details": { "name": "Example Site" },
            "party_id": "EXA",
            "country_code": "DE"
          }
        ]
      },
      "status_code": 1000,
      "status_message": "Success",
      "timestamp": "2026-11-19T09:05:28Z"
    }
    ```

| Request field | Rule |
|---|---|
| `token` | 1-64 printable ASCII characters without whitespace |
| `url` | Absolute. The partner's versions endpoint |
| `roles` | At least one |

`POST` and `PUT` call the partner back during the request (each call gives up after 10 s),
and only one runs at a time. Outcomes and the deviation on `PUT`:
[Register an OCPI partner](../guides/ocpi-partner.md#what-the-partner-gets-back).

---

## Locations

Spec: [Locations module](https://github.com/ocpi/ocpi/blob/release-2.3.0-bugfixes/mod_locations.asciidoc)
(sender interface)

cpms serves the one configured Location: static data from `location:` in config, with each
EVSE's live status from the station.

### List locations

```
GET /2.3.0/locations
```

| Query parameter | Default | |
|---|---|---|
| `offset` | `0` | |
| `limit` | `100` | Maximum 100 |
| `date_from` | | Inclusive, on the Location's `last_updated` |
| `date_to` | | Exclusive |

| Response header | |
|---|---|
| `X-Total-Count` | All matching Locations |
| `X-Limit` | The limit applied, not the number returned |
| `Link: <…>; rel="next"` | Only when more pages exist, with the filters kept |

`data` is an array of Location objects.

### Get a Location

```
GET /2.3.0/locations/{location_id}
```

```json
{
  "data": {
    "country_code": "DE",
    "party_id": "EXA",
    "id": "SITE-01",
    "publish": true,
    "name": "Example Site",
    "address": "Example Street 1",
    "city": "Berlin",
    "postal_code": "10115",
    "country": "DEU",
    "coordinates": { "latitude": "52.520000", "longitude": "13.405000" },
    "evses": [
      {
        "uid": "ALP-HYC-001-1",
        "evse_id": "DE*EXA*E001*1",
        "status": "AVAILABLE",
        "capabilities": ["REMOTE_START_STOP_CAPABLE", "RESERVABLE"],
        "connectors": [
          {
            "id": "1",
            "standard": "IEC_62196_T2_COMBO",
            "format": "CABLE",
            "power_type": "DC",
            "max_voltage": 920,
            "max_amperage": 500,
            "max_electric_power": 400000,
            "last_updated": "2026-11-19T09:03:54Z"
          }
        ],
        "last_updated": "2026-11-19T09:05:26Z"
      }
    ],
    "time_zone": "Europe/Berlin",
    "last_updated": "2026-11-19T09:05:26Z"
  },
  "status_code": 1000,
  "status_message": "Success",
  "timestamp": "2026-11-19T09:05:28Z"
}
```

### Get an EVSE

```
GET /2.3.0/locations/{location_id}/{evse_uid}
GET /2.3.0/locations/{location_id}/{evse_uid}/{connector_id}
```

`data` is the EVSE or Connector object, as inside the Location above. Ids match without
regard to case, as OCPI's `CiString` requires.

### How the fields are filled

| Field | Source |
|---|---|
| `status` | The station's own status ([mapping](../concepts.md#evse-status)). `UNKNOWN` while it is offline. A station-wide fault overrides every EVSE. cpms's reservation records never override it. |
| `capabilities` | `REMOTE_START_STOP_CAPABLE` always. `RESERVABLE` unless the EVSE has `reservable: false`. `UNLOCK_CAPABLE` only when every connector is a `SOCKET` (a tethered cable has no lock to release). |
| Connector `last_updated` | The config file's modification time, read at startup. |
| EVSE `last_updated` | The later of that and its last status change. Repeats that change nothing do not move it. |
| Location `last_updated` | The latest of all. |

---

## Commands

Spec: [Commands module](https://github.com/ocpi/ocpi/blob/release-2.3.0-bugfixes/mod_commands.asciidoc).
The full story, with the results and the replace and cancel flows, is in
[Take commands from the partner](../guides/ocpi-commands.md).

```
POST /2.3.0/commands/{command}
```

`{command}` is `RESERVE_NOW`, `CANCEL_RESERVATION`, `START_SESSION`, `STOP_SESSION` or
`UNLOCK_CONNECTOR`. Every body carries a `response_url`; the rest is the spec's object for
the command. The answer is a `CommandResponse`:

```json
{
  "data": { "result": "ACCEPTED", "timeout": 65 },
  "status_code": 1000,
  "status_message": "Success",
  "timestamp": "2026-11-23T14:58:12Z"
}
```

| `result` | Meaning |
|---|---|
| `ACCEPTED` | Passed on; the outcome follows at `response_url`. `timeout` is in seconds. |
| `REJECTED` | Refused without asking the station: unknown location (with `status_code` `2003`), EVSE or connector, charger offline, a token that is not an OCPP `idTag`, an expiry less than a minute ahead. `timeout` is 0. |
| `NOT_SUPPORTED` | Unknown command type, or `RESERVE_NOW` for an EVSE with `reservable: false`. |
| `UNKNOWN_SESSION` | `STOP_SESSION` for a `session_id` that is not an active transaction. |

A body that is not valid JSON, or lacks a field the command needs, is HTTP 400 with `2001`.

---

## Limits

| | |
|---|---|
| Request body | 1 MiB |
| Unknown JSON fields | Accepted, as OCPI requires |
| Read headers / whole request | 10 s / 30 s |
| Write response | 60 s |
| Idle keep-alive | 120 s |

`cpms run --log-level debug` logs one line per request: method, path, status, duration,
request id and correlation id.
