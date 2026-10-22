# issue-implementer memory

Lessons from implementing roadmap issues in this repo. Newest last. Keep generalised; prune when stale.

## Conventions you'd otherwise rediscover

- Issue comments override the body. Plan-update comments on #5, #11, #13, #14 and #16 (2026-10-21) changed dependencies and added rules.
- `gh` needs `-R wolffseb/headless-csms` in this checkout.
- Tests through `cpms run` use the issue's fixed port range, because config rejects port 0. Since #36, `runConfigPorts` lets a test set the OCPP and OCPI ports separately. Existing tests put OCPI on OCPP port + 1000 (20100–20399).
- `cpms run` takes an exclusive lock on `<state>.lock` (#27), so a test must never start two runs on one state file.
- `state.json` schema changes bump `state.Version` and need a migration (decision on #8). Avoid them unless the issue requires one, and say so loudly.
- OCPI seams from #12/#36: `ocpi.Options.Auth` is an `Authenticator` interface (`ocpi.AuthenticatorFunc` for test stubs); modules mount via `Server.Register(Module{...})`; `ocpi.NewSecret(tok).Matches` for constant-time compare; access levels are ordered none < registration < modules.
- OCPI auth header: strict standard padded Base64 of the token. The spec's own example decodes with a trailing newline (made with `echo`), so use `printf '%s'` in docs.
- golangci-lint in `~/go/bin` may have been built with an older Go and crash; reinstall it under the current Go before `make ci`.

## Process that worked

- Script the fail-without checks: apply one breaking change to production code, run the matching test, restore. List them in the PR as a table (#36 did 16).
- Finish with the real binary on spare ports, paste the responses into the PR, and check that the ports are free after Ctrl-C.
