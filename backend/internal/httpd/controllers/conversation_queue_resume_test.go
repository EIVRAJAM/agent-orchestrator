package controllers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type resumeQueueStub struct {
	*fakeConversationService
	session    domain.SessionID
	turnID     string
	generation string
	result     chatsvc.QueueResumeResult
	err        error
}

func (s *resumeQueueStub) ResumeHeldQueue(
	_ context.Context,
	session domain.SessionID,
	turnID, generation string,
) (chatsvc.QueueResumeResult, error) {
	s.session, s.turnID, s.generation = session, turnID, generation
	return s.result, s.err
}

func postResumeQueue(t *testing.T, conversations controllers.ConversationService, request map[string]any) (int, map[string]any) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		Sessions:      newFakeSessionService(),
		Conversations: conversations,
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	body, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	resp, err := http.Post(
		srv.URL+"/api/v1/sessions/p1-1/conversation/queue/resume",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST queue/resume: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil && err != io.EOF {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, decoded
}

func TestResumeQueuedTurnRouteResumed(t *testing.T) {
	svc := &resumeQueueStub{
		fakeConversationService: &fakeConversationService{},
		result:                  chatsvc.QueueResumeResult{Outcome: chatsvc.QueueResumeResumed, TurnID: "turn-1"},
	}
	status, body := postResumeQueue(t, svc, map[string]any{
		"expectedTurnId": "turn-1", "expectedControllerGeneration": "gen-1",
	})
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", status, http.StatusAccepted)
	}
	if body["outcome"] != "resumed" || body["turnId"] != "turn-1" {
		t.Fatalf("body = %v, want resumed turn-1", body)
	}
	if svc.session != "p1-1" || svc.turnID != "turn-1" || svc.generation != "gen-1" {
		t.Fatalf("svc saw session=%q turn=%q generation=%q", svc.session, svc.turnID, svc.generation)
	}
}

func TestResumeQueuedTurnRouteNoPendingWork(t *testing.T) {
	svc := &resumeQueueStub{
		fakeConversationService: &fakeConversationService{},
		result:                  chatsvc.QueueResumeResult{Outcome: chatsvc.QueueResumeNoPendingWork},
	}
	status, body := postResumeQueue(t, svc, map[string]any{"expectedTurnId": "turn-1"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if body["outcome"] != "no_pending_work" {
		t.Fatalf("body = %v, want no_pending_work", body)
	}
}

func TestResumeQueuedTurnRouteConflictOutcome(t *testing.T) {
	for _, tc := range []struct {
		outcome chatsvc.QueueResumeOutcome
		code    string
	}{
		{chatsvc.QueueResumeStaleQueueState, "CHAT_QUEUE_STALE_STATE"},
		{chatsvc.QueueResumeSessionBusy, "CHAT_TURN_RUNNING"},
		{chatsvc.QueueResumeCompactionInProgress, "CHAT_COMPACTION_BUSY"},
		{chatsvc.QueueResumeHandoffInProgress, "CHAT_INTERFACE_TRANSITION"},
		{chatsvc.QueueResumeControllerRecoveryRequired, "CHAT_CONTROLLER_NOT_READY"},
		{chatsvc.QueueResumeStateUncertain, "CHAT_QUEUE_STATE_UNCERTAIN"},
		{chatsvc.QueueResumeSessionTerminated, "SESSION_TERMINATED"},
		{chatsvc.QueueResumeStaleControllerGeneration, "CHAT_QUEUE_STALE_CONTROLLER"},
	} {
		svc := &resumeQueueStub{
			fakeConversationService: &fakeConversationService{},
			result:                  chatsvc.QueueResumeResult{Outcome: tc.outcome},
		}
		status, body := postResumeQueue(t, svc, map[string]any{"expectedTurnId": "turn-1"})
		if status != http.StatusConflict {
			t.Fatalf("outcome %q: status = %d, want %d", tc.outcome, status, http.StatusConflict)
		}
		if body["code"] != tc.code {
			t.Fatalf("outcome %q: code = %v, want %s", tc.outcome, body["code"], tc.code)
		}
	}
}

func TestResumeQueuedTurnRouteMissingTurn(t *testing.T) {
	svc := &resumeQueueStub{
		fakeConversationService: &fakeConversationService{},
		err:                     chatsvc.ErrResumeTurnIDRequired,
	}
	status, body := postResumeQueue(t, svc, map[string]any{"expectedTurnId": ""})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if body["code"] != "CHAT_QUEUE_RESUME_TURN_REQUIRED" {
		t.Fatalf("code = %v, want CHAT_QUEUE_RESUME_TURN_REQUIRED", body["code"])
	}
}

func TestResumeQueuedTurnRouteNotImplemented(t *testing.T) {
	// A service without the optional interface must answer 501, not 500.
	status, _ := postResumeQueue(t, &fakeConversationService{}, map[string]any{"expectedTurnId": "turn-1"})
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", status, http.StatusNotImplemented)
	}
}
