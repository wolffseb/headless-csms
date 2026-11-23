# Quickstart

Run a CSMS, connect a simulated charging station, and take it through
reserve → charge → stop. About five minutes, no hardware.

**You need:** [Go](https://go.dev/dl/) 1.24 or newer, `make` and `git`.

## 1. Build

```sh
git clone https://github.com/wolffseb/headless-csms.git
cd headless-csms
make build        # -> bin/cpms
```

## 2. Configure

The example config works as is.

```sh
cp config.example.yaml config.yaml
./bin/cpms config validate
```

```
config.yaml is valid.

  Charge point        ALP-HYC-001  (OCPP 1.6)  at 192.168.1.42
  OCPP listener       0.0.0.0:9000
  Station CSMS URL    ws://<this-host>:9000/ocpp/ALP-HYC-001
  ...
  EVSE           EVSE ID        OCPP CONNECTOR  CONNECTORS
  ALP-HYC-001-1  DE*EXA*E001*1  1               IEC_62196_T2_COMBO/CABLE/DC 920V 500A
  ALP-HYC-001-2  DE*EXA*E001*2  2               IEC_62196_T2_COMBO/CABLE/DC 920V 500A
```

!!! tip
    Every command reads `config.yaml` in the current directory. Use `-c <path>` for another file.

## 3. Start the CSMS

```sh
./bin/cpms run
```

```
cpms listening for ALP-HYC-001 on ws://127.0.0.1:9000/ocpp/ALP-HYC-001
OCPI versions endpoint http://192.168.1.10:8080/ocpi/versions (listening on 0.0.0.0:8080)
control socket cpms.sock
waiting for the charge point to connect; press Ctrl-C to stop
```

Leave it running. In a terminal it shows the [terminal UI](guides/terminal-ui.md)
(the startup lines above are its first log lines); with `--headless`, or when stdout is
not a terminal, it prints them and then logs everything the station does.

## 4. Connect a station

In a **second terminal**, start the simulator. It reads the same config and dials the CSMS.

```sh
./bin/cpms simulate charger --meter-interval 5s
```

The first terminal shows it arrive:

```
INFO msg="charge point connected" charge_point=ALP-HYC-001 version=1.6
INFO msg="ALP-HYC-001 booted (Alpitronic HYC300 fw simulator)"
INFO msg="ALP-HYC-001-1 UNKNOWN→AVAILABLE"
INFO msg="ALP-HYC-001-2 UNKNOWN→AVAILABLE"
```

## 5. Drive it

In a **third terminal**:

=== "Reserve"

    ```console
    $ ./bin/cpms reserve --evse ALP-HYC-001-1
    ALP-HYC-001-1 reserved for 04A1B2C3D4 until 10:18:57 (reservation 1)

    $ ./bin/cpms status
    ALP-HYC-001  online  Alpitronic HYC300 fw simulator  last seen 10:03:57

    EVSE           STATUS     RESERVATION                   TRANSACTION  METER
    ALP-HYC-001-1  RESERVED   #1 04A1B2C3D4 until 10:18:57  -            -
    ALP-HYC-001-2  AVAILABLE  -                             -            -

    OCPI  not registered
    ```

=== "Charge"

    ```console
    $ ./bin/cpms start --evse ALP-HYC-001-1
    ALP-HYC-001-1 remote start accepted

    $ ./bin/cpms status
    ALP-HYC-001  online  Alpitronic HYC300 fw simulator  last seen 10:04:03

    EVSE           STATUS     RESERVATION  TRANSACTION                  METER
    ALP-HYC-001-1  CHARGING   -            1 04A1B2C3D4 since 10:03:59  50.0 kW 125.0 A, 1s ago
    ALP-HYC-001-2  AVAILABLE  -            -                            -
    ```

    The session used the tag the EVSE was reserved for, so it redeemed the reservation.

=== "Stop"

    ```console
    $ ./bin/cpms stop --evse ALP-HYC-001-1
    ALP-HYC-001-1 remote stop accepted
    ```

## 6. Call the OCPI API

`cpms run` also serves OCPI. Tokens are sent Base64-encoded:

```sh
TOKEN_A=change-me-token-a   # ocpi.token_a from config.yaml
curl -s -H "Authorization: Token $(printf '%s' "$TOKEN_A" | base64 | tr -d '\n')" \
  http://127.0.0.1:8080/ocpi/versions
```

```json
{
  "data": [{ "version": "2.3.0", "url": "http://192.168.1.10:8080/ocpi/2.3.0" }],
  "status_code": 1000,
  "status_message": "Success",
  "timestamp": "2026-11-19T09:04:05Z"
}
```

## Next steps

| I want to... | Read |
|---|---|
| Understand what just happened | [Concepts](concepts.md) |
| Use a real charger | [Connect a real station](guides/connect-station.md) |
| Make the simulator misbehave | [Test with the simulator](guides/simulator.md) |
| Cap the charging power | [Limit charging power](guides/smart-charging.md) |
| Let a backend read my location | [Register an OCPI partner](guides/ocpi-partner.md) |
| Script against `cpms` | [CLI reference](reference/cli.md): `--json` and stable exit codes |
