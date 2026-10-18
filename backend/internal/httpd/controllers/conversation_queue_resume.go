package controllers

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

// resumeQueuedTurnsPath is the one route this file owns, named once so the
// not-implemented answer and the spec cannot drift apart.
const resumeQueuedTurnsPath = "/api/v1/sessions/{sessionId}/conversation/queue/resume"

// conversationQueuePayload maps the derived queue view onto the wire shape. It
// is nil for a reviewer-owned snapshot, which has no session queue.
func conversationQueuePayload(q *chatsvc.QueueState) *ConversationQueuePayload {
	if q == nil {
		return nil
	}
	return &ConversationQueuePayload{
		QueueState:           string(q.State),
		PendingCount:         q.PendingCount,
		OldestPendingAt:      optionalTimestamp(q.OldestPendingAt),
		HeadTurnID:           q.HeadTurnID,
		ControllerGeneration: q.ControllerGeneration,
		ControllerAvailable:  q.ControllerAvailable,
		ActiveTurn:           q.ActiveTurnID,
		CompactionPending:    q.CompactionPending,
		ResumeSafety:         string(q.ResumeSafety),
	}
}

// queueResumeService is feature-detected so an older service still answers 501
// rather than failing to satisfy ConversationService.
type queueResumeService interface {
	ResumeHeldQueue(ctx context.Context, session domain.SessionID, expectedTurnID, expectedControllerGeneration string) (chatsvc.QueueResumeResult, error)
}

// resumeHeldQueue releases exactly one never-sent queued turn.
//
// This first delivery resumes only while a live, authoritative controller owns a
// durable queued row; it does not recover after a cold start, where the queue has
// already been closed. See docs/chat-queue-resume.md.
func (c *ConversationsController) resumeHeldQueue(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(queueResumeService)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodPost, resumeQueuedTurnsPath)
		return
	}
	var req ResumeConversationQueueRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	result, err := svc.ResumeHeldQueue(
		r.Context(),
		domain.SessionID(chi.URLParam(r, "sessionId")),
		req.ExpectedTurnID,
		req.ExpectedControllerGeneration,
	)
	if err != nil {
		writeQueueResumeError(w, r, err)
		return
	}
	switch result.Outcome {
	case chatsvc.QueueResumeResumed:
		// 202: the turn is accepted and the provider acts on it asynchronously.
		envelope.WriteJSON(w, http.StatusAccepted, ResumeConversationQueueResponse{
			Outcome: string(result.Outcome), TurnID: result.TurnID,
		})
	case chatsvc.QueueResumeNoPendingWork:
		envelope.WriteJSON(w, http.StatusOK, ResumeConversationQueueResponse{
			Outcome: string(result.Outcome),
		})
	default:
		writeQueueResumeConflict(w, r, result.Outcome)
	}
}

// writeQueueResumeConflict maps the expected-state outcomes to a conflict with a
// stable code. The outcome name and the code are a deliberate 1:1 mapping so a
// client can branch on either.
func writeQueueResumeConflict(w http.ResponseWriter, r *http.Request, outcome chatsvc.QueueResumeOutcome) {
	switch outcome {
	case chatsvc.QueueResumeStaleQueueState:
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_QUEUE_STALE_STATE",
			"that turn is no longer the queue head; re-read the conversation and retry", nil)
	case chatsvc.QueueResumeSessionBusy:
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_TURN_RUNNING",
			"a turn is already in flight; the queue continues when it ends", nil)
	case chatsvc.QueueResumeCompactionInProgress:
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_COMPACTION_BUSY",
			"a compaction is in progress; retry after it finishes", nil)
	case chatsvc.QueueResumeHandoffInProgress:
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_INTERFACE_TRANSITION",
			"the session is switching interfaces; retry after the handoff completes", nil)
	case chatsvc.QueueResumeControllerRecoveryRequired:
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_CONTROLLER_NOT_READY",
			"the agent controller for this session is not running; start or restore the session first", nil)
	case chatsvc.QueueResumeStateUncertain:
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_QUEUE_STATE_UNCERTAIN",
			"a turn may still be running with unconfirmed delivery; the queue will not be released", nil)
	case chatsvc.QueueResumeSessionTerminated:
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"SESSION_TERMINATED", "the session is terminated", nil)
	case chatsvc.QueueResumeStaleControllerGeneration:
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_QUEUE_STALE_CONTROLLER",
			"the chat controller changed; re-read the conversation and retry", nil)
	default:
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_QUEUE_RESUME_FAILED", "the queue could not be resumed", nil)
	}
}

func writeQueueResumeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, chatsvc.ErrResumeTurnIDRequired):
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation",
			"CHAT_QUEUE_RESUME_TURN_REQUIRED",
			"expectedTurnId is required: re-read the conversation and name its queue head", nil)
	case errors.Is(err, chatsvc.ErrQueueDispatchFailed):
		// The attempted row was settled failed; the rest of the queue is intact.
		// A retry releases the next turn, and the failed turn keeps its own retry.
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_QUEUE_DISPATCH_FAILED",
			"the provider did not accept the released turn; the rest of the queue is unchanged", nil)
	case errors.Is(err, chatsvc.ErrStaleControllerGeneration):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_QUEUE_STALE_CONTROLLER",
			"the chat controller changed; re-read the conversation and retry", nil)
	case errors.Is(err, chatsvc.ErrSessionTerminated):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"SESSION_TERMINATED", "the session is terminated", nil)
	default:
		writeConversationError(w, r, err)
	}
}
