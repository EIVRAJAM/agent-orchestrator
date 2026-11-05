package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrackerWritebacksQueriesBoundedReadOnlyStates(t *testing.T) {
	cfg := setConfigEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/tracker/writebacks" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("status") != "uncertain" || q.Get("limit") != "2" || q.Get("offset") != "1" {
			t.Errorf("query = %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{
				"id": "wb-1", "projectId": "demo", "sessionId": "demo-1",
				"status": "uncertain", "attempts": 1, "failureCode": "remote_uncertain",
				"createdAt": "1970-01-01T00:01:40Z", "updatedAt": "1970-01-01T00:01:41Z",
			}},
			"limit": 2, "offset": 1, "hasMore": false,
		})
	}))
	t.Cleanup(server.Close)
	writeRunFileFor(t, cfg, server)

	out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"tracker", "writebacks", "--status", "uncertain", "--limit", "2", "--offset", "1", "--json")
	if err != nil {
		t.Fatalf("tracker writebacks: %v\nstderr=%s", err, stderr)
	}
	var response trackerWritebackListDTO
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatalf("decode: %v: %s", err, out)
	}
	if response.Limit != 2 || response.Offset != 1 || len(response.Items) != 1 || response.Items[0].ID != "wb-1" {
		t.Fatalf("response = %+v", response)
	}
}
