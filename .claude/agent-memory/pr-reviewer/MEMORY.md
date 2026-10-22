# pr-reviewer memory

Lessons from earlier reviews in this repo. Newest last. Keep generalised; prune when stale.

## Bugs this codebase is prone to

- **Semantic conflicts between PRs that are each green on their own** (2026-10-21). #29 made Reserve wait one extra call_timeout, and #27's CLI test asserted about one call_timeout. Both were green alone, and `main` went red after merging. When PRs touch the same behaviour, test them merged (`git merge` in a scratch worktree, `go test -race ./...`) and report it.
- **State outside `core`** (2026-10-21). Fixes tend to add a map or flag in `control` (e.g. a per-EVSE "blocked" set). The #5 rule is that only `core` holds mutable charger state. Flag it, and propose a visible core state (#33 ended up with `cancelling`).
- **Orphans after a CALL was sent** (2026-10-21). A timeout or a dropped connection after ReserveNow went out means the station may hold it. Check what happens locally *and* at the station, including the window before the cancel arrives, the "connection already closed" case, and id reuse (#16 replaces reservations under the same OCPP id).
- **Goroutine lifecycles behind a WaitGroup** (2026-10-21). Check that `Add` can't happen after `Wait` started (a `closed` flag under the same lock), and that shutdown order keeps the connection alive for in-flight work.
- **Unknown vs. absent** (2026-10-21). Probe/config readers conflate "not reported" or "empty" with "no". OCPP 1.6 Core is mandatory, so assume it when not reported.
- **Overloading a field** (e.g. packing a CALLERROR code into `Status`). Suggest a separate field.
- **Shutdown reordering in `cpms run`** (2026-10-22). Moving where the grace `context.WithTimeout` is created can put the unbounded `commands.Close()` (in-flight Reserve and orphan cancels, up to call_timeout each) inside the window, so `csms.Shutdown` gets an expired ctx and skips its wg wait. Check what each Close/Shutdown waits on, relative to where the deadline starts.
- **HTTP seams for later modules** (2026-10-22). Check that handlers can get generated request/correlation ids from `r.Context()` (not only from response headers), that the server has a lifetime context that Shutdown cancels, and that it sets Read/Idle timeouts, not just ReadHeaderTimeout.
- **Seconds-valued config keys** (2026-10-22). `strconv.Atoi` followed by `time.Duration(n) * time.Second` overflows above about 9.2e9, which gives a negative interval, and a negative `time.NewTimer` fires immediately (a heartbeat flood). Check the upper bound as well as `n < 0`. OCPP 1.6 §5.3 wants `Rejected` for out-of-range values.
- **Settings that only apply "from the next tick"** (2026-10-22). A loop parked on a timer armed with the old interval ignores a new value until that timer fires. OCPP `Accepted` means "effective immediately". Prove it with clocktest: a long interval, change it to a short one, advance by the short one.

## Checks that paid off

- Break the code in a scratch worktree (revert the fix, remove a call) and confirm a test fails. "The new tests didn't compile on the old code" is not proof.
- Verify third-party behaviour at the source. rey's "300 s heartbeat" was a stale config key (`HeartbeatInterval` hard-coded); on the wire it used the BootNotification interval. #31 was built on the wrong premise and closed.
- For spec claims, quote the spec text (OCPP 1.6 transcriptions in SAP/e-mobility-charging-stations-simulator `docs/ocpp16/`; OCPI 2.3.0 at github.com/ocpi/ocpi release-2.3.0-bugfixes).
- Check CI on the head (`statusCheckRollup`) first. Implementers' local golangci-lint is often broken (toolchain mismatch), so lint can be red on CI even when the PR says `make ci` is green.
- Mount the routes later issues will need (wildcards, trailing slashes) in a throwaway scratch test, to prove a seam works instead of assuming it.
- Deadlock checks on simulator CALL locks (2026-10-22): `ocppj.Conn` reads in `readLoop` and dispatches inbound CALLs on a separate `handleLoop`, and simulator handlers defer their own CALLs to `after` goroutines. So a lock held across `Conn.Call` cannot block delivery of its answer. Check instead whether a handler takes that lock synchronously, and whether the lock order stays consistent (`callMu` before `mu`).
- Test merges with other open PRs without a fixed branch name: `git merge-tree --write-tree HEAD <ref>` shows conflicts, then merge in the scratch worktree and run the tests. Delete any temporary refs you fetched into the main repo afterwards.
- README shell examples: `base64` wraps at 76 columns on GNU but not on macOS. Use `| tr -d '\n'`.
- Check that the PR doesn't reverse a recorded decision (git blame the test or README line). If it does, the light is at least yellow, and the owner decides.

## False alarms to avoid

- `TestSimulateChargerAgainstRun` can flake under full-suite load (shutdown races the simulator's status burst). It passes in isolation and isn't caused by unrelated PRs.
- "address already in use" when other agents run `cli` tests on fixed ports in parallel: retry, don't report.
