package persistenthost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const startupEvidenceVersion = 1

const (
	startupEvidenceFileName       = "host.startup.json"
	startupAttemptEnv             = "AO_CHAT_HOST_STARTUP_ATTEMPT"
	maxStartupEvidenceBytes int64 = 4 << 10
)

// StartupStage is a closed, bounded classification of a detached host startup
// failure. It deliberately carries no provider message, argv, environment, or
// prompt data.
type StartupStage string

// StartupStage values identify the bounded child-side startup phase.
const (
	StartupStageConfig        StartupStage = "config"
	StartupStageLock          StartupStage = "host_lock"
	StartupStageToken         StartupStage = "token"
	StartupStageListener      StartupStage = "listener"
	StartupStageProviderPipe  StartupStage = "provider_pipe"
	StartupStageProviderStart StartupStage = "provider_start"
	StartupStageDescriptor    StartupStage = "descriptor"
	StartupStageACPRelay      StartupStage = "acp_relay"
	StartupStageProviderExit  StartupStage = "provider_exit"
	StartupStageAccept        StartupStage = "accept"
)

func (s StartupStage) valid() bool {
	switch s {
	case StartupStageConfig, StartupStageLock, StartupStageToken,
		StartupStageListener, StartupStageProviderPipe, StartupStageProviderStart,
		StartupStageDescriptor, StartupStageACPRelay, StartupStageProviderExit,
		StartupStageAccept:
		return true
	default:
		return false
	}
}

type startupEvidence struct {
	Version          int          `json:"version"`
	AttemptID        string       `json:"attemptId"`
	Stage            StartupStage `json:"stage"`
	ProviderExited   bool         `json:"providerExited,omitempty"`
	ProviderExitCode int          `json:"providerExitCode,omitempty"`
}

// StartupError reports the bounded child-side stage that prevented a detached
// host from becoming usable. Callers keep the existing coarse chat-driver
// classification; this error only adds safe startup diagnosis.
type StartupError struct {
	Stage            StartupStage
	ProviderExited   bool
	ProviderExitCode int
}

func (e *StartupError) Error() string {
	if e.ProviderExited {
		return fmt.Sprintf("chat host startup failed: stage=%s provider_exit_code=%d",
			e.Stage, e.ProviderExitCode)
	}
	return fmt.Sprintf("chat host startup failed: stage=%s", e.Stage)
}

func startupEvidencePath(dataDir, sessionID string) (string, error) {
	dir, err := hostDir(dataDir, sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, startupEvidenceFileName), nil
}

func writeStartupEvidence(dataDir, sessionID string, evidence startupEvidence) error {
	if !evidence.Stage.valid() {
		return errors.New("invalid chat host startup stage")
	}
	if len(evidence.AttemptID) < 16 || len(evidence.AttemptID) > 128 {
		return errors.New("invalid chat host startup attempt id")
	}
	path, err := startupEvidencePath(dataDir, sessionID)
	if err != nil {
		return err
	}
	evidence.Version = startupEvidenceVersion
	payload, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+startupEvidenceFileName+".tmp-"+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}

func readStartupEvidence(dataDir, sessionID, attemptID string) (*startupEvidence, error) {
	if attemptID == "" {
		return nil, nil
	}
	path, err := startupEvidencePath(dataDir, sessionID)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path) //nolint:gosec // AO-owned path derived from a validated session id.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("chat host startup evidence unavailable")
	}
	defer func() { _ = file.Close() }()
	payload, err := io.ReadAll(io.LimitReader(file, maxStartupEvidenceBytes+1))
	if err != nil {
		return nil, errors.New("chat host startup evidence unavailable")
	}
	if int64(len(payload)) > maxStartupEvidenceBytes {
		return nil, errors.New("invalid chat host startup evidence")
	}
	var evidence startupEvidence
	if err := json.Unmarshal(payload, &evidence); err != nil {
		return nil, errors.New("invalid chat host startup evidence")
	}
	if evidence.AttemptID != attemptID {
		return nil, nil
	}
	if evidence.Version != startupEvidenceVersion || !evidence.Stage.valid() {
		return nil, errors.New("invalid chat host startup evidence")
	}
	return &evidence, nil
}

func clearStartupEvidence(dataDir, sessionID, attemptID string) error {
	if attemptID == "" {
		return nil
	}
	evidence, err := readStartupEvidence(dataDir, sessionID, attemptID)
	if err != nil || evidence == nil {
		return err
	}
	path, err := startupEvidencePath(dataDir, sessionID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func withStartupAttemptEnv(env []string, attemptID string) []string {
	if attemptID == "" {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && name == startupAttemptEnv {
			continue
		}
		out = append(out, entry)
	}
	return append(out, startupAttemptEnv+"="+attemptID)
}

func startupErrorFromEvidence(evidence *startupEvidence) error {
	if evidence == nil {
		return nil
	}
	return &StartupError{
		Stage:            evidence.Stage,
		ProviderExited:   evidence.ProviderExited,
		ProviderExitCode: evidence.ProviderExitCode,
	}
}
