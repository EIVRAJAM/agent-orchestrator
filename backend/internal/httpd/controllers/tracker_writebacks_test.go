package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeTrackerWritebackLister struct {
	status, limit, offset int
	rows                  []domain.TrackerWriteback
	hasMore               bool
}

func (f *fakeTrackerWritebackLister) List(_ context.Context, status string, limit, offset int) ([]domain.TrackerWriteback, bool, error) {
	f.status = len(status)
	f.limit, f.offset = limit, offset
	return f.rows, f.hasMore, nil
}

func TestTrackerWritebacksListIsBoundedAndSanitized(t *testing.T) {
	svc := &fakeTrackerWritebackLister{
		rows: []domain.TrackerWriteback{{
			ID: "wb-1", ProjectID: "proj", SessionID: "proj-1",
			IssueID: "github:acme/demo#12", PRURL: "https://github.com/acme/demo/pull/1",
			Status: domain.TrackerWritebackUncertain, Attempts: 1, FailureCode: "remote_uncertain",
			CreatedAt: time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(101, 0).UTC(),
		}},
		hasMore: true,
	}
	r := chi.NewRouter()
	(&TrackerWritebacksController{Svc: svc}).Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tracker/writebacks?status=uncertain&limit=10&offset=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items   []map[string]any `json:"items"`
		Limit   int              `json:"limit"`
		Offset  int              `json:"offset"`
		HasMore bool             `json:"hasMore"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Limit != 10 || body.Offset != 2 || !body.HasMore || len(body.Items) != 1 {
		t.Fatalf("page = %+v", body)
	}
	item := body.Items[0]
	if item["id"] != "wb-1" || item["status"] != "uncertain" || item["failureCode"] != "remote_uncertain" {
		t.Fatalf("item = %+v", item)
	}
	for _, forbidden := range []string{"issueId", "prUrl", "commentUrl", "body", "prompt"} {
		if _, ok := item[forbidden]; ok {
			t.Fatalf("response leaked %q: %+v", forbidden, item)
		}
	}
}

func TestTrackerWritebacksListRejectsInvalidFilters(t *testing.T) {
	svc := &fakeTrackerWritebackLister{}
	r := chi.NewRouter()
	(&TrackerWritebacksController{Svc: svc}).Register(r)
	for _, path := range []string{
		"/tracker/writebacks?status=nope",
		"/tracker/writebacks?limit=0",
		"/tracker/writebacks?limit=101",
		"/tracker/writebacks?offset=-1",
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", path, rec.Code)
		}
	}
}
