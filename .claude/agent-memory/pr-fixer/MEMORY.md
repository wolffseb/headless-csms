# pr-fixer memory

Lessons from earlier fix passes in this repo. Newest last within each section. Keep generalised; prune when stale.

## Before changing code

- Check whether the comment is right before fixing it. #31's premise (rey ignores the heartbeat interval) was wrong, and the fix reopened a bug from #3. If a comment relies on third-party behaviour, check it.
- Don't keep charger state outside `core` to make a test pass (2026-10-21, #33). If a test forbids the state you need, the test's expectation is what should change: say so and relax it minimally rather than hiding state elsewhere.
- Look for an earlier deliberate decision before reversing behaviour: git blame the test, README or comment. If you find one, stop and report instead of flipping it.
- Read the orchestrator's brief against the code. If a requested outcome can't happen, implement what the state actually decides and say why. In #41, "TOKEN_A PUT during a POST → 401" was really 405, because TOKEN_A is valid while nothing is registered.
- Don't invent spec exemptions. OCPP 1.6 §3.7.1 lets a station give up on a transaction message only after repeated failures, with no exception for FormationViolation and similar codes (#42).
- Validating a name against the host (`time.LoadLocation`) gives host-dependent answers (macOS is case-insensitive). A shape regex is leaky too (`Europe/BerLin`); match against an embedded canonical list and test it against the toolchain data, using `go env GOROOT` (`runtime.GOROOT` trips SA1019) (2026-11-13, #40).
- When a mapping changes a core status (e.g. Finishing → BLOCKED), grep consumers for comparisons with the old value; core itself only passes status through, so the audit mostly lands in the simulator's fake station (2026-11-13, #40).
- OCPP 1.6 errata text isn't in the repo. When a review quotes an erratum, a web search for the quoted sentence usually finds it cited by another station implementation; say in the reply what you checked it against (2026-11-16, #62).
- Simulator ChargePointMaxProfile is a station total (§3.13.3): one `shareOf` converter divides it between running sessions for the meter and the per-connector composite, and multiplies per-connector limits up for connector 0. Change both paths together (#62).
- When one test file holds rows for two fixes, remove the later fix's rows before the first commit and re-add them afterwards, since `git add -p` is unavailable.
- The merge-risk light is the highest of all its signals. Fixing the finding the brief calls "the reason for red" doesn't lower it when a structural signal remains, such as a new CALL to a real station. Re-read every signal in the review body before promising yellow (2026-11-16, #61).

- A test that stays lenient until a dependency PR lands (e.g. a reflect check for a field) must turn strict in the rebase onto that PR: drop the fallback, then prove it bites with a temporary mutation (#66).
- Keep-both conflict resolution for Go type/func blocks: when both sides end at the same shared closing brace, ours needs its own `}` (and `}` for loops) before theirs. Build right after resolving (#66).

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
- Per-field merges (MeterValues): test with two messages whose measurand sets don't overlap, and assert each value's own time as well as its value. A merge that loses the per-field time passes a values-only test (#61).

- A guard condition a review says is untested ("remove it, tests stay green"): the mutant check is to delete just that clause and see the new test fail on its side effects (events, profiles, reading, log text), not only on the headline one (#74).

- A client that waits for a daemon must wait longer than the daemon's longest path (queue cap + call timeout + margin), and the daemon needs a distinct "not sent" error: only then can the client promise "failed" means "nothing happened". To show a CLI-wait test fails, put the old wait formula back in place (not `git stash`, which also drops the new test) and run it; keep the scenario in whole seconds (call_timeout 9s, station answers after 7.5s, two concurrent commands) so it stays ~15 s and `testing.Short()` can skip it (2026-11-19, #76).
- Before sizing such a wait, grep every op the daemon serves for how many outbound calls it makes (here each makes one; Reserve's cancel is background).

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
- A branch already checked out in another worktree can't be checked out again: use a local branch of another name tracking it, and push with `HEAD:<branch>` (#66).
- Stop a background `cpms run` by its PID (`kill -INT`), not `pkill -f`, which also hits other agents' runs.
- Run `go test -race -timeout 240s ./...`: a run with no timeout hung once for 15 min under load and named nothing. With the timeout, a real hang fails with a stack dump (#61).

- Queue-aware timeouts (2026-11-19, #76): once a call can wait for a slot, every ctx deadline sized "call_timeout + slack" for a send that never waits is wrong. Size per attempt with `ocpp.CallBound`, and when a background action (orphan cancel) must reach the station, retry on `ErrNotSent` instead of ending its record. A queue-cap error also caps *other* queued calls in the test: a second command queued early is itself dropped as not-sent before it gets the slot. Queue it late (0.75 T) in the scenario.
- An error that matches two sentinels (`ErrClosedUnsent` is both not-sent and not-connected) makes `errors.Is` order matter in every switch: check the more specific/terminal one (not-connected) before the retry/"busy" one, in `classify`, `setProfile` and the retry loop, and test each.
- A select with two ready cases (free buffer vs closed `done`) is random: a "closed before send" guarantee needs a non-blocking pre-check of `done` first, and the test needs a loop (200 calls) because a single call passes half the time. Keep the leftover race (close after the check) classified as the honest "may have been sent" and say so in a comment (2026-11-19, #76).
- revive wants a doc comment on every exported method, even tiny `Is` helpers: copy the sibling type's comment. Run golangci-lint before the first push, since the lint job is the first CI check to go red (#76).
- A "must fail when X is removed" test for a retry or timer loop should fail fast, not by `eventually` timing out: wait on "retry armed OR round ended" and assert which one happened, so the mutant gives a clear message in milliseconds (2026-11-19, #78).
- Transport errors from `client.Do` wrap a `*url.Error` and open with the full URL twice. Anything shown cut to a width (`cpms status`) should store `errors.As(*url.Error).Err` instead (#78).
- Untested wiring inside `run` closures: extract to a small type in its own file (`internal/cli/charger_policy.go`) so run.go shrinks to one line per field and stacked PRs editing run.go merge cleanly; test the helper and mutate it (2026-11-23, #90). The module path is `github.com/wolffseb/cli-cpms`, not headless-csms.

- Multi-charger (2026-11-23, #89): core makes synthetic EVSE uids "<cp>-<n>" per charger, so uids are not unique across chargers' snapshots; key any per-snapshot map by charger first. Hand-built configs may leave EVSE.Charger empty: resolve it to the only charger (local `chargerOf` in locations/push, as config is another PR's file).
- Multi-charger (2026-11-23, #91): a default documented in config ("empty Charger = the only charger") needs one exported accessor (`Config.ChargerOf`); every raw `e.Charger` read in other packages silently skips it. Prove with the original hand-built test configs (no `Charger`), not by adding the field to them. A routing field the old daemon ignores needs a socket `Version` bump. Sandbox: a branch checked out elsewhere needs `git checkout -B fix-N origin/<branch>` and `git push origin HEAD:<branch>`.

- Bind async deliveries to the identity they were caused under (2026-11-23, #94): a stored `response_url` must carry the partner registration (`RegisteredAt`: POST sets it, PUT keeps it) and the sender asks `ClientFor(registration)`, which reads the store once and returns false when it changed. Demonstrate the old leak by temporarily downgrading the "wait for the drop log" fatal to `break`, so the Authorization assertion shows it; keep the log wait in the real test, because it is what makes the absence check meaningful.
- `internal/state` has a reflect test (`full()`) that fails on any new unset field, and `normalize()` must `.UTC()` new time fields. Use `omitzero`, not `omitempty`, for time.Time.
- A replacing ReserveNow that fails: the station's state is known for ErrNotSent and for a refusal (keep the original: `RevertReplacement`), unknown for timeout/connection drop (end it, but `AuthoriseTag` both tags to their own expiries). A pending replacement keeps `prior`, so the file lists it and its tag stays redeemable. Reasoning from the owner's rule, flagged "for the owner to confirm" in the PR.
- A cancel that can wait for a pending reserve needs a 2-call bound (`plan.calls`) in both the CommandResponse timeout and the run context. Wait inside control (a `settling` channel per OCPP id), not in the commands package, so no charger state lives outside core/control.

- Waits that must cover a state write (2026-11-23, #94): a wait keyed by an id that exists only after `Begin*` returns misses the window of the persist inside it. Register the wait before the record exists, keyed by something known earlier (the EVSE, as a slice of channels so concurrent calls don't overwrite each other). `statetest.Store.BlockNext()` blocks the next write on a gate for such tests.
- A security check on a partner-supplied URL: prove the e2e test fails by disabling the check in place (`if false &&`), and make the scenario reach ACCEPTED on the old code (the EVSE must be free again, so run it after the `cpms cancel`), or the mutant is rejected for another reason and proves nothing.
- state.json schema: any new field bumps `state.Version` (and the "newer version" test's literal moves up). Docs don't mention the version.
- Bubble Tea v1 queries the terminal (OSC 11 + DSR, up to 5 s) from a package init in every binary that links it; v2 does not (2026-11-23, #93). Check `go list -deps ./cmd/...` for it, and prove it on a pty nobody answers (python `pty.fork`, or `creack/pty` + `io.ReadAll`, no write-back).
- Charm v2 migration (2026-11-23, #93): the Go minimum follows the module (bubbletea v2.0.3+ needs 1.25; v2.1.0 / lipgloss v2.1.0 need 1.26), so pick the newest release that keeps `go` where you can live with it, and update README/docs "Needs Go". v2 renders only changed cells, so `bytes.Contains` on raw pty output fails: feed the bytes to `x/vt` `SafeEmulator` (pipe its replies back to the pty, never `Close` it, it races) and search `ansi.Strip(emu.Render())`. lipgloss v2 always emits SGR, so goldens use `ansi.Strip` and style tests read the raw render. teatest v2 has no tagged release: use `tea.NewProgram` with `WithInput(io.Pipe)`/`WithOutput`.
- `signal.NotifyContext`'s stop() restores the default signal action: never call it before shutdown ends; derive a `WithCancel` for "quit" instead (2026-11-23, #93). The e2e: slow simulator (SlowDelay > call_timeout) keeps a Reserve pending, press q, SIGINT after "shutting down", expect exit 0.
- Fixed test ports are shared by the whole package: grep `19[0-9]{3}` across `internal/cli/*_test.go` before picking one, because a clash only shows in the full run (2026-11-23, #93).

- TUI wording (2026-11-23, #93): the log pane's event text (`eventtext.go`) and the charger line must share one converter (`offlineReason`), or a transport error leaks into one of them. `-update` on a golden test also rewrites other goldens it renders (help at 80 and 50 columns): check `git diff testdata` and run the whole package before committing. A guard clause ("is online") needs its own test where the guarded case is online but has the data the clause protects (a last-seen time).
