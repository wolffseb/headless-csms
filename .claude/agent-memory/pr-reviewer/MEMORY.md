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
- **Gitignored test fixtures** (2026-10-22). The root `.gitignore` has an unanchored `config.yaml`, so any `testdata/config.yaml` is silently untracked: green in the implementer's worktree, red on CI. When CI fails with "no such file", run `git check-ignore -v <path>` and compare `git ls-tree -r <head>` with what the tests open.
- **OCPI date filters vs. rendered precision** (2026-10-22). Filtering on `last_updated` truncated to the rendered second drops changes for a client that polls with a fractional `date_from` (its last request time). Test the boundaries with sub-second client times, not just echoed values. Echo cases work with the untruncated time too.
- **OCPI `publish: false`** means only tokens in `publish_allowed_to` may be shown the Location. Without that field, a compliant eMSP shows it to no driver. Check comments/README that claim "private to our backend".
- **Auth decided before the lock** (2026-10-22). The OCPI `authorize` middleware checks the token against state *before* the handler takes its mutex. A request queued behind a state change (POST registering, DELETE unregistering) then runs under the new state with the old token's rights. Handlers that change state must re-check the presented token under the lock. Prove it by blocking the fake peer mid-handshake and sending the second request with the then-valid token.
- **Mutex across I/O** (2026-10-22). Watch for locks held over body reads (bounded only by the server ReadTimeout) and outbound calls, and for operator paths (socket ops) that `Lock()` without honouring ctx. The CLI's socket deadline (call_timeout + 5 s) then expires while the action still completes, so the reported exit contradicts the outcome.
- **Settings that only apply "from the next tick"** (2026-10-22). A loop parked on a timer armed with the old interval ignores a new value until that timer fires. OCPP `Accepted` means "effective immediately". Prove it with clocktest: a long interval, change it to a short one, advance by the short one.
- **Sentinel ids escaping their path** (2026-10-22). A placeholder (negative tx id) introduced "for offline" tends to be set on every path, so public getters show it online too, and the error/give-up branches leave it on the connector, so it reaches the wire. Check each exit of the queue: delivered, refused, out of attempts, and answered with id 0/Invalid (cpms answers unknown tags with 0). Grep tests for `TransactionID(n) != 0` used as a "started" signal.
- **Optional wire fields on spec preconditions** (2026-10-22). Adding e.g. `reservationId` to StartTransaction must follow the spec's condition (only for the reserved idTag). The simulator doesn't store a reserved tag, and core matches on the id alone. Check the foreign-tag case against core.
- **At-least-once replay makes duplicates** (2026-10-22). A drop after the CSMS processed a CALL (or a timeout) means identical bytes get sent again. cpms has no dedupe for StartTransaction, so it opens a second active tx. Prove it with the fake CSMS `respond` drop hook.

- **"Busy" refusals pre-empting state-determined answers** (2026-10-22). When a single-flight/in-progress check is added, it tends to come before the spec's 405/401 checks. Then a request that can never succeed gets "retry shortly". Check the order: auth, then state-determined refusals, then busy.

- **idTag comparisons** (2026-10-22). OCPP 1.6 IdToken is CiString20Type, which is case-insensitive (§7.15). core uses `strings.EqualFold`, so flag any `==` on idTags in the simulator or the handlers. Prove it with a case-differs row in the table test.
- **Retry bounds scale with the queue** (2026-10-22). "At most 3 attempts" bounds one message, but a replay holds the CALL slot for queue length × (backoff + 3 × call_timeout), which blocks the status round and new Start/Stop. Quantify the total, not just the per-message cost.

## Checks that paid off

- **Concurrency tests that release a gate and then race** (2026-10-22). "Handler is now doing X" after `Release()`, with nothing waiting for it, is usually vacuous. Mutate the code back to the old ordering and run `-count=30`. For "reading the body", wrap `r.Body` in a reader that signals on its first Read.

- Break the code in a scratch worktree (revert the fix, remove a call) and confirm a test fails. "The new tests didn't compile on the old code" is not proof.
- Verify third-party behaviour at the source. rey's "300 s heartbeat" was a stale config key (`HeartbeatInterval` hard-coded); on the wire it used the BootNotification interval. #31 was built on the wrong premise and closed.
- For spec claims, quote the spec text (OCPP 1.6 transcriptions in SAP/e-mobility-charging-stations-simulator `docs/ocpp16/`; OCPI 2.3.0 at github.com/ocpi/ocpi release-2.3.0-bugfixes).
- Check CI on the head (`statusCheckRollup`) first. Implementers' local golangci-lint is often broken (toolchain mismatch), so lint can be red on CI even when the PR says `make ci` is green.
- Mount the routes later issues will need (wildcards, trailing slashes) in a throwaway scratch test, to prove a seam works instead of assuming it.
- Deadlock checks on simulator CALL locks (2026-10-22): `ocppj.Conn` reads in `readLoop` and dispatches inbound CALLs on a separate `handleLoop`, and simulator handlers defer their own CALLs to `after` goroutines. So a lock held across `Conn.Call` cannot block delivery of its answer. Check instead whether a handler takes that lock synchronously, and whether the lock order stays consistent (`callMu` before `mu`).
- Test merges with other open PRs without a fixed branch name: `git merge-tree --write-tree HEAD <ref>` shows conflicts, then merge in the scratch worktree and run the tests. Delete any temporary refs you fetched into the main repo afterwards.
- Parallel PRs without an open PR (e.g. #13 vs #14) may exist only as uncommitted changes in `.claude/worktrees/agent-*`. Read them with `git -C <worktree> diff` (read-only) to assess overlap.
- OCPP 1.6 says a connector-specific ReserveNow makes the station report `Reserved` itself. So OCPI status derived only from station reports is correct for reservations, as long as core never reserves connector 0.
- README shell examples: `base64` wraps at 76 columns on GNU but not on macOS. Use `| tr -d '\n'`.
- A red CI on only one of the two runs (push vs pull_request) on the same head is still a finding until explained. Read the failing test (#42's "flake" was its own placeholder race), and don't assume it's flaky just because it passes locally.
- Spec section numbers in PRs are often wrong (e.g. "§4.10" for the retry rule, which is §3.7.1). grep the transcription before repeating them.
- Never run `git stash` in a scratch worktree: the stash ref is shared with the main repo. Move scratch files out instead.
- Check that the PR doesn't reverse a recorded decision (git blame the test or README line). If it does, the light is at least yellow, and the owner decides.

## False alarms to avoid

- `TestSimulateChargerAgainstRun` can flake under full-suite load (shutdown races the simulator's status burst). It passes in isolation and isn't caused by unrelated PRs.
- `TestShutdownWaitsForTheOrphanCancel` (cli/station_test.go) flakes under `-race` on the base branch too (2026-10-22). Check the base before blaming the PR.
- Race fixes in code driven by clocktest (2026-10-22). Reverting the fix often still passes `-race -count=20`, because `Advance` orders the racing goroutine after the caller returns. Report "correct by inspection, not guarded by a test", and don't call it proven or missing.
- "address already in use" when other agents run `cli` tests on fixed ports in parallel: retry, don't report.
