package controllers

import "time"

// ListTrackerWritebacksQuery bounds the read-only outbox inspection.
type ListTrackerWritebacksQuery struct {
	Status string `query:"status,omitempty" enum:"pending,claimed,sent,uncertain,failed" description:"Optional durable outbox status."`
	Limit  int    `query:"limit,omitempty" minimum:"1" maximum:"100" description:"Maximum rows to return. Defaults to 50."`
	Offset int    `query:"offset,omitempty" minimum:"0" description:"Rows to skip."`
}

// TrackerWritebackView is the sanitized inspection projection. It deliberately
// omits issue id, PR URL, comment URL, comment body, and credentials.
type TrackerWritebackView struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"projectId,omitempty"`
	SessionID   string     `json:"sessionId"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	FailureCode string     `json:"failureCode,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ClaimedAt   *time.Time `json:"claimedAt,omitempty"`
}

// ListTrackerWritebacksResponse is one bounded page of outbox state.
type ListTrackerWritebacksResponse struct {
	Items   []TrackerWritebackView `json:"items"`
	Limit   int                    `json:"limit"`
	Offset  int                    `json:"offset"`
	HasMore bool                   `json:"hasMore"`
}
