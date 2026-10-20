package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Explicit, one-shot reanudation of a deliberately held chat queue.
//
// The hold after a failed or recovered turn is deliberate (issue #4861): the
// queue must not cascade into the same outage. This file adds the user-initiated
// counterpart — release exactly one never-sent turn — without weakening the
// automatic hold and without ever re-sending a turn whose delivery is uncertain.
//
// Safety rests on one durable fact: a row that is still state='queued' has not
// crossed the provider boundary (dispatch moves it to 'running' before the
// provider call, and ACP's deferred start binds it before session/prompt). So
// only 'queued' rows are ever released, and the release is a durable
// compare-and-set on the exact turn the caller named.

// QueueResumeOutcome is the domain result of an explicit queue resume.
type QueueResumeOutcome string

const (
	// QueueResumeResumed means exactly one never-sent turn was dispatched.
	QueueResumeResumed QueueResumeOutcome = "resumed"
	// QueueResumeStaleQueueState means the named turn is no longer the queue
	// head, or the durable compare-and-set refused it.
	QueueResumeStaleQueueState QueueResumeOutcome = "stale_queue_state"
	// QueueResumeNoPendingWork means there is no queued turn to release.
	QueueResumeNoPendingWork QueueResumeOutcome = "no_pending_work"
	// QueueResumeSessionBusy means a turn is already running.
	QueueResumeSessionBusy QueueResumeOutcome = "session_busy"
	// QueueResumeCompactionInProgress means a compaction is in flight.
	QueueResumeCompactionInProgress QueueResumeOutcome = "compaction_in_progress"
	// QueueResumeHandoffInProgress means a controller handoff is armed.
	QueueResumeHandoffInProgress QueueResumeOutcome = "handoff_in_progress"
	// QueueResumeControllerRecoveryRequired means there is no live controller.
	QueueResumeControllerRecoveryRequired QueueResumeOutcome = "controller_recovery_required"
	// QueueResumeStateUncertain means a running turn's delivery is unconfirmed.
	QueueResumeStateUncertain QueueResumeOutcome = "state_uncertain"
	// QueueResumeSessionTerminated means the session is terminated.
	QueueResumeSessionTerminated QueueResumeOutcome = "session_terminated"
	// QueueResumeStaleControllerGeneration means the controller that would
	// dispatch is no longer the durable owner of the session.
	QueueResumeStaleControllerGeneration QueueResumeOutcome = "stale_controller_generation"
)

// QueueResumeResult reports the outcome of an explicit queue resume. It never
// carries a provider turn id: ACP/OpenCode ids are AO-generated and do not prove
// the provider accepted anything, so exposing one would imply a guarantee that
// does not exist.
type QueueResumeResult struct {
	Outcome QueueResumeOutcome
	// TurnID is AO's id for the released turn, set only for QueueResumeResumed.
	TurnID string
}

// ErrResumeTurnIDRequired reports a resume call that did not name the queue head.
// The CAS is the concurrency guard, so the head id is mandatory.
var ErrResumeTurnIDRequired = errors.New("expectedTurnId is required")

// ErrResumeControllerGenerationRequired reports a resume call that did not name
// the controller generation. An empty generation is never interpreted as "use
// the current one": the caller must name the epoch it observed so a stale caller
// cannot silently resume against a replacement controller.
var ErrResumeControllerGenerationRequired = errors.New("expectedControllerGeneration is required")

// ErrStaleControllerGeneration reports that the controller selected for a resume
// is no longer the durable owner of the session. The client should re-read the
// conversation and retry against the current generation.
var ErrStaleControllerGeneration = errors.New("chat controller generation changed")

// ErrSessionTerminated reports a resume against a terminated session.
var ErrSessionTerminated = errors.New("session is terminated")

// ErrQueueDispatchFailed reports that the released turn could not be handed to
// the provider. dispatch already settled that turn failed; the rest of the queue
// is untouched.
var ErrQueueDispatchFailed = errors.New("the released turn was not accepted by the provider")

// errUncertainEagerDelivery reports an eager SendTurn whose provider turn id
// could not be written. The provider may already have accepted and run the turn,
// so this is an uncertain delivery — not an undispatched turn. The row stays
// running/unbound, the one-shot hold stays armed, and no automatic drain follows.
var errUncertainEagerDelivery = errors.New("provider accepted the turn but its id could not be recorded")

// errResumeNoPendingWork and errResumeStaleQueueState are the internal signals
// drainLockedFor returns for the explicit-resume path only.
var (
	errResumeNoPendingWork   = errors.New("no queued turn to resume")
	errResumeStaleQueueState = errors.New("queued turn is no longer the queue head")
)

// resumeHold is the one-shot explicit-resume state. It is bound to the exact AO
// turn and the controller generation it was resumed under, so a terminal event
// for any other turn (or a stale generation) never suppresses the automatic
// drain. providerTurnID is filled after dispatch binds the row.
type resumeHold struct {
	turnID         string
	generation     string
	providerTurnID string
}

// armResumeHold records the one-shot before dispatch begins. The caller must
// hold sendMu so the terminal event cannot be projected until the hold is armed.
func (c *Controller) armResumeHold(turnID string) {
	c.mu.Lock()
	c.resumeHold = &resumeHold{turnID: turnID, generation: c.generation}
	c.mu.Unlock()
}

// clearResumeHold retires the one-shot on failure, interrupt, handoff or close.
func (c *Controller) clearResumeHold() {
	c.mu.Lock()
	c.resumeHold = nil
	c.mu.Unlock()
}

// consumeResumeHold clears and reports whether a primary terminal event belongs
// to the one-shot resumed turn. Only a matching event suppresses the automatic
// drain; an ordinary completed turn is unaffected.
func (c *Controller) consumeResumeHold(providerTurnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	hold := c.resumeHold
	if hold == nil || hold.generation != c.generation {
		return false
	}
	if hold.providerTurnID == "" || hold.providerTurnID != providerTurnID {
		return false
	}
	c.resumeHold = nil
	return true
}

// ResumeQueuedTurn releases exactly one never-sent queued turn named by
// expectedTurnID for the controller epoch named by expectedControllerGeneration.
// The caller must supply validate, which re-proves — under the dispatch lock —
// that this controller is still the registered, durable owner of the session.
//
// The controller itself refuses any generation other than its own before the
// durable claim, and the claim's compare-and-set re-checks both the row's
// generation and the handling session's live generation in one statement. So a
// controller that lost ownership — even one that passed the caller's preliminary
// validate — cannot claim, and can never claim a turn that belongs to a newer
// generation.
func (c *Controller) ResumeQueuedTurn(
	ctx context.Context,
	expectedTurnID, expectedControllerGeneration string,
	validate func() error,
) (QueueResumeResult, error) {
	if expectedControllerGeneration == "" || expectedControllerGeneration != c.generation {
		return QueueResumeResult{}, ErrStaleControllerGeneration
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	if err := validate(); err != nil {
		return QueueResumeResult{}, err
	}
	if c.HandoffActive() {
		return QueueResumeResult{Outcome: QueueResumeHandoffInProgress}, nil
	}
	running, err := c.store.ListVisibleRunningTurnProviderIDs(ctx, c.conversation.ID)
	if err != nil {
		return QueueResumeResult{}, err
	}
	for _, providerTurnID := range running {
		if providerTurnID == "" {
			// A running turn with no provider binding is a dispatch whose delivery
			// is unconfirmed. Releasing the next turn could run two turns at once.
			return QueueResumeResult{Outcome: QueueResumeStateUncertain}, nil
		}
	}
	if len(running) > 0 {
		return QueueResumeResult{Outcome: QueueResumeSessionBusy}, nil
	}
	if c.Compacting() {
		return QueueResumeResult{Outcome: QueueResumeCompactionInProgress}, nil
	}
	if c.Busy() {
		return QueueResumeResult{Outcome: QueueResumeSessionBusy}, nil
	}

	// Arm the one-shot before dispatch so the resumed turn's terminal event holds
	// the queue instead of releasing the next turn.
	c.armResumeHold(expectedTurnID)
	if err := c.drainLockedFor(ctx, true, expectedTurnID, expectedControllerGeneration, true); err != nil {
		if errors.Is(err, errUncertainEagerDelivery) {
			// The eager provider boundary was crossed but AO could not record the
			// provider id. Keep the one-shot armed so a late terminal for this
			// provider id still holds the queue instead of releasing the next turn.
			// The durable running row keeps the snapshot at state_uncertain.
			return QueueResumeResult{Outcome: QueueResumeStateUncertain}, nil
		}
		c.clearResumeHold()
		switch {
		case errors.Is(err, errResumeNoPendingWork):
			return QueueResumeResult{Outcome: QueueResumeNoPendingWork}, nil
		case errors.Is(err, errResumeStaleQueueState):
			return QueueResumeResult{Outcome: QueueResumeStaleQueueState}, nil
		default:
			// dispatch already settled the attempted row failed; the remaining
			// queue is untouched. Surface the provider failure to the caller.
			return QueueResumeResult{}, errors.Join(ErrQueueDispatchFailed, err)
		}
	}
	// drainLockedFor can return nil without dispatching: it re-reads the busy and
	// handoff state under the lock, and a drain that finds either returns early.
	// A dispatched turn has left the queue (queued -> running), so a row still in
	// 'queued' proves nothing was released. Retire the one-shot instead of leaving
	// it armed as if a one-shot turn were in flight.
	turn, lookupErr := c.store.TurnByID(ctx, expectedTurnID)
	if lookupErr != nil {
		c.clearResumeHold()
		return QueueResumeResult{}, lookupErr
	}
	if turn.State == domain.TurnStateQueued {
		c.clearResumeHold()
		if c.HandoffActive() {
			return QueueResumeResult{Outcome: QueueResumeHandoffInProgress}, nil
		}
		return QueueResumeResult{Outcome: QueueResumeSessionBusy}, nil
	}
	return QueueResumeResult{Outcome: QueueResumeResumed, TurnID: expectedTurnID}, nil
}

// ResumeHeldQueue releases exactly one never-sent queued turn for a session.
//
// The server is the authority on the controller generation: it re-reads the
// durable session row under the dispatch lock and refuses when the selected
// controller is no longer the owner. expectedControllerGeneration is only an
// optimistic CAS for a client acting on a snapshot it already holds.
//
// This first delivery resumes only while a live, authoritative controller owns a
// durable queued row. It deliberately does not recover after a cold start, where
// SettleOrphanedTurns has already closed the queue.
func (s *Service) ResumeHeldQueue(
	ctx context.Context,
	id domain.SessionID,
	expectedTurnID, expectedControllerGeneration string,
) (QueueResumeResult, error) {
	if strings.TrimSpace(expectedTurnID) == "" {
		return QueueResumeResult{}, ErrResumeTurnIDRequired
	}
	if strings.TrimSpace(expectedControllerGeneration) == "" {
		return QueueResumeResult{}, ErrResumeControllerGenerationRequired
	}
	record, err := s.requireChatSession(ctx, id)
	if err != nil {
		return QueueResumeResult{}, err
	}
	if record.IsTerminated {
		return QueueResumeResult{Outcome: QueueResumeSessionTerminated}, nil
	}

	controller, release, err := s.workingController(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNoController) {
			return QueueResumeResult{Outcome: QueueResumeControllerRecoveryRequired}, nil
		}
		return QueueResumeResult{}, err
	}
	defer release()

	// Hold the session's controller gate across the whole resume: validation,
	// the durable claim, and the provider boundary. Start/Stop/branch-handoff
	// install and replace controllers under this same gate, and the durable
	// controller_generation only changes inside Start while it holds the gate.
	// So while this section runs the epoch cannot change, which is the shared
	// synchronization boundary between "this controller owns the session" and
	// "the provider is called". Lock order is gate -> sendMu (inside
	// ResumeQueuedTurn), matching lockCurrentSide; nothing takes sendMu then the
	// gate, so there is no inversion.
	gate := s.controllerGate(domain.SessionConversationOwner(id))
	if err := gate.lock(ctx); err != nil {
		return QueueResumeResult{}, err
	}
	defer gate.unlock()

	generation := controller.Generation()
	validate := func() error {
		current, err := s.Controller(id)
		if err != nil {
			return ErrNoController
		}
		if current != controller {
			return ErrStaleControllerGeneration
		}
		durable, found, err := s.sessions.GetSession(ctx, id)
		if err != nil {
			return fmt.Errorf("read session %s: %w", id, err)
		}
		if !found {
			return ports.ErrSessionNotFound
		}
		if durable.IsTerminated {
			return ErrSessionTerminated
		}
		if durable.Metadata.ControllerGeneration != generation {
			return ErrStaleControllerGeneration
		}
		if expectedControllerGeneration != generation {
			return ErrStaleControllerGeneration
		}
		return nil
	}
	return controller.ResumeQueuedTurn(ctx, expectedTurnID, expectedControllerGeneration, validate)
}

// QueueState is the derived, never-persisted view of a conversation's send queue
// and controller readiness. Every field is computed from durable rows plus the
// live controller; none of it is stored.
type QueueState struct {
	// State distinguishes an idle session from a held queue. See the constants
	// below; it never labels a long-running or merely idle session as blocked.
	State QueueStateValue `json:"state"`
	// PendingCount is the number of never-sent (queued) turns.
	PendingCount int `json:"pendingCount"`
	// OldestPendingAt is when the oldest never-sent turn was recorded.
	OldestPendingAt *time.Time `json:"oldestPendingAt,omitempty"`
	// HeadTurnID is the queue head a resume must name, empty when none.
	HeadTurnID string `json:"headTurnId,omitempty"`
	// ControllerGeneration is the durable generation of the live controller, the
	// value a client echoes back as expectedControllerGeneration.
	ControllerGeneration string `json:"controllerGeneration,omitempty"`
	// ControllerAvailable reports whether a live controller owns the session.
	ControllerAvailable bool `json:"controllerAvailable"`
	// ActiveTurnID is the running turn AO currently owns, empty when none.
	ActiveTurnID string `json:"activeTurnId,omitempty"`
	// CompactionPending reports a compaction the provider accepted and has not
	// finished.
	CompactionPending bool `json:"compactionPending"`
	// ResumeSafety is the outcome an explicit resume would return right now.
	ResumeSafety QueueResumeOutcome `json:"resumeSafety"`
}

// QueueStateValue classifies the queue for a client without persisting anything.
type QueueStateValue string

const (
	// QueueStateIdle: controller ready, no running turn, empty queue.
	QueueStateIdle QueueStateValue = "idle"
	// QueueStateRunning: a bound turn is in flight.
	QueueStateRunning QueueStateValue = "running"
	// QueueStateCompacting: a compaction is in flight.
	QueueStateCompacting QueueStateValue = "compacting"
	// QueueStateWaitingForActiveTurn: queued turns wait behind an active turn.
	QueueStateWaitingForActiveTurn QueueStateValue = "queued_waiting_for_active_turn"
	// QueueStateHeldAfterFailure: queued turns are held behind a non-completed
	// terminal turn and nothing is running.
	QueueStateHeldAfterFailure QueueStateValue = "held_after_terminal_failure"
	// QueueStateControllerRecoveryRequired: pending work but no live controller.
	QueueStateControllerRecoveryRequired QueueStateValue = "controller_recovery_required"
	// QueueStateHandoffInProgress: a controller handoff is armed.
	QueueStateHandoffInProgress QueueStateValue = "handoff_in_progress"
	// QueueStateUncertain: a running turn's delivery is unconfirmed.
	QueueStateUncertain QueueStateValue = "state_uncertain"
)

// deriveQueueState computes the observable queue view. controller may be nil.
func deriveQueueState(
	record domain.SessionRecord,
	facts domain.ConversationQueueFacts,
	head *domain.QueuedTurn,
	controller *Controller,
) QueueState {
	state := QueueState{
		PendingCount:         facts.PendingCount,
		ControllerGeneration: record.Metadata.ControllerGeneration,
		ControllerAvailable:  controller != nil && controller.State() != ports.ChatControllerStopped,
		ActiveTurnID:         facts.ActiveTurnID,
	}
	if head != nil {
		state.HeadTurnID = head.TurnID
		if !head.RequestedAt.IsZero() {
			at := head.RequestedAt
			state.OldestPendingAt = &at
		}
	}
	var compactionPending, handoffInProgress bool
	if controller != nil {
		compactionPending = controller.Compacting()
		handoffInProgress = controller.HandoffActive()
	}
	state.CompactionPending = compactionPending

	runningBound := facts.RunningCount-facts.RunningUnboundCount > 0
	runningUnbound := facts.RunningUnboundCount > 0
	// A queue is held only when a terminal turn that actually withholds the queue
	// left work behind. A completed turn drains; a cancelled turn was withdrawn
	// before dispatch and does not hold anything. A session with no terminal turn
	// yet is a fresh queue that has not had its chance to dispatch, not a hold.
	heldAfterFailure := facts.PendingCount > 0 && facts.RunningCount == 0 &&
		(facts.LatestTerminalState == domain.TurnStateFailed ||
			facts.LatestTerminalState == domain.TurnStateRecovered ||
			facts.LatestTerminalState == domain.TurnStateInterrupted)

	switch {
	case record.IsTerminated:
		state.State = QueueStateControllerRecoveryRequired
		state.ResumeSafety = QueueResumeSessionTerminated
	case handoffInProgress:
		state.State = QueueStateHandoffInProgress
		state.ResumeSafety = QueueResumeHandoffInProgress
	case runningUnbound:
		state.State = QueueStateUncertain
		state.ResumeSafety = QueueResumeStateUncertain
	case runningBound:
		if facts.PendingCount > 0 {
			state.State = QueueStateWaitingForActiveTurn
		} else {
			state.State = QueueStateRunning
		}
		state.ResumeSafety = QueueResumeSessionBusy
	case compactionPending:
		state.State = QueueStateCompacting
		state.ResumeSafety = QueueResumeCompactionInProgress
	case facts.PendingCount == 0:
		state.State = QueueStateIdle
		state.ResumeSafety = QueueResumeNoPendingWork
	case !state.ControllerAvailable:
		state.State = QueueStateControllerRecoveryRequired
		state.ResumeSafety = QueueResumeControllerRecoveryRequired
	case heldAfterFailure:
		state.State = QueueStateHeldAfterFailure
		state.ResumeSafety = QueueResumeResumed
	default:
		// Queued work exists with a live controller and no active turn, but the
		// last terminal turn completed: the queue is not held by a failure, it is
		// pending processing (or held by a one-shot resume). Report it as waiting
		// rather than as a held failure.
		state.State = QueueStateWaitingForActiveTurn
		state.ResumeSafety = QueueResumeResumed
	}
	return state
}
