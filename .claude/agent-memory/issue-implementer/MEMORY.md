# issue-implementer memory

Lessons from implementing roadmap issues in this repo. Newest last within each section. Keep generalised; prune when stale.

## Conventions you'd otherwise rediscover

- Issue comments override the body. Plan-update and decision comments live on #5, #11, #13, #14, #16, #34 and #39.
- `gh` needs `-R wolffseb/headless-csms` in this checkout; `gh api` takes the repo in the path instead.
- Tests through `cpms run` use the issue's fixed port range, because config rejects port 0. `runConfigPorts` sets the OCPP and OCPI ports separately. `newStation` always puts OCPI on OCPP port + 1000 (20100–20399), so build `station{...}` by hand for a test that needs its own OCPI port.
- `cpms run` takes an exclusive lock on `<state>.lock` (#27), so a test must never start two runs on one state file.
- `state.json` schema changes bump `state.Version` and need a migration (decision on #8). Avoid them unless the issue requires one, and flag it as a 🔴 trigger.
- A new required config key has to go into every inline YAML fixture: `config.example.yaml`, `internal/config/testdata/*.yaml`, `baseYAML`, `runConfigPorts` in cli/run_test.go, and cli_test.go.
- The status golden file is regenerated with `go test ./internal/cli -run TestStatusJSONGolden -update`. Call the change out in the PR.
- When the owner asks you to "consider" a feature, implement the small part, park the rest in `docs/IDEAS.md`, and say which went where in the PR.

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

## OCPI/OCPP traps

- OCPI auth header: strict standard padded Base64. The spec's own example decodes with a trailing newline, and GNU `base64` wraps at 76 columns. Use `printf '%s' tok | base64 | tr -d '\n'` in docs.
- OCPI date filters must compare the real timestamp, not the second-truncated rendering, or a fractional `date_from` loses changes made in the same second.
- OCPI `publish: false` means "only for tokens in `publish_allowed_to`". cpms defaults to true (#14 decision).
- OCPP idTags (IdToken) compare case-insensitively (§7.15). Use `strings.EqualFold`.
- The CSMS gaps in handling replayed offline transactions are tracked in #44. Don't work around them in the simulator.

## Process that worked

- Script the fail-without checks: apply one breaking change to production code, run the matching test, restore. List them in the PR as a table (#36 did 16, #40 did 33). Mutations that survive point at weak tests, so fix the tests.
- Bound how long a fake peer can block, so a mutation fails instead of hanging.
- Finish with the real binary on spare ports, paste the responses into the PR, and check that the ports are free after Ctrl-C. Stop it by PID (`kill -INT`), not `pkill -f`.
- `bodyclose` flags test helpers that return `*http.Response`, even when they close the body. Return status, headers and the decoded body instead.

## Tooling

- golangci-lint v2.6.2 in `~/go/bin` (not on PATH) fails under Go 1.27. Run `GOTOOLCHAIN=go1.26.0 ~/go/bin/golangci-lint run ./...`. GitHub CI is the authority.
- The worktree sandbox refuses compound commands (heredocs mixed with other commands, `$(...)`/`$VAR`-built commands, `for` loops, `&&`/`;` chains with git/gh, pipes into `base64`). Use one plain command per call and the Write/Edit tools for files. Run scratchpad scripts as `bash <path>`, and give them unique names.
- BSD sed has no `\|`, and `sed -i '' -n '/a\|b/p'` silently empties the file. Prefer the Edit tool.
