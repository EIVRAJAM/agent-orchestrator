package codexappserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestClassifySendTurnError proves the adapter only claims a definitive outcome
// when it has one. A provider error response is a rejection; a connection
// refused before any write is not delivered. Everything else stays ambiguous so
// the controller leaves the turn running/uncertain.
func TestClassifySendTurnError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		delivered bool
		rejected  bool
	}{
		{"rpc rejection", fmt.Errorf("turn/start: %w", &rpcError{Code: -1, Message: "bad request"}), false, true},
		{"closed before write", fmt.Errorf("turn/start: %w: %w", ErrConnClosed, errConnClosedBeforeWrite), true, false},
		{"eof", io.EOF, false, false},
		{"closed after write", fmt.Errorf("turn/start: %w", ErrConnClosed), false, false},
		{"timeout", context.DeadlineExceeded, false, false},
		{"cancelled", context.Canceled, false, false},
		{"undecodable response", fmt.Errorf("turn/start: decode result: %w", errors.New("invalid character")), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySendTurnError(tc.err)
			if errors.Is(got, ports.ErrTurnNotDelivered) != tc.delivered {
				t.Fatalf("ErrTurnNotDelivered = %v, want %v (err %v)", errors.Is(got, ports.ErrTurnNotDelivered), tc.delivered, got)
			}
			if errors.Is(got, ports.ErrTurnRejected) != tc.rejected {
				t.Fatalf("ErrTurnRejected = %v, want %v (err %v)", errors.Is(got, ports.ErrTurnRejected), tc.rejected, got)
			}
			if !tc.delivered && !tc.rejected && !errors.Is(got, tc.err) {
				t.Fatalf("ambiguous error lost its chain: %v", got)
			}
		})
	}
}

// TestSendTurnEmptyTextIsDefinitive proves a local validation failure is
// classified before touching the wire.
func TestSendTurnEmptyTextIsDefinitive(t *testing.T) {
	c := &conversation{}
	_, err := c.SendTurn(context.Background(), ports.ChatUserMessage{Text: "  "})
	if !errors.Is(err, ports.ErrTurnNotDelivered) {
		t.Fatalf("err = %v, want ErrTurnNotDelivered", err)
	}
	if errors.Is(err, ports.ErrTurnRejected) {
		t.Fatalf("local validation error = %v, must not be a provider rejection", err)
	}
}
