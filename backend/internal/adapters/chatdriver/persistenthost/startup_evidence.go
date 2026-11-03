package persistenthost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const startupEvidenceVersion = 1

const startupEvidenceFileName = "host.startup.json"

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

func readStartupEvidence(dataDir, sessionID string) (*startupEvidence, error) {
	path, err := startupEvidencePath(dataDir, sessionID)
	if err != nil {
		return nil, err
	}
	payload, err := os.ReadFile(path) //nolint:gosec // AO-owned path derived from a validated session id.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var evidence startupEvidence
	if err := json.Unmarshal(payload, &evidence); err != nil {
		return nil, errors.New("invalid chat host startup evidence")
	}
	if evidence.Version != startupEvidenceVersion || !evidence.Stage.valid() {
		return nil, errors.New("invalid chat host startup evidence")
	}
	return &evidence, nil
}

func clearStartupEvidence(dataDir, sessionID string) error {
	path, err := startupEvidencePath(dataDir, sessionID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
