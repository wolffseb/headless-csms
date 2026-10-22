---
name: csms-offline-gaps
description: Known CSMS (core / v16 handler) gaps with replayed offline transactions, found in #39 and left unfixed
metadata:
  type: project
---

As of 2026-10-22 (PR #42, "Found, not fixed"):
- A replayed StartTransaction with an unknown tag gets `transactionId: 0` and is not recorded. `TestOfflineStartWithAnUnknownTag` t.Skips until core records it.
- `core.RecordMeterValues` stamps events with receive time and ignores the sample timestamp.
- A StopTransaction for an unknown tx is logged and accepted; its data is lost.
- A reservation ended as timeout/failed can still be consumed by a replayed start with its `reservationId`; no correcting event is emitted.
- The simulator's StartTransaction sends `reservationId` since #39.

**Why:** the orchestrator files CSMS issues from these, and a later issue may fix them.
**How to apply:** if you touch the v16 handler's start/stop or core meter events, check these first, and un-skip the test when you fix the first one.
