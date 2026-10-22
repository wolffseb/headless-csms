# pr-fixer memory

Lessons from earlier fix passes in this repo. Newest last within each section. Keep generalised; prune when stale.

## Before changing code

- Check whether the comment is right before fixing it. #31's premise (rey ignores the heartbeat interval) was wrong, and the fix reopened a bug from #3. If a comment relies on third-party behaviour, check it.
- Don't keep charger state outside `core` to make a test pass (2026-10-21, #33). If a test forbids the state you need, the test's expectation is what should change: say so and relax it minimally rather than hiding state elsewhere.
- Look for an earlier deliberate decision before reversing behaviour: git blame the test, README or comment. If you find one, stop and report instead of flipping it.
- Read the orchestrator's brief against the code. If a requested outcome can't happen, implement what the state actually decides and say why. In #41, "TOKEN_A PUT during a POST → 401" was really 405, because TOKEN_A is valid while nothing is registered.
- Don't invent spec exemptions. OCPP 1.6 §3.7.1 lets a station give up on a transaction message only after repeated failures, with no exception for FormationViolation and similar codes (#42).

## Testing

- "Shown to fail" means: revert the fix (or remove the call) in place, run the test, see it fail, restore. A test that only fails to compile on the old code doesn't count.
- Async work (background cancels, timers) needs a deterministic gate in the fake (a channel the test releases) and a `Close()`/wait before asserting. Never sleep.
- A test that races a handler against a lock must wait for the handler's own progress, e.g. a wrapped `r.Body` that signals on first Read, behind a scratch `httptest.Server`. A "wait until it should be there" step is vacuous (#41). Prove it with a mutant: move the lock, run `-count=5`.
- For races between an auth decision and a handler, wrap the `ocpi.Authenticator` in a gate that signals after deciding, then blocks on a channel (#41).
- A mutex held across an outbound call makes tests that queue behind it hang on the old code. Give every wait a failure deadline (`select` with `time.After`) so the old code fails quickly instead of hanging.
- A new "in progress, retry" answer changes concurrent-request tests: count by OCPI `status_code`, not by HTTP status.
- A race that depends on fake-clock ordering can't be guarded by a test. Say so in the report instead of adding a flaky one (#42).
- When an owner changes the default of an optional bool, use a `*bool` with an `IsX()` accessor (like `Reservable`/`IsReservable`), because a plain bool can't tell unset from false. To show the default test fails first, add the accessor with the old behaviour, watch the test fail, then flip it.
- Simulator scenario `slow` delays every answer, including the cancel. Assert on the run log or core state, not on the station receiving it.
- `goleak.VerifyNone` with `IgnoreCurrent()` in non-parallel tests. Background goroutines must finish by cleanup.
- Clocktest timers: a test that advances the clock right after a "re-arm" must first see the re-armed delay (record the last `d` in the AfterFunc wrapper in an atomic), because `Pending()==1` holds both before and after a stop-and-re-arm.
- Config-change wake-ups: one closed-and-replaced channel per setting (not one shared), read together with the value under `s.mu`, so a loop never misses a change and unrelated changes don't reset its timer.
- Give the heartbeat its own `HeartbeatAfterFunc`; sharing `AfterFunc` would break the meter tests' `Pending()` counts.
- A placeholder that leaks is cheapest to fix at the public accessor: mask it, keep the internal bookkeeping (#42 `TransactionID()`).
- A message that is "queued" and a caller that has "returned" can disagree. Read the outcome while still holding the lock that serialises the queue, and mark it detached under the state mutex (#42).
- HTTP servers: `httptest.NewServer(handler)` ignores `BaseContext`, so join the lifetime context in `ServeHTTP` too. Give each listener its own shutdown grace context, created after anything that may block before it (#36).
- Unanchored `.gitignore` rules (e.g. `config.yaml`) also hide test fixtures, so CI fails while local runs pass on untracked files. After any ignore-rule change, run `git check-ignore -v` on the fixture paths (#40).

## Known flakes (rerun, don't fix in unrelated PRs)

- `TestShutdownWaitsForTheOrphanCancel` (#43): two 1 s timers race. Rerun the failed job.
- `TestSimulateChargerAgainstRun` under full-suite load.
- "address already in use" when other agents run `cli` tests on fixed ports in parallel: retry.

## Working with stacked/parallel PRs

- Pushing to a branch another worktree has checked out moves that worktree's ref, and its index then looks dirty. Harmless, but don't commit from a stale worktree.
- When told another agent owns a file, don't touch it; put the needed change under "Found, not fixed" with exact wording.
- One commit per comment, unless two fixes share one rewrite and splitting would leave a broken intermediate commit. Then say so in the replies.

## Tooling

- golangci-lint v2.6.2 in `~/go/bin` (not on PATH) fails under Go 1.27 with "export data version" errors. Run `GOTOOLCHAIN=go1.26.0 ~/go/bin/golangci-lint run ./...`. GitHub CI is the authority.
- The worktree sandbox refuses compound commands: heredoc plus another command, `&&`/`;` chains with git or gh, `$VAR`- or `$(...)`-built commands, `for` loops reaching git/gh, `python3` with computed paths, and `sleep` before a command. Use one plain command per call, absolute literal paths, and the Write/Edit tools for files. Give scratchpad scripts unique names, because other runs share the scratchpad.
- `gh api` takes no `-R`: put the repo in the path. To wait for CI, use `gh run watch <id> --exit-status` (there is no `timeout` on macOS).
- BSD `sed -i` needs `''` and can't take `\n` in replacements. Use the Edit tool for multi-line changes and for Go code containing `\n`.
- Stop a background `cpms run` by its PID (`kill -INT`), not `pkill -f`, which also hits other agents' runs.
