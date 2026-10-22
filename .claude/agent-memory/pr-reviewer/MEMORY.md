# pr-reviewer memory

Lessons from earlier reviews in this repo. Newest last. Keep generalised; prune when stale.

## Bugs this codebase is prone to

- **Semantic conflicts between PRs that are each green on their own** (2026-10-21). #29 made Reserve wait one extra call_timeout, and #27's CLI test asserted about one call_timeout. Both were green alone, and `main` went red after merging. When PRs touch the same behaviour, test them merged (`git merge` in a scratch worktree, `go test -race ./...`) and report it.
- **State outside `core`** (2026-10-21). Fixes tend to add a map or flag in `control` (e.g. a per-EVSE "blocked" set). The #5 rule is that only `core` holds mutable charger state. Flag it, and propose a visible core state (#33 ended up with `cancelling`).
- **Orphans after a CALL was sent** (2026-10-21). A timeout or a dropped connection after ReserveNow went out means the station may hold it. Check what happens locally *and* at the station, including the window before the cancel arrives, the "connection already closed" case, and id reuse (#16 replaces reservations under the same OCPP id).
- **Goroutine lifecycles behind a WaitGroup** (2026-10-21). Check that `Add` can't happen after `Wait` started (a `closed` flag under the same lock), and that shutdown order keeps the connection alive for in-flight work.
- **Unknown vs. absent** (2026-10-21). Probe/config readers conflate "not reported" or "empty" with "no". OCPP 1.6 Core is mandatory, so assume it when not reported.
- **Overloading a field** (e.g. packing a CALLERROR code into `Status`). Suggest a separate field.

## Checks that paid off

- Break the code in a scratch worktree (revert the fix, remove a call) and confirm a test fails. "The new tests didn't compile on the old code" is not proof.
- Verify third-party behaviour at the source. rey's "300 s heartbeat" was a stale config key (`HeartbeatInterval` hard-coded); on the wire it used the BootNotification interval. #31 was built on the wrong premise and closed.
- For spec claims, quote the spec text (OCPP 1.6 transcriptions in SAP/e-mobility-charging-stations-simulator `docs/ocpp16/`; OCPI 2.3.0 at github.com/ocpi/ocpi release-2.3.0-bugfixes).
- Check that the PR doesn't reverse a recorded decision (git blame the test or README line). If it does, the light is at least yellow, and the owner decides.

## False alarms to avoid

- `TestSimulateChargerAgainstRun` can flake under full-suite load (shutdown races the simulator's status burst). It passes in isolation and isn't caused by unrelated PRs.
- "address already in use" when other agents run `cli` tests on fixed ports in parallel: retry, don't report.
