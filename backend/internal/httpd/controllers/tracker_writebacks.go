package controllers

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// TrackerWritebackService is the read-only outbox inspection contract.
type TrackerWritebackService interface {
	List(ctx context.Context, status string, limit, offset int) ([]domain.TrackerWriteback, bool, error)
}

// TrackerWritebacksController serves the read-only outbox inspection route.
type TrackerWritebacksController struct{ Svc TrackerWritebackService }

// Register mounts the tracker write-back inspection route.
func (c *TrackerWritebacksController) Register(r chi.Router) {
	r.Get("/tracker/writebacks", c.list)
}

func (c *TrackerWritebacksController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/tracker/writebacks")
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	switch status {
	case "", domain.TrackerWritebackPending, domain.TrackerWritebackClaimed,
		domain.TrackerWritebackSent, domain.TrackerWritebackUncertain, domain.TrackerWritebackFailed:
	default:
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "TRACKER_WRITEBACK_STATUS_INVALID", "Unsupported tracker write-back status", nil)
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "TRACKER_WRITEBACK_LIMIT_INVALID", "limit must be between 1 and 100", nil)
			return
		}
		limit = n
	}
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "TRACKER_WRITEBACK_OFFSET_INVALID", "offset must be zero or greater", nil)
			return
		}
		offset = n
	}
	rows, hasMore, err := c.Svc.List(r.Context(), status, limit, offset)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	items := make([]TrackerWritebackView, len(rows))
	for i, row := range rows {
		items[i] = TrackerWritebackView{
			ID: row.ID, ProjectID: string(row.ProjectID), SessionID: string(row.SessionID),
			Status: row.Status, Attempts: row.Attempts, FailureCode: row.FailureCode,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ClaimedAt: row.ClaimedAt,
		}
	}
	envelope.WriteJSON(w, http.StatusOK, ListTrackerWritebacksResponse{
		Items: items, Limit: limit, Offset: offset, HasMore: hasMore,
	})
}
