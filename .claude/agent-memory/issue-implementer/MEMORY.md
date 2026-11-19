# issue-implementer memory

Lessons from implementing roadmap issues in this repo. Newest last within each section. Keep generalised; prune when stale.

## Conventions you'd otherwise rediscover

- Issue comments override the body. Plan-update and decision comments live on #5, #11, #13, #14, #16, #34 and #39.
- `gh` needs `-R wolffseb/headless-csms` in this checkout; `gh api` takes the repo in the path instead. `gh issue view` can print nothing at all even with `-R` (2026-11-16): read issues with `gh api repos/wolffseb/headless-csms/issues/<n>` and `/comments`.
- Tests through `cpms run` use the issue's fixed port range, because config rejects port 0. `runConfigPorts` sets the OCPP and OCPI ports separately. `newStation` always puts OCPI on OCPP port + 1000 (20100–20399), so build `station{...}` by hand for a test that needs its own OCPI port.
- `cpms run` takes an exclusive lock on `<state>.lock` (#27), so a test must never start two runs on one state file.
- `state.json` schema changes bump `state.Version` and need a migration (decision on #8). Avoid them unless the issue requires one, and flag it as a 🔴 trigger.
- A new required config key has to go into every inline YAML fixture: `config.example.yaml`, `internal/config/testdata/*.yaml`, `baseYAML`, `runConfigPorts` in cli/run_test.go, and cli_test.go.
- The status golden file is regenerated with `go test ./internal/cli -run TestStatusJSONGolden -update`. Call the change out in the PR.
- When the owner asks you to "consider" a feature, implement the small part, park the rest in `docs/IDEAS.md`, and say which went where in the PR.
- Port ranges named in an issue may already be partly taken by merged tests (2026-11-16: #53 got 19600-19609, but `TestProbeWithoutSmartCharging` holds 19600). `grep -rn 196 internal --include='*_test.go'` before picking; an "address already in use" that repeats inside one `go test ./internal/cli` run is a collision, not another agent.

## Seams later issues build on

- OCPI foundation (#12/#36):
  - `ocpi.Options.Auth` is an `Authenticator` interface; `ocpi.AuthenticatorFunc` makes test stubs a one-liner. Access levels are ordered none < registration < modules.
  - Modules mount through `Server.Register(Module{...})`.
  - Helpers: `WriteOK`, `WriteError`, `WriteUnauthorized`, `DecodeJSON`, `FormatDateTime`, `NewID`.
  - The request context carries `RequestID`/`CorrelationID`, plus `TokenFromContext` since #41. `Server.Context()` is the lifetime context, cancelled on Shutdown.
  - In `internal/cli/run.go`, `startOCPI(d ocpiDeps)`: add fields, never change the signature.
- Credentials (#13/#41):
  - `credentials.Registrar` has `Authenticator()` and `Mount(server)`, wired via `ocpiDeps.Credentials`.
  - `Registrar.Client()` sends the stored TOKEN_B; #15/#16 use it.
  - `credentials.PushEndpoint(reg)` finds the locations RECEIVER.
  - The outbound client propagates `ocpi.CorrelationID(ctx)`.
- Locations (#14/#40): `locations.Build(cfg, staticAt, snap)` is pure and #15 can reuse it. Also `locations.DateTime`, `config.Config.ModTime`, `core.EVSESnapshot.LastUpdated`, `ocpiDeps.Core`.
- Simulator (#34/#37, #39/#42):
  - Injected clocks: `Options.AfterFunc`, `HeartbeatAfterFunc` and `ReconnectAfterFunc`, all driven with `internal/clocktest`.
  - Every simulator CALL goes through one in-flight slot (`callMu`).
  - Test seams:
    - the fake CSMS `startConf func(n int) v16.StartTransactionConf` hook, set under `f.mu`;
    - `sim.HandleCall(ctx, "", v16.ActionReserveNow, payload)` to reserve without core;
    - `offlineSession`/`transactionIDsOnTheWire` helpers in offline_test.go;
    - the cuttable TCP proxy in `internal/ocpptest/proxy.go`.
  - Capture logs with a mutex-guarded `strings.Builder` as `Options.Log`.
- Probe (#10, #47, 2026-11-16): a new capability needs `ocpp.Cap*` + `AllCapabilities()` (append last), a `neededProfiles` grant, a `Capabilities` field, and maybe a `needs` entry in cli/probe.go (only if cpms uses it). New simulator config keys change the raw-key count in `TestProbeJSONAndRaw` and both probe goldens (`go test ./internal/cli -run 'TestProbeNormalStation|TestProbeJSONAndRaw' -update`). Changing which keys `Capabilities()` asks for changes `testdata/golden/GetConfiguration.json` (`go test ./internal/ocpp/v16 -run Golden -update`).
- Probe (#10, #47, 2026-11-16): a new capability needs `ocpp.Cap*` + `AllCapabilities()` (append last), a `neededProfiles` grant, a `Capabilities` field, and maybe a `needs` entry in cli/probe.go (only if cpms uses it). New simulator config keys change the raw-key count in `TestProbeJSONAndRaw` and both probe goldens (`go test ./internal/cli -run 'TestProbeNormalStation|TestProbeJSONAndRaw' -update`).
- Smart charging C1 (#49, 2026-11-16): `ocpp.ChargePoint` has `SetChargingProfile`/`ClearChargingProfile`/`GetCompositeSchedule`. A new `ChargePoint` method needs stubs in the `fakeStation`s of `control/control_test.go` and `control/socket/server_test.go`. The v16 e2e `simRig.power` (a `powerLog` wrapping the handler) is how a test sees streamed W, since core records no power. Simulator tests can drive an unconnected sim with `offlineSim` + `handle[T]` (smartcharging_test.go). Meter cap: `meter.capW/capped`, refreshed in `sendMeterValues`.
- Meter readings and station config (#48, 2026-11-16): `core.MeterReading` (power, current, offered, energy) is in `EVSESnapshot.Reading`, in memory only, and dropped on stop or disconnect. The v16 extraction is `meterReading` in `v16/meter.go`. `ocpp.ConfigurationChanger` is an optional interface beside `ocpp.ChargePoint` (1.6 only), behind `control.ChangeConfiguration` and socket op `config_set`.
- Connect hook (#53, 2026-11-16): `csms.Options.OnConnect(ctx, cp)` runs per connection under `s.wg`, cp bound to that connection, ctx ends with it. `core.BootedSinceConnect` tells a resumed station from a booting one. `control.Options.AfterFunc`/`BootGrace` put the grace on a `clocktest` clock; `simRig.grace` and `simRig.hooksDone()` in control/e2e_test.go. Simulator `ResumeWithoutBoot` redials without boot or status round.

- Smart charging C2 (#50, 2026-11-16): core keeps profile records (`RecordChargingProfile`/`ForgetChargingProfiles`/`ChargingProfiles`, Tx dropped on StopTransaction); control has `SetLimit`/`SetProfile`/`ClearProfiles`/`Schedule`; socket `classifyProfile` maps "cannot do that" to `not_supported` (exit 11) only for limit/profile ops, keeping unlock's mapping. Purposes are kebab-case (`tx-default`) in CLI and JSON via `socket.ParsePurpose`/`PurposeName`. state.json is version 2; `parse` accepts 1..Version.
- Station meter (#64, 2026-11-16): connector 0 never maps to an EVSE. `core.ChargePointSnapshot.StationReading` holds its MeterValues; `socket.MeterStatus` is the reading shape, embedded in `EVSEStatus` (flat JSON) and as `ChargePointStatus.Meter`. `meterCell` takes a `MeterStatus`. Any new per-connector handler in core must branch on connector 0 like `SetConnectorStatus`/`RecordMeterValues`, or `evseUID` invents `<cp>-0`. Raw OCPP through `cpms run` in a cli test: `ocpptest.MustDial(t, "ws://127.0.0.1:<port>/ocpp/ALP-HYC-001")` on a `newStation` without `connect()`.
- A test that needs a field from an unmerged sibling PR can gate on `reflect.TypeOf(T{}).FieldByName(...)` and decode the raw JSON itself: it compiles on both bases and turns strict by itself after the rebase. Prove the gated part in a scratch worktree with the sibling merged (never pushed).
- One session per connector (#73, 2026-11-18): `core.StartTransaction` ends any other active tx on the same EVSE (`ReasonSuperseded` = "Other", via `endTransactionLocked`). A test that needs two active on one EVSE must seed state.json: control's `newRigOn(t, store)` + `twoActiveOnEVSE1(t)`. `control.Stop(ctx, evse, txID)` and `transactionOn(evse, txID)` pick a session; socket `Request.TransactionID` / `LimitRequest.TransactionID`.

## OCPI/OCPP traps

- OCPI auth header: strict standard padded Base64. The spec's own example decodes with a trailing newline, and GNU `base64` wraps at 76 columns. Use `printf '%s' tok | base64 | tr -d '\n'` in docs.
- OCPI date filters must compare the real timestamp, not the second-truncated rendering, or a fractional `date_from` loses changes made in the same second.
- OCPI `publish: false` means "only for tokens in `publish_allowed_to`". cpms defaults to true (#14 decision).
- OCPP idTags (IdToken) compare case-insensitively (§7.15). Use `strings.EqualFold`.
- The CSMS gaps in handling replayed offline transactions are tracked in #44. Don't work around them in the simulator.
- OCPI `string(n)` limits count characters: use `utf8.RuneCountInString`, and test the edge with umlauts (2026-11-16, #55). Validate derived values (e.g. `OCPIBusinessName()` falling back to `location.name`) and name the key the value came from.
- OCPP 1.6 `SupportedFeatureProfiles` can be truncated at `SupportedFeatureProfilesMaxLength` (Alpitronic HYC50 drops Reservation). Since #52 a profile missing from a full list is unknown with an `ocpp.Feature.Note`; the simulator reports max length = its profile count, so a same-length `FeatureProfiles` override reproduces it (2026-11-16).
- OCPP 1.6 JSON schemas: the OCA zip is not on GitHub; mobilityhouse/ocpp vendors them at `ocpp/v16/schemas/<Action>.json` / `<Action>Response.json`. ClearChargingProfile's `Unknown` is a defined status, not "out of spec". `encoding/json` writes float64 without an exponent below 1e21, so a `limit` needs no custom marshaller.
- The simulator reports `MeterValuesSampledData` as read-only and answers `Rejected` to it. Use `ConnectionTimeOut` or `HeartbeatInterval` when a test needs an `Accepted` ChangeConfiguration.
- Core keeps each EVSE's last *reported* status through a disconnect and restores it on reconnect (`recompute`), so in-process the EVSEs are stale, not UNKNOWN; UNKNOWN only after a cpms restart. Write reconnect tests for the case you mean. OCPP 1.6 spec PDF: the OCA zip (openchargealliance.org/download-ocpp) has `ocpp-1.6 edition 2.pdf`; `pdftotext -layout` reads it.
- Core profile records replace on same EVSE + purpose + stack level, like the station does, so two test profiles on one EVSE need different `StackLevel`s or the first silently vanishes (2026-11-18).

## Process that worked

- Script the fail-without checks: apply one breaking change to production code, run the matching test, restore. List them in the PR as a table (#36 did 16, #40 did 33). Mutations that survive point at weak tests, so fix the tests.
- Bound how long a fake peer can block, so a mutation fails instead of hanging.
- Finish with the real binary on spare ports, paste the responses into the PR, and check that the ports are free after Ctrl-C. Stop it by PID (`kill -INT`), not `pkill -f`.
- CLI tests capture output with `SetOut`, which hides that cobra's `cmd.Println` goes to stderr in the real binary (2026-11-16: `cpms probe --json | jq` gets nothing). Pipe stdout of the real binary when checking scriptable output.
- Since #54 (2026-11-16): reports use `fmt.Fprint*(cmd.OutOrStdout(), ...)`, never `cmd.Print*` (`TestNoCobraPrint` fails on it). `TestReportsGoToStdout` in cli/binary_test.go builds the binary and checks both streams; add a line there for a new report command.
- For a CLI path the simulator cannot produce (such as a `RebootRequired` answer), a fake `cpms run` works: a unix listener in `shortDir` that reads one line and writes a canned `{"ok":true,"result":...}`. Pass it with `--socket`.
- `bodyclose` flags test helpers that return `*http.Response`, even when they close the body. Return status, headers and the decoded body instead.
- A Python mutation table (name, file, old, new, pkg, -run) applied one at a time with restore in `finally` makes the fail-without proof cheap; keep the script in the scratchpad.
- A counter of finished hooks must count every connection's: the first connection's hook also finishes (cancelled) on a reconnect, so wait for n == 2, not 1. A fake-clock test that a timer has not fired yet should assert `clk.Pending()`, not only the absence of its effect, or a shortened timer survives mutation.
- A station that forgets its session across a cpms restart, in a cli test: `s.sim.Close(); s.sim.Wait(); s.stopRun(); s.startRun(); s.connect()` (a fresh sim). A sim with `Reconnect` keeps its session instead. With the real binary: `kill -9` the simulator. Build the pre-change binary from a detached `git worktree add` in the scratchpad for the "before" run (2026-11-18).
- Assert a reason/constant against its literal value, not the exported const, or a mutation of the const survives (2026-11-18).
- Flaky test that will not reproduce even under CPU load (#68, 2026-11-19): read the CI log for the exact failure shape, then widen the suspected window with a throwaway `time.Sleep` in production code (revert with `git checkout <file>`) and run 10x. If the old test fails with the CI signature and the fixed one stays green with the sleep in, the cause is proven. Under 8 busy loops 150 runs still passed, so load alone is not proof.
- Waiting on state is not waiting on an event: core restores EVSE statuses in `Connected`, so `IsOnline && status==Available` is true before `Booted`. Wait for the `core.EventBootNotified` event (or `BootedSinceConnect`). And `cpms run` logs a status before it answers the call: a test that kills it on that log line can cut the station's first connection mid-`Connect`, which ends the simulator (no redial for a first connect). The sim logs `msg=online` once `Connect` is done; wait for that before killing the CSMS.

## Tooling

- golangci-lint v2.6.2 in `~/go/bin` (not on PATH) fails under Go 1.27. Run `GOTOOLCHAIN=go1.26.0 ~/go/bin/golangci-lint run ./...`. GitHub CI is the authority.
- The worktree sandbox refuses compound commands (heredocs mixed with other commands, `$(...)`/`$VAR`-built commands, `for` loops, `&&`/`;` chains with git/gh, pipes into `base64`). Use one plain command per call and the Write/Edit tools for files. Run scratchpad scripts as `bash <path>`, and give them unique names.
- BSD sed has no `\|`, and `sed -i '' -n '/a\|b/p'` silently empties the file. Prefer the Edit tool.
- Two timers with the same duration race, and Go's select picks at random when both are ready (#43, 2026-11-16). A backstop context around `Conn.Call` needs slack past `call_timeout`, or the log flips between "no answer within" and "context deadline exceeded". Prove a flake with `-race -count=50 -cpu 1,2,4` on a clean `origin/main` worktree, and run it before and after the fix one after the other, because both use the same test port.
