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
| `queueState` | `idle`, `running`, `compacting`, `queued_waiting_for_active_turn`, `held_after_terminal_failure`, `held_after_manual_resume`, `session_terminated`, `controller_recovery_required`, `handoff_in_progress`, `state_uncertain` |
| `pendingCount` | never-sent (`queued`) turns |
| `oldestPendingAt` | when the queue head was recorded |
| `headTurnId` | the queue head a resume must name |
| `controllerGeneration` | the durable generation to echo back |
| `controllerAvailable` | a live controller owns the session |
| `activeTurn` | the running turn AO owns, empty when none |
| `compactionPending` | a compaction is in flight |
| `resumeSafety` | the outcome a resume would return right now |

A merely idle or long-running session is never reported as blocked: `queueState`
is `idle`/`running`/`queued_waiting_for_active_turn`, and a queue held behind a
non-completed terminal turn is `held_after_terminal_failure`.

`held_after_manual_resume` means an explicit resume released one turn, that turn
**ended**, queued work remains, and the automatic drain is deliberately held until
another explicit resume. It is not `queued_waiting_for_active_turn`: there is no
active turn. The retention lives only in the controller, is tied to its
generation, records the turn that produced it, is cleared by the next explicit
resume, and is cleared on handoff, close, terminate or replacement. It is never
persisted and does not change the cold-start policy.

`session_terminated` is the strongest state; a terminated session is never
reported as a recoverable queue.

Priority, strongest first:

1. `session_terminated`
2. `handoff_in_progress`
3. `state_uncertain` (a running turn with no durable provider id)
4. `running` / `queued_waiting_for_active_turn` (an active turn)
5. `compacting`
6. `controller_recovery_required` (pending work, no live controller)
7. `held_after_terminal_failure`
8. `held_after_manual_resume`
9. `queued_waiting_for_active_turn` (a normal pending queue)
10. `idle`

An uncertain turn or a handoff is never hidden behind a retention.

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

The provider boundary and the durable binding are not the same step, and each
adapter orders them differently. Do not claim "rejected" from a local error.

| Adapter | Provider boundary | Durable binding | Acceptance evidence | Definitive errors | Ambiguous after-boundary errors |
| --- | --- | --- | --- | --- | --- |
| Eager (Codex app-server, `ChatConversation.SendTurn`) | `SendTurn` — it writes `turn/start` and waits for the response | `BindTurnToProvider`, after `SendTurn` returns the provider id | the provider turn id returned by `SendTurn` | `ports.ErrTurnNotDelivered` (local validation, or the connection was closed before any write) and `ports.ErrTurnRejected` (the provider answered with an RPC error) | a write failure, EOF, close, timeout, cancellation after the write, or an undecodable response: the request may have reached the provider |
| Deferred (ACP/OpenCode/OpenCode v2, `ChatDeferredTurnStarter`) | `StartDeferredTurn` — ACP `session/prompt` | `BindTurnToProvider`, **before** `StartDeferredTurn` | none synchronously; the turn's events arrive on the stream | `ports.ErrDeferredTurnNotStarted`: the adapter proves the prompt was never issued (conversation closed, prepared turn missing) | any other `StartDeferredTurn` error, and any later transport error/EOF/timeout/cancel that arrives as a turn event |

The eager adapter classifies in `classifySendTurnError`
(`adapters/chatdriver/codexappserver/conversation.go`): a `*rpcError` becomes
`ErrTurnRejected`; `errConnClosedBeforeWrite` becomes `ErrTurnNotDelivered`; every
other error keeps its plain chain so the controller treats the delivery as
uncertain. It does not classify by text. Any adapter that cannot prove the error
was before the write must leave it ambiguous.

An uncertain eager `SendTurn` leaves the row `running`/unbound, keeps
`dispatchingTurnID` so a late `TurnStarted` can persist the binding, returns
`state_uncertain`, and never settles `failed`, requeues, retries, or dispatches
the next turn. If no event ever arrives, the normal controller cleanup/recovery
settles the orphan.

`StartDeferredTurn` in ACP never performs the RPC itself:
`adapters/chatdriver/acp/conversation.go` validates local state and then starts
`runTurn` in a goroutine. `runTurn` calls `conn.Prompt`, and a prompt error is
reported as a terminal turn event. So an ACP `StartDeferredTurn` error is
definitively before the provider boundary, and it is wrapped with
`ports.ErrDeferredTurnNotStarted`. Any error the adapter cannot prove is
pre-send must not be wrapped: the controller then treats it conservatively as an
uncertain delivery.

A `BindTurnToProvider` failure means different things:

| Driver | Bind failure means | Row | One-shot | Outcome |
| --- | --- | --- | --- | --- |
| Eager | `SendTurn` already succeeded, so the provider may have accepted and run the turn | stays `running`, **no** provider id | **stays armed**, bound to the returned provider id | `state_uncertain` (409) |
| Deferred | the provider never received the prompt | settled `failed` | cleared | `CHAT_QUEUE_DISPATCH_FAILED` (409) |

For an eager bind failure the resume never reports `resumed`, never requeues the
row, never retries the send, and never dispatches the next turn. The hold is bound
to the provider id **before** the durable write, so a late terminal for that id is
still recognized as the resumed turn's own. The row stays `running`/unbound, so
the snapshot reports `state_uncertain`, every later resume is refused with zero
sends, and the controller stays busy until recovery settles the orphan. If the
late terminal does arrive, the marker is the safe correlation: the controller
settles that known AO turn by id (it does not pretend the provider binding was
persisted), so the row does not stay `running` forever.

For a deferred bind failure the prepared turn is discarded and the row is settled
`failed`; because the provider boundary was never crossed, the next turn may be
resumed normally.

An ambiguous deferred `StartDeferredTurn` error keeps the durable binding and the
one-shot, leaves the row `running`, reports `state_uncertain`, and settles only
when a terminal arrives. A later resume is refused with zero sends.

The durable dispatch guard and the explicit-resume pre-check use
`ListActiveRunningTurnsForController`, scoped to:

- `conversation_id` (another conversation never blocks);
- `handled_by_session_id` and `controller_generation` (a residual row from an
  obsolete controller epoch follows recovery/settlement instead of blocking a
  replacement forever);
- `state = 'running'`, `rolled_back_at IS NULL`, `promoted_to_turn_id IS NULL`
  (terminal, rolled-back and promoted rows never block);
- the same active-branch lineage the snapshot exposes.

### Failure classes

| When the failure happens | Turn row | One-shot | Outcome | Provider may have it | Another resume | Who settles |
| --- | --- | --- | --- | --- | --- | --- |
| before the CAS (head moved, decode/excerpt, store error) | `queued` or already terminal | cleared | `stale_queue_state` / `no_pending_work` / error | no | yes (re-read) | no one; the row never left the queue |
| eager `SendTurn` proves local/not-written or an RPC rejection | `failed` (settled by dispatch) | cleared | `CHAT_QUEUE_DISPATCH_FAILED` | no | yes | `dispatchTurn` settles it |
| eager `SendTurn` ambiguous (EOF/timeout/cancel/undecodable) | `running`, unbound | **armed** | `state_uncertain` (409) | **possibly yes** | **no** | a late `TurnStarted`+terminal, or recovery |
| after the provider may have accepted (eager `SendTurn` ok, bind failed) | `running`, unbound | **armed** | `state_uncertain` (409) | **possibly yes** | **no** | the late terminal by AO id, or recovery |
| deferred `StartDeferredTurn` proves not started | `failed` (binding persisted, provider never received) | cleared | `CHAT_QUEUE_DISPATCH_FAILED` | no | yes | `dispatchTurn` settles it |
| deferred `StartDeferredTurn` ambiguous (EOF/timeout/disconnect) | `running`, bound | **armed** | `state_uncertain` (409) | **possibly yes** | **no** | the late terminal |
| deferred bind fails before `StartDeferredTurn` | `failed` | cleared | `CHAT_QUEUE_DISPATCH_FAILED` | no | yes | `dispatchTurn` settles it |
| terminal event arrives after an uncertain delivery | terminal per the event | consumed | — | yes | only after the terminal | the event handler |

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

### Retained queue reason

When the one-shot turn's terminal is consumed **and a queued backlog exists**,
the controller records a **retained queue reason** distinct from the active hold:
`{turnID, generation}`. It means "a one-shot completed and the rest of the queue
is waiting for another explicit resume". A one-shot that leaves nothing queued
records nothing, so no hidden state can survive an empty queue. The snapshot
derives `held_after_manual_resume` from it while queued work exists; a
failed/interrupted one-shot keeps `held_after_terminal_failure`.

Policy for messages that arrive during the retention: **they join the retained
backlog** (option A). They are appended as `queued` behind the existing head and
are never dispatched directly, so FIFO order is preserved. The retention ends
when the backlog becomes empty.

- The automatic drain refuses to dispatch while the reason is active; only an
  explicit resume (`claim=true`) bypasses it, and it releases the current head.
- A successful explicit resume clears it before claiming the next turn.
- Cancelling every held row clears it; the snapshot becomes `idle` and a later
  normal `Send` dispatches immediately.
- Cancelling only the head keeps it; the new head is still held and must be named
  by the next explicit resume.
- Handoff, close, terminate and controller replacement clear it; the next
  generation never inherits it.
- `Send` checks the retention before dispatching when the controller is otherwise
  idle, and defensively clears a retention whose backlog is already empty.
- It is not persisted and is not a display status: the snapshot derives the
  observable state from durable rows plus the live controller.
- Cold start is unchanged; the reason only exists on a live controller.

### FIFO and explicit operations

A durable queued head is authoritative for order, not only a manual-resume
retention. A new `Send` — and the `SteerOrSend` fallback when there is no active
turn, even though it calls `sendLocked(..., false)` — appends behind the head and
never dispatches in front of it. Only the authorized drain or an explicit resume
dispatches the head. This closes the historical case where an idle controller with
a queued row dispatched a newly added message first.

While a retention is active:

- `RetryTurn` creates the retry as `queued` behind the head (the backlog is read
  before the retry row is inserted) and does not clear the retention; an explicit
  resume releases the head first.
- Reorder, cancel and edit mutate the queue without dispatching. Cancelling every
  held row clears the retention; cancelling only the head keeps it for the new
  head.
- Promote requires an acknowledged active turn; with the retention there is none,
  so it is refused and the backlog is untouched.
- Handoff clears the retention and blocks dispatch. Compaction may contact the
  provider but does not release the backlog; a later `Compacted` drain is blocked
  by the retention guard.
- A normal completed turn still drains through the existing path when no backlog
  hold applies.

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
- `backend/internal/storage/sqlite/store/conversation_active_running_test.go`: the
  running guard is scoped to the live controller epoch: active bound and unbound
  rows count; terminal, rolled-back, promoted, other-conversation and obsolete
  generation rows do not.
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
  is bound before `StartDeferredTurn`; a deferred `ErrDeferredTurnNotStarted`
  settles failed while an ambiguous EOF stays `state_uncertain` until its late
  terminal; an eager bind failure after `SendTurn` stays `state_uncertain` with
  the marker armed, and a late completed/failed/interrupted terminal settles the
  row by AO id, clears the marker, leaves the next turn queued, and allows the
  next explicit resume; an obsolete-generation running row does not block a
  current-generation resume.
- `backend/internal/service/chat/queue_recovery_eager_errors_test.go`: the eager
  `SendTurn` error split — `ErrTurnNotDelivered`/`ErrTurnRejected` settle failed,
  while EOF/timeout/cancellation/undecodable leaves the row running/unbound with
  `state_uncertain` and zero further sends; a late `TurnStarted` persists the
  binding and its terminal settles the exact row; no events fall to recovery
  without a replay.
- `backend/internal/service/chat/queue_dispatch_audit_test.go`: a new `Send`
  joins an existing durable head (including failed/recovered/interrupted holds);
  `RetryTurn` during a retention queues behind the head and does not clear it;
  the `SteerOrSend` fallback queues; reorder keeps the retention and changes the
  head; edit and promote do not dispatch; an unrelated completion that would
  normally drain cannot pass the retained head.
- `backend/internal/service/chat/queue_state_derivation_test.go`: a fresh queue, a
  completed terminal turn, a compaction, a handoff, and an absent controller are
  not labelled `held_after_terminal_failure`; a completed one-shot with queued
  work is `held_after_manual_resume` (no active turn), clears on the next resume,
  reappears after the next completion, and ends `idle`; a failed/interrupted
  one-shot stays `held_after_terminal_failure`; a normal completion never retains;
  handoff/close clear the retention and a replacement does not inherit it; a
  one-shot with no backlog records nothing and a later normal Send dispatches; a
  Send during a retained backlog queues behind the original head and the resume
  releases that head; cancelling the whole backlog clears the retention and the
  next Send dispatches; cancelling only the head keeps the retention for the new
  head and a stale resume is refused.
- `backend/internal/service/chat/controller_hold_queue_test.go`: the #4861 hold
  tests, unchanged.
- `backend/internal/httpd/controllers/conversation_queue_resume_test.go`: route
  statuses and stable codes, including missing turn and missing generation; the
  snapshot carries `held_after_manual_resume` with no active turn.
- `backend/internal/cli/session_queue_test.go`: the CLI reads the observable state
  and sends its head and generation; no-work path; flag overrides; missing and
  explicitly empty generation are usage errors; stale CAS and daemon faults
  surface as exit 1; misuse exits 2; `held_after_manual_resume` is printed with
  the explicit resume hint.
- `backend/internal/adapters/chatdriver/codexappserver/send_classification_test.go`:
  `classifySendTurnError` maps an RPC error to `ErrTurnRejected`, a closed-before-
  write to `ErrTurnNotDelivered`, and leaves EOF/close-after-write/timeout/
  cancellation/undecodable ambiguous; empty text is a local definitive error.

## Provider failure taxonomy and local evidence

This delivery adds diagnostics, not a recovery policy. It does not add a scheduler,
automatic canary, cold-start adoption, process lock, or controller lease. A
failed or recovered turn still holds the queue exactly as described above, and
`ao session resume-queue` is still the only way to release one held turn.

### Demonstrated classes

The durable vocabulary has only classes backed by a structured signal:

| Class | Evidence |
| --- | --- |
| `auth_required` | ACP JSON-RPC `-32000`, or Codex `CodexErrorInfo=unauthorized` |
| `rejected` | An explicit Codex `turn/start` RPC error response |
| `not_delivered` | A local empty message, a connection refused before the first write, or an ACP deferred start that provably did not cross the boundary |
| `uncertain` | EOF, timeout, cancellation after a write, an unreadable response, or a late connection close |
| `interrupted` | A deliberate local interrupt |
| `provider_internal` | ACP JSON-RPC `-32603`; other structured codes remain `unknown` until demonstrated |
| `unknown` | A demonstrated provider failure that does not match a narrower class, including Codex errors not proven ambiguous and other ACP request codes |

There is deliberately **no `rate_limited` and no `transient` class**. Prose such as
"rate limit exceeded", "try again", or "timeout" is never classified with string
matching. An RPC rejection carrying that prose remains `rejected`; an ACP
`RequestError` carrying it remains `provider_internal` only for `-32603`, or
`unknown` otherwise. Codex EOF/timeout/cancellation/close-after-write and
unreadable-response boundaries are the demonstrated `uncertain` cases; every
other Codex send error remains `unknown`. A future scheduler would need a stable
structured temporal signal (or an explicit product decision), not a text
heuristic.

`ErrTurnNotDelivered` means only that the instruction did not reach the provider.
It says nothing about temporary availability and is not a retry signal.

### Sanitized archive

The existing local provider-event archive is the only persistence route:
`conversation_provider_events.payload_json` under `AO_DATA_DIR`. There is no new
table, migration, telemetry exporter, network call, or second event file.

Classified terminal events gain one structured block:

```json
{
  "providerError": {
    "class": "provider_internal",
    "protocol": "acp",
    "harness": "opencode",
    "operation": "session/prompt",
    "code": -32603,
    "dataShape": { "error": "string" },
    "eventId": "validated-host-event-id"
  }
}
```

`dataShape` contains only allowlisted field names and JSON types. It never contains
a `data` value, a nested body, an array item, or a blob. Its bound is explicit: at
most the three allowlisted names (`error`, `eventId`, and the relay's
`ao.persistentEventId`), one known relay wrapper level, and no recursive copy.
`eventId` is copied only when AO's existing relay event-id validation accepts it;
the format check also rejects non-printable/whitespace values and values over 256
bytes. The block contains no provider message, prompt, response text, path, token,
cookie, credential, email, or raw `RequestError.Error()`. The `harness` identifies
the adapter family (`opencode`, `opencode-v2`, `codex`, etc.).

For a `SendTurn`/deferred-start failure with evidence, the same archive receives a
`provider.error` row through `ProjectProviderEvent` with a no-op projection. It
does not create a synthetic timeline event, settle a turn, or advance the queue.
For classified events, the archive's raw `error` field is the class; the existing
visible `error_message` on the durable turn still carries the provider's message.
Unclassified events retain their previous archive behavior.

### OpenCode representative case

The repository has no original frame from the reported OpenCode incident. Tests
use a clearly synthetic representative `-32603` frame with
`data.error = "Rate limit exceeded. Please try again later."`; it is not claimed
as forensic evidence. The tests prove that:

- it is `provider_internal` (never `rate_limited` or `transient`);
- `data.error` is absent from the structured evidence;
- `dataShape.error` is `"string"`;
- OpenCode and OpenCode v2 keep the same ACP semantics;
- no automatic retry or queue decision is made.

### Separation from behavior

The class is diagnostic evidence only. It does not change whether a turn settles
as failed/running/interrupted, whether a queue drains, whether a held queue can be
resumed, or how Start/Stop/handoff/live reconnect work. Existing regression tests
(`TestFailedPrimaryTurnHoldsQueuedWork`,
`TestRecoveredPrimaryTurnHoldsQueuedWork`, the uncertain-delivery tests, and the
`resume-queue` tests) must keep passing unchanged.

## Out of scope

- Preserving `queued` rows across a cold start (Option B).
- Re-queueing `failed` turns whose `started_at IS NULL`.
- Automatic retries. Delivery classification for a possibly-accepted turn is not
  sufficient for a safe second delivery (no `Retry-After` is preserved for chat
  turns, and ACP/OpenCode provider turn ids are AO-generated).
