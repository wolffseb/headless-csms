# Merge risk

Every PR gets one **merge-risk light** from its review. The light decides who may merge it.
It is set as a PR label (`merge-risk: green`, `merge-risk: yellow`, `merge-risk: red`) and
as the first line of the review body, with the reason.

| Light | Meaning | Who merges |
|---|---|---|
| 🟢 **green: full agent** | Small blast radius, easy to revert, no behaviour anyone relies on changes | The orchestrating agent, once CI is green on the PR head and every review thread is resolved or answered. It tells the owner afterwards. |
| 🟡 **yellow: human in the loop** | Changes behaviour someone relies on, or interacts with other work | The owner, after reading the review summary. |
| 🔴 **red: system critical / security** | Hard to undo, or a security, data-loss or real-station risk | The owner, after reading the code. A second, independent review first is recommended. |

## How the light is set

The light is the **highest** of three inputs. Nothing can lower it below the floor.

### 1. Path floor

Determined only by the files the PR touches.

| Floor | Paths |
|---|---|
| 🔴 | `internal/state/**` (the `state.json` schema, migrations, the lock) · OCPI auth and credentials (`internal/ocpi/**` files handling tokens, the `Authenticator`, `credentials`) · `.github/workflows/**`, `.goreleaser.yaml` · `go.mod`, `go.sum` (a new or upgraded dependency) |
| 🟡 | `internal/core/**` · `internal/control/**` · `internal/ocpp/**` (protocol behaviour towards a station) · `internal/ocpi/**` (anything else) · `internal/cli/**` · `internal/config/**`, `config.example.yaml` · `internal/simulator/**` (a downstream project, `wolffseb/ocpp-tap`, pins it) · `.claude/agents/**`, this file |
| 🟢 | `docs/**` apart from this file · `README.md` · `*_test.go` and `testdata/**` when nothing else changes |

A PR touching several paths takes the highest floor among them.

### 2. Change signals

These raise the light whatever the paths are.

- 🔴: the `state.json` schema changes, or anything can lose state or the OCPI registration · a token, secret or credential is logged, stored or compared · something new goes outward-facing (a release, a publish, a call to a real station or partner) · OCPP semantics towards a real station change in a way the simulator can't prove.
- 🟡: a public surface changes (CLI command, flag, exit code, `--json` output, socket protocol, config key, OCPI response) · concurrency, locking, timers or goroutine lifecycles change · it reverses a decision recorded in an issue, the README, a test or `docs/IDEAS.md` · it interacts with another open PR (shared files, or behaviour the other one relies on) · a test was relaxed or deleted.

### 3. Review outcome

- An unresolved finding: at least 🟡.
- A verified correctness or security bug left unfixed: 🔴.
- CI not green on the PR head: not mergeable at any light.

## Re-assessment

The reviewer sets the light. After each fix pass, it is set again from the new diff. A fix can lower the light to its floor, or raise it.

## When in doubt

Take the higher light. A yellow that could have been green costs the owner a minute. A green that should have been red can cost a station, a registration or a token.
