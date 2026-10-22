---
name: simulator-test-seams
description: Seams for testing the simulator's connection lifecycle, offline queue and outages (proxy, fake CSMS hooks, separate clocks)
metadata:
  type: project
---

Seams added for #39 (2026-10-22, PR #42). Check that they still exist before relying on them:
- `ocpptest.NewProxy(t, hostport)`: a TCP relay with `Cut()` (drop everything and refuse new connections), `Restore()`, `Hold()` (drop CSMS→station traffic only) and `Saw(substr)`. To put a rig's simulator behind it, use a mutate func that reads `o.URL`, builds the proxy and swaps `o.URL`. That works for `newRig` (simulator tests) and `newSimRig` (control e2e).
- Fake CSMS (`meter_test.go`) has `bootStatus(n)` and `respond(action, n) (code, drop)` hooks. Set them under `f.mu`, because the race detector does not see happens-before through sockets. Transaction ids count up from `txID`.
- The simulator has one clock per concern: `AfterFunc` (meter), `HeartbeatAfterFunc`, `ReconnectAfterFunc` (redial and tx-retry waits). That way `clk.Pending()==1` assertions on one clock are not disturbed by the others.

**Why:** reconnect/offline tests need deterministic outages and per-message CALLERRORs without real sleeps.
**How to apply:** reuse these for CSMS-side tests of late or duplicated transaction messages. Don't build a new proxy.
