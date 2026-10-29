//go:build !windows

package persistenthost

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func spawnDetached(ctx context.Context, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	stdout, stderr, closeDiagnostics, diagnosticsErr := openHostStreams()
	if diagnosticsErr != nil {
		// Diagnostics must never prevent the host from starting. The original
		// startup error, if any, remains the one surfaced by the host.
		fmt.Fprintf(os.Stderr, "chat host diagnostics unavailable: %v\n", diagnosticsErr)
		stdout, stderr = nil, nil
		closeDiagnostics = func() {}
	}
	defer closeDiagnostics()
	cmd := exec.Command(exe, hostArgs(cfg)...) //nolint:forbidigo // detached chat host has specialized lifetime and session ownership
	cmd.Dir = cfg.Workdir
	cmd.Env = cfg.Env
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn detached chat host: %w", err)
	}
	// Setsid detaches the host from our session, but it remains our child until
	// Wait reaps it. Release alone leaves a zombie after each hibernation.
	go func() { _ = cmd.Wait() }()
	return nil
}
