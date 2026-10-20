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

Both `expectedTurnId` and `expectedControllerGeneration` are **mandatory**. They
are the compare-and-set that makes the operation idempotent and generation-safe:
an empty generation is rejected with `400
CHAT_QUEUE_RESUME_GENERATION_REQUIRED`, never read as "use the current
generation". The server independently re-reads the durable session row and the
row's own generation inside the claim, so the fields are a client's assertion, not
the authority.

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
`expectedTurnId`), `400 CHAT_QUEUE_RESUME_GENERATION_REQUIRED` (missing
`expectedControllerGeneration`), `409 CHAT_QUEUE_DISPATCH_FAILED` (the provider
refused the released turn; that turn was settled `failed` and the rest of the
queue is untouched), `404 SESSION_NOT_FOUND`, `409 SESSION_MODE_MISMATCH`.

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
values. Both are always sent: a missing generation is a usage error rather than an
implicit "current generation", and an explicitly empty `--turn`/`--generation` is
also a usage error. When there is no head it reports the state instead of sending
a request the server would reject. Both commands are read/act against the daemon
HTTP API; neither touches storage directly. Misuse exits 2; a daemon refusal or
fault exits 1 and preserves the error envelope.

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

The whole resume — admission, validation, the durable claim, and the provider
boundary — runs while the per-owner `controllerGate` is held, and the claim and
dispatch additionally run under `sendMu`. The only writer of
`sessions.controller_generation` is `Service.Start`, which holds that same gate
while it claims the generation and installs the controller. So a replacement
cannot interleave between validation and the provider boundary: it would have to
take the gate, which the resume is holding.

1. **Admission.** `Service.workingController` takes the gate, re-reads the
   session, and returns the controller. The resume then re-takes the gate and
   keeps it for the rest of the operation. Re-taking it (rather than keeping the
   one `workingController` used) matches `lockCurrentSide`; the re-validation
   below closes the small window in between.
2. **Re-validation under the gate and `sendMu`.** The resume re-proves that the
   selected controller is still the registered one (`Service.controllers[id] ==
   controller`), that the durable `sessions.controller_generation` equals
   `controller.Generation()`, and that the caller's
   `expectedControllerGeneration` equals it too. The controller itself refuses
   any generation other than its own before touching the queue.
3. **Preconditions under `sendMu`.** Handoff armed; a durable `running` turn
   (`session_busy`, or `state_uncertain` when a running row has no
   `provider_turn_id`); `compactionPending`; and the in-memory busy gate.
4. **Durable compare-and-set.** The store runs
   `ClaimQueuedConversationTurnForResume`, an `UPDATE ... WHERE id=? AND
   conversation_id=? AND state='queued' AND promotion_started_at IS NULL AND
   rolled_back_at IS NULL AND controller_generation=? AND EXISTS (SELECT 1 FROM
   sessions s WHERE s.id = conversation_turns.handled_by_session_id AND
   s.controller_generation=?)` returning rows-affected. The claim therefore
   checks, in one statement, both that the row belongs to the named epoch and
   that the handling session's live generation is still that epoch. A Go-side
   pre-check followed by an unconditional update would be a TOCTOU; this is the
   linearization point. Because the check is atomic with the claim, even a
   generation change that bypassed the gate (a direct store write, or a stale
   caller) yields zero rows and no provider call.
5. **Dispatch.** `dispatchTurn(..., preClaimed=true)` hands the claimed row to
   the provider without marking it dispatching again.

### The window after the claim

Between the claim and the provider boundary the row is already `running` and
bound to the old generation, so:

- no other path can claim it — the drain, a second resume, and the replacement
  controller all select only `state='queued'` rows — so there is no re-delivery;
- the old controller can never dispatch a turn of the new generation, because the
  claim requires `conversation_turns.controller_generation` to equal the old
  controller's own generation;
- the turn is not lost: it is owned by the epoch that claimed it and is settled
  normally by that controller's stream.

A `Start` that replaces the controller terminates the old controller
(`Controller.Terminate`/`Close`), which closes the provider conversation and
cancels any not-yet-crossed provider work, so the claimed turn is settled rather
than duplicated. For ACP/OpenCode the real `session/prompt` runs in a goroutine
started by `StartDeferredTurn`; by the time it runs the row is claimed and bound,
so it cannot be re-claimed either.

Two concurrent requests that name the same `expectedTurnId`: the first claims and
dispatches it; the second finds the head moved (or a live turn) and returns
`stale_queue_state`, `session_busy`, or `no_pending_work`. It never releases the
next turn.

### Delivery certainty and bind failures

The provider boundary and the durable binding are not the same step, and the two
drivers order them differently:

- **Eager** (for example Codex app-server): `SendTurn` is the provider boundary.
  It returns once the provider acknowledges. `BindTurnToProvider` then records the
  provider turn id.
- **Deferred** (ACP/OpenCode): `SendTurn` only prepares the request.
  `BindTurnToProvider` runs first and `StartDeferredTurn` is the provider
  boundary.

So a `BindTurnToProvider` failure means different things:

| Driver | Bind failure means | Row | One-shot | Outcome |
| --- | --- | --- | --- | --- |
| Eager | `SendTurn` already succeeded, so the provider may have accepted and run the turn | stays `running`, **no** provider id | **stays armed**, bound to the returned provider id | `state_uncertain` (409) |
| Deferred | the provider never received the prompt | settled `failed` | cleared | `CHAT_QUEUE_DISPATCH_FAILED` (409) |

For an eager bind failure the resume never reports `resumed`, never requeues the
row, never retries the send, and never dispatches the next turn. The hold is bound
to the provider id **before** the durable write, so a late terminal for that id is
still recognized as the resumed turn's own and suppresses the automatic drain.
The row stays `running`/unbound, so the snapshot reports `state_uncertain`, every
later resume is refused with zero sends, and the controller stays busy until
recovery settles the orphan. The drain also refuses while any durable running turn
exists, so an unrelated later event cannot advance the queue either.

For a deferred bind failure the prepared turn is discarded and the row is settled
`failed`; because the provider boundary was never crossed, the next turn may be
resumed normally.

### Failure classes

| When the failure happens | Turn row | One-shot | Outcome | Provider may have it | Another resume | Who settles |
| --- | --- | --- | --- | --- | --- | --- |
| before the CAS (head moved, decode/excerpt, store error) | `queued` or already terminal | cleared | `stale_queue_state` / `no_pending_work` / error | no | yes (re-read) | no one; the row never left the queue |
| after the CAS, before the provider (eager) | `failed` (settled by dispatch) | cleared | `CHAT_QUEUE_DISPATCH_FAILED` | no | yes | `dispatchTurn` settles it |
| after the provider may have accepted (eager `SendTurn` ok, bind failed) | `running`, unbound | **armed** | `state_uncertain` (409) | **possibly yes** | **no** | the controller's stream or recovery (`SettleOrphanedConversationTurns`) |
| after the durable binding, `StartDeferredTurn` fails | `failed` | cleared | `CHAT_QUEUE_DISPATCH_FAILED` | request issued, rejected | yes | `dispatchTurn` settles it |
| deferred bind fails before `StartDeferredTurn` | `failed` | cleared | `CHAT_QUEUE_DISPATCH_FAILED` | no | yes | `dispatchTurn` settles it |
| dispatch/claim later fails after a durable binding | terminal per the store | cleared by the error path | mapped by outcome | yes | only after the terminal | the terminal / recovery |

No failure after a possible acceptance can requeue the row, replay the send, or
let the queue drain automatically.

### One-shot semantics

A resume releases **exactly one** turn. When that turn terminates — even as
`completed` — the queue is held again instead of continuing automatically. This
is an in-memory one-shot on the controller, bound to the AO turn id **and** the
controller generation (never a bare boolean).

Exact cycle:

1. `armResumeHold(expectedTurnID)` records `{turnID, generation, providerTurnID:
   ""}` before the dispatch, under `sendMu`.
2. The claim moves the row `queued -> running`.
3. `SendTurn` crosses the provider boundary and returns a provider turn id.
4. `BindTurnToProvider` records that id on the row, and `dispatchTurn` binds it
   onto the hold in the same `c.mu` critical section that publishes
   `pendingTurnID` — still under `sendMu`.
5. `project()` consumes the terminal event with `sendMu` held (lifecycle events
   take it), so `consumeResumeHold(providerTurnID)` cannot run before step 4 has
   finished. A driver that completes the turn synchronously from `SendTurn` (or
   from `StartDeferredTurn`) therefore cannot be mistaken for an ordinary
   completion.
6. `consumeResumeHold` clears the hold and suppresses the automatic drain only
   when the event's provider turn id matches the hold's **and** the generation is
   still current. An ordinary completed turn is unaffected.
7. The hold is cleared on dispatch failure, on interrupt, on handoff, and on
   controller close/termination. A replacement controller starts with no hold and
   the queue held by the normal rule.
8. It is also retired whenever the drain returns without claiming. The drain
   re-reads the busy and handoff state under the lock and can return early, so
   `ResumeQueuedTurn` confirms the row actually left `queued` before reporting
   `resumed`. A row still in `queued` proves nothing was released: the marker is
   cleared and the call reports `handoff_in_progress` or `session_busy`. A failed
   CAS, a store error, a stale head, a decode/excerpt failure, a `SendTurn` error,
   a `BindTurnToProvider` error and a `StartDeferredTurn` error all take the
   error path, which clears the marker unconditionally.

### Lock order

`controllerGate` → `sendMu` → `{ c.mu, Service.mu (registry read),
store.writeMu }`. This is the order `lockCurrentSide` already uses. `Start` and
`Stop` take the gate first and never take `sendMu` while holding it; the event
loop takes `sendMu` for lifecycle events and never takes the gate; the store never
calls back into the service. So there is no cycle and no inversion.

## Tests

- `backend/internal/storage/sqlite/store/conversation_queue_claim_test.go`: the
  CAS claims exactly the named turn; refuses a stale row generation; refuses a
  changed live session generation; refuses a wrong named generation; never claims
  another turn or re-claims the same one; the row stays `queued` on every refusal.
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
- `backend/internal/service/chat/queue_recovery_race_test.go`: a generation change
  after validation but before the claim is refused by the CAS with zero provider
  sends (a pausing session reader opens the window deterministically); a driver
  that completes the resumed turn synchronously from `SendTurn` still holds the
  queue (eager path); the same for the deferred ACP path, where the completion is
  emitted from `StartDeferredTurn` after the provider id is bound; an empty
  generation is refused.
- `backend/internal/service/chat/queue_recovery_exits_test.go`: the marker is
  cleared on every non-dispatch exit (a lost CAS, a claim store error), the
  follow-up valid resume of the new head works, and an unrelated terminal event
  does nothing; a concurrent `Start` is excluded by `controllerGate` and a real
  `Stop` waits on it, then terminates the controller and settles the running turn
  after the single send; a real replacement that wins the gate first makes the
  resume revalidate and refuse; deferred `SendTurn` and `StartDeferredTurn`
  failures settle the claimed row and clear the marker; the deferred provider id
  is bound before `StartDeferredTurn`; an eager bind failure after `SendTurn`
  stays `state_uncertain` with the marker armed, a late terminal is consumed, and
  a second resume sends nothing (with and without the late terminal).
- `backend/internal/service/chat/queue_state_derivation_test.go`: a fresh queue, a
  completed terminal turn, a compaction, a handoff, and an absent controller are
  not labelled `held_after_terminal_failure`; the priority order between
  concurrent states (handoff beats a running turn).
- `backend/internal/service/chat/controller_hold_queue_test.go`: the #4861 hold
  tests, unchanged.
- `backend/internal/httpd/controllers/conversation_queue_resume_test.go`: route
  statuses and stable codes, including missing turn and missing generation.
- `backend/internal/cli/session_queue_test.go`: the CLI reads the observable state
  and sends its head and generation; no-work path; flag overrides; missing and
  explicitly empty generation are usage errors; stale CAS and daemon faults
  surface as exit 1; misuse exits 2.

## Out of scope

- Preserving `queued` rows across a cold start (Option B).
- Re-queueing `failed` turns whose `started_at IS NULL`.
- Automatic retries. Delivery classification for a possibly-accepted turn is not
  sufficient for a safe second delivery (no `Retry-After` is preserved for chat
  turns, and ACP/OpenCode provider turn ids are AO-generated).
