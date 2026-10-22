# pr-fixer memory

Lessons from earlier fix passes in this repo. Newest last. Keep generalised; prune when stale.

## Before changing code

- Check whether the comment is right before fixing it. #31's premise (rey ignores the heartbeat interval) was wrong, and the fix reopened a bug from #3. If a comment relies on third-party behaviour, check it.
- Don't keep charger state outside `core` to make a test pass (2026-10-21, #33). If a test forbids the state you need, the test's expectation is what should change: say so and relax it minimally rather than hiding state elsewhere.
- Look for an earlier deliberate decision before reversing behaviour: git blame the test, README or comment. If you find one, stop and report instead of flipping it.

## Testing

- "Shown to fail" means: revert the fix (or remove the call) in place, run the test, see it fail, restore. A test that only fails to compile on the old code doesn't count.
- Async work (background cancels, timers) needs a deterministic gate in the fake (a channel the test releases) and a `Close()`/wait before asserting. Never sleep.
- Simulator scenario `slow` delays every answer, including the cancel. Assert on the run log or core state, not on the station receiving it.
- `goleak.VerifyNone` with `IgnoreCurrent()` in non-parallel tests. Background goroutines must finish by cleanup.
- Fixed ports per issue range; other agents may be running the same tests, so on "address already in use", retry.

## Working with stacked/parallel PRs

- Pushing to a branch another worktree has checked out moves that worktree's ref, and its index then looks dirty. Harmless, but don't commit from a stale worktree.
- When told another agent owns a file, don't touch it; put the needed change under "Found, not fixed" with exact wording.

## Tooling and timer tests (2026-10-22, #37)

- Worktree-isolated Bash refuses compound commands (heredoc + git, `PATH=... make`, `&&` chains with git). Write scripts with the Write tool into the scratchpad and run them as one plain command.
- golangci-lint v2.6.2 built under Go 1.27 fails with "export data version" typecheck errors; `golangci-lint@latest` (installed to a scratch GOBIN) lints clean. `make ci` needs it on PATH, which the sandbox won't let you set inline; run vet, lint and `go test -race` separately. GitHub CI is the authority.
- Clocktest timers: a test that advances the clock right after a "re-arm" must first see the re-armed delay (record the last `d` in the AfterFunc wrapper in an atomic), because `Pending()==1` holds both before and after a stop-and-re-arm.
- Config-change wake-ups: one closed-and-replaced channel per setting (not one shared), read together with the value under `s.mu`, so a loop never misses a change and unrelated changes don't reset its timer.
- Give the heartbeat its own `HeartbeatAfterFunc`; sharing `AfterFunc` would break the meter tests' `Pending()` counts.
