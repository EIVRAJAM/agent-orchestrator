# Chat queue resume: releasing one held, never-sent turn

When a chat turn ends as `failed` or `recovered`, AO deliberately **holds** the
queued work behind it so the outage cannot cascade through the rest of the queue
(issue #4861). This document describes the user-initiated counterpart added in
this change: releasing **exactly one** never-sent queued turn.

## Scope and the cold-start limitation

This first delivery resumes a durable `queued` turn **only while a live,
authoritative controller owns the session**. It deliberately does **not** recover
after a cold start: on a cold start `SettleOrphanedConversationTurns` has already
converted the queued rows to `failed` (`queries/conversations.sql`), so there is
nothing left to release. Preserving queued rows across a cold start changes the
recovery policy (agent switches, provider-thread changes) and is a separate
change with its own forensic and generation tests. The API, the CLI help, and the
derived `queueState` all state this limitation.

The recovery that would help *after* a closure — re-queueing a `failed` turn that
was never sent (`state='failed' AND started_at IS NULL`) — is **not** part of
this change.

## What "never sent" means

Only a row still in `state='queued'` (unpromoted, unrolled back) is treated as
never sent. This is a durable fact, not an inference:

- a non-deferred driver calls `MarkTurnDispatching` (`queued -> running`) **before**
  `SendTurn`, so the row leaves the queue before the provider boundary;
- an ACP/OpenCode (deferred) driver binds the row (`queued -> running`) **before**
  `StartDeferredTurn`, which is where `session/prompt` is actually issued.

So a `queued` row has not crossed the provider boundary. A `running` row —
especially one with no `provider_turn_id` — may have been delivered, and is never
resumed.

## API

```
POST /api/v1/sessions/{sessionId}/conversation/queue/resume
```

Request:

```json
{ "expectedTurnId": "turn-id", "expectedControllerGeneration": "generation-id" }
```

`expectedTurnId` is **mandatory**: it is the compare-and-set that makes the
operation idempotent. `expectedControllerGeneration` is optional and is only an
optimistic check; the server always compares the selected controller against the
durable generation itself.

Response (`202` when a turn was released, `200` for `no_pending_work`):

```json
{ "outcome": "resumed", "turnId": "turn-id" }
```

No `providerTurnId` is returned: for ACP/OpenCode it is an AO-generated id and
does not prove the provider accepted anything.

### Outcome → HTTP mapping

| Outcome | HTTP | Stable code | Retryable | Dispatched |
| --- | --- | --- | --- | --- |
| `resumed` | 202 | — | n/a | yes (1) |
| `no_pending_work` | 200 | — | n/a | no |
| `stale_queue_state` | 409 | `CHAT_QUEUE_STALE_STATE` | yes (re-read) | no |
| `session_busy` | 409 | `CHAT_TURN_RUNNING` | yes | no |
| `compaction_in_progress` | 409 | `CHAT_COMPACTION_BUSY` | yes | no |
| `handoff_in_progress` | 409 | `CHAT_INTERFACE_TRANSITION` | yes | no |
| `controller_recovery_required` | 409 | `CHAT_CONTROLLER_NOT_READY` | yes | no |
| `state_uncertain` | 409 | `CHAT_QUEUE_STATE_UNCERTAIN` | no | no |
| `session_terminated` | 409 | `SESSION_TERMINATED` | no | no |
| `stale_controller_generation` | 409 | `CHAT_QUEUE_STALE_CONTROLLER` | yes (re-read) | no |

Additional errors: `400 CHAT_QUEUE_RESUME_TURN_REQUIRED` (missing
`expectedTurnId`), `409 CHAT_QUEUE_DISPATCH_FAILED` (the provider refused the
released turn; that turn was settled `failed` and the rest of the queue is
untouched), `404 SESSION_NOT_FOUND`, `409 SESSION_MODE_MISMATCH`.

## CLI

```
ao session queue <id>                 # show the derived queue state and resume hint
ao session resume-queue <id>          # release the observed queue head
ao session resume-queue <id> --turn <turnId> [--generation <generation>]
```

`ao session queue` prints the state, the pending count, the queue head, and
whether a resume is safe, and (when the queue is held) the exact
`resume-queue` command to run.

`ao session resume-queue` reads the observable queue state first and sends that
state's `headTurnId` and `controllerGeneration` as `expectedTurnId` and
`expectedControllerGeneration`; `--turn` and `--generation` override the observed
values. When there is no head it reports the state instead of sending a request
the server would reject. Both commands are read/act against the daemon HTTP API;
neither touches storage directly. Misuse exits 2; a daemon refusal or fault exits
1 and preserves the error envelope.

## Observable state

The conversation snapshot (`GET /api/v1/sessions/{sessionId}/conversation`) now
carries a derived `queue` object. It is computed on read from durable rows and
the live controller; nothing is persisted.

| Field | Meaning |
| --- | --- |
| `queueState` | `idle`, `running`, `compacting`, `queued_waiting_for_active_turn`, `held_after_terminal_failure`, `controller_recovery_required`, `handoff_in_progress`, `state_uncertain` |
| `pendingCount` | never-sent (`queued`) turns |
| `oldestPendingAt` | when the queue head was recorded |
| `headTurnId` | the queue head a resume must name |
| `controllerGeneration` | the durable generation to echo back |
| `controllerAvailable` | a live controller owns the session |
| `activeTurn` | the running turn AO owns, empty when none |
| `compactionPending` | a compaction is in flight |
| `resumeSafety` | the outcome a resume would return right now |

A merely idle or long-running session is never reported as blocked: `queueState`
is `idle`/`running`/`queued_waiting_for_active_turn`, and only a queue held behind
a non-completed terminal turn is `held_after_terminal_failure`.

## Synchronization

Everything below is evaluated while the controller's `sendMu` is held, so the
check and the dispatch share one boundary.

1. **Controller admission.** `Service.workingController` takes the per-owner
   `controllerGate`, re-reads the session, and releases the gate before the
   dispatch lock (so `Kill` can still interrupt provider work).
2. **Re-validation under `sendMu`.** The resume then re-proves, inside the
   dispatch lock, that the selected controller is still the registered one
   (`Service.controllers[id] == controller`) and that the durable
   `sessions.controller_generation` equals `controller.Generation()`. This is the
   server-side generation authority; `expectedControllerGeneration` is only an
   extra optimistic check.
3. **Preconditions under `sendMu`.** Handoff armed; a durable `running` turn
   (`session_busy`, or `state_uncertain` when a running row has no
   `provider_turn_id`); `compactionPending`; and the in-memory busy gate.
4. **Durable compare-and-set.** The exact `expectedTurnId` must still be the
   queue head, and the store performs
   `ClaimQueuedConversationTurnForResume` — an `UPDATE ... WHERE id=? AND
   conversation_id=? AND state='queued' AND promotion_started_at IS NULL AND
   rolled_back_at IS NULL` returning rows-affected. A pre-check followed by an
   unconditional update is not enough; the CAS is the linearization point that
   moves the row `queued -> running` before the provider call.
5. **Dispatch.** `dispatchTurn(..., preClaimed=true)` hands the claimed row to the
   provider without marking it dispatching again.

Two concurrent requests that name the same `expectedTurnId`: the first claims and
dispatches it; the second finds the head moved (or a live turn) and returns
`stale_queue_state`, `session_busy`, or `no_pending_work`. It never releases the
next turn.

### One-shot semantics

A resume releases **exactly one** turn. When that turn terminates — even as
`completed` — the queue is held again instead of continuing automatically. This
is an in-memory one-shot on the controller, bound to the AO turn id **and** the
controller generation (never a bare boolean):

- it is armed **before** the dispatch and its provider turn id is filled once the
  row is bound;
- `afterProject` suppresses the automatic drain **only** when the terminal event
  matches that provider turn id; an ordinary completed turn still drains the
  queue exactly as before;
- it is cleared on completion (consumed), on dispatch failure, on interrupt, on
  handoff, and on controller close/termination. A replacement controller starts
  with no hold and the queue held by the normal rule.

### Lock order

`controllerGate` (admission only, released before `sendMu`) → `sendMu` →
`{ Service.mu (registry read), store.writeMu }`. Nothing takes `sendMu` while
holding `Service.mu`, and the store never calls back into the service, so there
is no cycle.

## Tests

- `backend/internal/service/chat/queue_recovery_test.go`: one-turn release; re-hold
  after the resumed turn completes; ordinary completed turn still drains;
  concurrent resume dispatches at most one; stale head refused; no pending work;
  busy refused; unconfirmed running refused; `expectedTurnId` required; snapshot
  reports held / idle / waiting states.
- `backend/internal/service/chat/queue_recovery_supervision_test.go`: a second
  resume releases the next turn; immediate completion of the first dispatch does
  not advance the queue; stale client generation refused; a replaced controller
  (durable generation drift) refused; handoff and compaction refused; controller
  absent asks for recovery; terminated session refused; a provider disconnect
  during the released dispatch keeps the still-queued rows queued; a rate-limit
  report never dispatches and a rate-limited turn re-holds the queue.
- `backend/internal/service/chat/controller_hold_queue_test.go`: the #4861 hold
  tests, unchanged.
- `backend/internal/httpd/controllers/conversation_queue_resume_test.go`: route
  statuses and stable codes.
- `backend/internal/cli/session_queue_test.go`: the CLI reads the observable state
  and sends its head and generation; no-work path; stale CAS and daemon faults
  surface as exit 1; misuse exits 2.

## Out of scope

- Preserving `queued` rows across a cold start (Option B).
- Re-queueing `failed` turns whose `started_at IS NULL`.
- Automatic retries. Delivery classification for a possibly-accepted turn is not
  sufficient for a safe second delivery (no `Retry-After` is preserved for chat
  turns, and ACP/OpenCode provider turn ids are AO-generated).
