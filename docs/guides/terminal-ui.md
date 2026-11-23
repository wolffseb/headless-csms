# Use the terminal UI

`cpms run` shows the stations on one screen and lets you act on them with the keyboard,
so a second terminal is not needed for the everyday commands. The one-shot commands
(`cpms reserve`, `cpms status` and the rest) still work, for scripts and for what the
keys do not cover.

## What you see

```text
cpms · 1/2 chargers online · OCPI not registered
────────────────────────────────────────────────────────────────────────────────────────────────────
ALP-HYC-001  online  Alpitronic HYC300 fw 1.4.2
     EVSE           STATUS       DETAIL
 ▸   ALP-HYC-001-1  AVAILABLE    -
     ALP-HYC-001-2  CHARGING     session 12 · 04A1B2C3D4 · since 09:31 · 48.2 kW
ALP-HYC-002  offline  last seen 09:30:00 (heartbeat timeout)
     ALP-HYC-002-1  UNKNOWN      -
     ALP-HYC-002-2  UNKNOWN      -
── log ─────────────────────────────────────────────────────────────────────────────────────────────




09:30:00 ALP-HYC-001 connected
09:30:00 ALP-HYC-001 booted (Alpitronic HYC300 fw 1.4.2)
09:30:00 ALP-HYC-001-1 UNKNOWN→AVAILABLE
09:30:00 ALP-HYC-001-2 UNKNOWN→AVAILABLE
09:30:00 ALP-HYC-002 connected
09:30:00 ALP-HYC-002 booted (Alpitronic HYC300 fw 1.4.2)
09:30:00 ALP-HYC-002-1 UNKNOWN→AVAILABLE
09:30:00 ALP-HYC-002-2 UNKNOWN→AVAILABLE
09:31:00 ALP-HYC-002 disconnected: heartbeat timeout
09:31:00 ALP-HYC-002-1 AVAILABLE→UNKNOWN
09:31:00 ALP-HYC-002-2 AVAILABLE→UNKNOWN
09:31:00 ALP-HYC-001 transaction 12 started on ALP-HYC-001-2 (tag 04A1B2C3D4)
09:31:00 ALP-HYC-001-2 AVAILABLE→CHARGING


↑↓ select  r reserve  c cancel  s start  x stop  u unlock  t refresh  ? help  q quit
```

- **Header.** How many chargers are online, and the OCPI side: `OCPI not registered`, or
  `registered with DE*ABC` followed by the status push (`last push 10:14:55`,
  `nothing pushed yet`, or `last push failed 10:14:55: 503 Service Unavailable` in red).
  The push status is read once a second.
- **One group per charger.** The charger line shows `online` with the station's vendor,
  model and firmware, or `offline` with when it was last seen and why it went. An offline
  group is dimmed; the keys still work and fail with the same sentence the CLI gives.
- **One row per EVSE.** `▸` marks the selected one. STATUS is the word `cpms status`
  prints, with the OCPP error code after it if there is one. DETAIL is the reservation
  (`#7 04A1B2C3D4 until 10:30`, with `(pending)` or `(cancelling)` while it is not settled)
  or the session (`session 12 · 04A1B2C3D4 · since 09:51 · 48.2 kW`).
- **Log.** The last lines that fit, newest at the bottom: every core event except
  heartbeats and meter values, the result of each key, and warnings from the logger.
  There is no scrolling; the full log is in a file (below).
- **Message line.** The latest result, above the key hints: `✓` in green, `✗` in red with
  the CLI's wording. It stays until the next result or `esc`.

Below 60 columns the layout changes: the header splits into two lines, each EVSE's detail
moves to a line of its own, and the kW figure is left out.

## Keys

The keys act on the selected EVSE. There are no prompts: they use the default tag
(`auth.default_id_tag`) and the default reservation length of 15 minutes.

| Key | Does |
|---|---|
| `↑` `↓` or `k` `j` | Select an EVSE. The selection stops at the first and last one. |
| `r` | Reserve the EVSE for the default tag for 15 minutes. |
| `c` | Cancel its reservation. |
| `s` | Start a session with the default tag. |
| `x` | Stop its session. It asks first: `y` confirms, any other key cancels. |
| `u` | Unlock the cable. |
| `t` | Ask the station for the EVSE's status again. |
| `?` | Show the key list; `?` or `esc` closes it. |
| `q`, `ctrl-c` | Quit, with no confirmation. |

While a command waits for the station, its row shows a spinner and the message line says
`reserve ALP-HYC-001-1: waiting for the station…`. Pressing a key for the same EVSE meanwhile sends
nothing and says so; the other EVSEs stay usable.

For another tag, another expiry or a charging limit, run the CLI in another terminal:

```sh
cpms reserve --evse ALP-HYC-001-1 --tag 04FFEE0011 --expires 30m
```

## Quitting

`q` and `ctrl-c` end `cpms run` exactly as `SIGINT` does: the OCPI and OCPP listeners, the
control socket and the status push are closed within the usual grace period. `SIGTERM`
works too. The screen is restored first, and `shutting down` is printed after it.

## Where the log goes

While the UI is up, nothing is written to stderr, because it would tear the screen. The
log goes to **`cpms.log` next to the state file**, appended to at `--log-level` (events and the
startup lines included). Warnings and errors are mirrored into the log pane whatever the level.

```sh
tail -f cpms.log
```

## Without the UI

`cpms run --headless` prints log lines to stderr, as before the UI existed. `cpms run` does
the same, after printing `stdout is not a terminal; running headless`, when its stdout is
not a terminal (redirected to a file, under systemd, in CI). The same happens when stdin is
not a terminal, since there would be no keys to read. In both cases `cpms.log` is not written.

```sh
cpms run --headless 2> cpms.log
```
