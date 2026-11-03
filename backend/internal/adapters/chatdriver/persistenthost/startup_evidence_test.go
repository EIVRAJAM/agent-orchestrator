package persistenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartupEvidenceReadRejectsOversizedFile(t *testing.T) {
	dataDir := t.TempDir()
	const sessionID = "startup-evidence-oversized"
	const attemptID = "oversized-attempt-0123456789abcdef"

	payload, err := json.Marshal(startupEvidence{
		Version:   startupEvidenceVersion,
		AttemptID: attemptID,
		Stage:     StartupStageProviderExit,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, bytes.Repeat([]byte(" "), int(maxStartupEvidenceBytes))...)
	path, err := startupEvidencePath(dataDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	evidence, err := readStartupEvidence(dataDir, sessionID, attemptID)
	if err == nil {
		t.Fatal("oversized startup evidence was accepted")
	}
	if evidence != nil {
		t.Fatalf("oversized startup evidence returned data: %+v", evidence)
	}
	if startupErrorFromEvidence(evidence) != nil {
		t.Fatalf("oversized evidence produced StartupError: %v", startupErrorFromEvidence(evidence))
	}
	for _, leaked := range []string{dataDir, "host.startup.json", `"attemptId"`} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("oversized evidence error leaked %q: %v", leaked, err)
		}
	}
}

func TestStartupEvidenceOnlyMatchesAttempt(t *testing.T) {
	dataDir := t.TempDir()
	const sessionID = "startup-evidence-correlation"
	const oldAttempt = "old-attempt-0123456789abcdef"
	const newAttempt = "new-attempt-0123456789abcdef"

	if err := writeStartupEvidence(dataDir, sessionID, startupEvidence{
		AttemptID: oldAttempt,
		Stage:     StartupStageProviderExit,
	}); err != nil {
		t.Fatal(err)
	}

	evidence, err := readStartupEvidence(dataDir, sessionID, newAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if evidence != nil {
		t.Fatalf("old attempt evidence matched new attempt: %+v", evidence)
	}
	if err := clearStartupEvidence(dataDir, sessionID, newAttempt); err != nil {
		t.Fatal(err)
	}
	evidence, err = readStartupEvidence(dataDir, sessionID, oldAttempt)
	if err != nil || evidence == nil {
		t.Fatalf("new attempt cleared old evidence: %+v, %v", evidence, err)
	}
	if err := clearStartupEvidence(dataDir, sessionID, oldAttempt); err != nil {
		t.Fatal(err)
	}
	evidence, err = readStartupEvidence(dataDir, sessionID, oldAttempt)
	if err != nil || evidence != nil {
		t.Fatalf("old evidence remained after owner clear: %+v, %v", evidence, err)
	}
}

func TestConnectOrStartIgnoresStaleEvidenceFromAnotherAttempt(t *testing.T) {
	dataDir := t.TempDir()
	const sessionID = "startup-evidence-stale"
	const staleAttempt = "stale-attempt-0123456789abcdef"

	if err := writeStartupEvidence(dataDir, sessionID, startupEvidence{
		AttemptID: staleAttempt,
		Stage:     StartupStageProviderExit,
	}); err != nil {
		t.Fatal(err)
	}
	release, err := acquireHostLock(dataDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	_, err = ConnectOrStart(ctx, Config{
		SessionID: sessionID, DataDir: dataDir, Workdir: t.TempDir(),
		Argv: []string{os.Args[0], "-test.run=TestProviderHelper"},
		Env: append(os.Environ(),
			"AO_CHAT_HOST_PROVIDER_HELPER=1",
			"AO_CHAT_HOST_EXIT_IMMEDIATELY=1",
		),
	})
	if err == nil {
		t.Fatal("ConnectOrStart unexpectedly succeeded while another lock owner existed")
	}
	var startupErr *StartupError
	if errors.As(err, &startupErr) {
		t.Fatalf("ConnectOrStart attributed stale evidence: %v", err)
	}
	evidence, readErr := readStartupEvidence(dataDir, sessionID, staleAttempt)
	if readErr != nil || evidence == nil {
		t.Fatalf("stale evidence was removed or corrupted: %+v, %v", evidence, readErr)
	}
}

func TestRunWritesEvidenceBeforeReleasingLockAndLoserCannotReplaceIt(t *testing.T) {
	dataDir := t.TempDir()
	const sessionID = "startup-evidence-ownership"
	const ownerAttempt = "owner-attempt-0123456789abcdef"
	const loserAttempt = "loser-attempt-0123456789abcdef"

	published := make(chan struct{})
	releaseOwner := make(chan struct{})
	ownerDone := make(chan error, 1)
	go func() {
		ownerDone <- Run(context.Background(), Config{
			SessionID:        sessionID,
			DataDir:          dataDir,
			Workdir:          t.TempDir(),
			StartupAttemptID: ownerAttempt,
			Env: append(os.Environ(),
				"AO_CHAT_HOST_PROVIDER_HELPER=1",
				"AO_CHAT_HOST_EXIT_IMMEDIATELY=1",
			),
			Argv:                     []string{os.Args[0], "-test.run=TestProviderHelper"},
			startupEvidencePublished: func() { close(published); <-releaseOwner },
		})
	}()

	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not publish startup evidence")
	}

	evidence, err := readStartupEvidence(dataDir, sessionID, ownerAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if evidence == nil || evidence.AttemptID != ownerAttempt {
		t.Fatalf("owner evidence = %+v, want attempt %q", evidence, ownerAttempt)
	}
	if _, err := acquireHostLock(dataDir, sessionID); !errors.Is(err, ErrHostExists) {
		t.Fatalf("owner released the host lock before evidence publication: %v", err)
	}

	loserErr := Run(context.Background(), Config{
		SessionID:        sessionID,
		DataDir:          dataDir,
		Workdir:          t.TempDir(),
		StartupAttemptID: loserAttempt,
		Argv:             []string{os.Args[0], "-test.run=TestProviderHelper"},
		Env: append(os.Environ(),
			"AO_CHAT_HOST_PROVIDER_HELPER=1",
			"AO_CHAT_HOST_EXIT_IMMEDIATELY=1",
		),
	})
	if loserErr == nil {
		t.Fatal("loser Run unexpectedly acquired the owner lock")
	}
	evidence, err = readStartupEvidence(dataDir, sessionID, ownerAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if evidence == nil || evidence.AttemptID != ownerAttempt {
		t.Fatalf("loser changed owner evidence: %+v", evidence)
	}
	loserEvidence, err := readStartupEvidence(dataDir, sessionID, loserAttempt)
	if err != nil || loserEvidence != nil {
		t.Fatalf("loser published evidence: %+v, %v", loserEvidence, err)
	}

	close(releaseOwner)
	select {
	case err := <-ownerDone:
		if err == nil {
			t.Fatal("owner Run unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner Run did not return after release")
	}
	evidence, err = readStartupEvidence(dataDir, sessionID, ownerAttempt)
	if err != nil || evidence == nil {
		t.Fatalf("owner evidence disappeared after lock release: %+v, %v", evidence, err)
	}
}
