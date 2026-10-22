---
name: worktree-tooling
description: Tooling traps in this repo's agent worktrees - command guard, lint toolchain, mutation checks
metadata:
  type: feedback
---

- The worktree guard refuses Bash commands it cannot verify. Typical triggers: `git grep` across several refs combined with other commands, chains of `cp` + heredoc + `&&`, and some long python heredocs. Run plain, separate commands. A failed write may also be the transient "classifier gave no verdict"; retry once.
- golangci-lint is at `~/go/bin/golangci-lint` (v2.6.2, not on PATH). Under Go 1.27 run it as `GOTOOLCHAIN=go1.26.0 ~/go/bin/golangci-lint run ./...`.
- For the "show each acceptance test fails" step: commit a WIP first, then mutate with `sed -i ''` or the Edit tool, run, and `git checkout -- file`. Squash afterwards with `git reset --soft <part-1 sha>` before pushing.

**Why:** found on 2026-10-22 (#39); each cost several retries.
**How to apply:** at the start of every issue in this repo.
