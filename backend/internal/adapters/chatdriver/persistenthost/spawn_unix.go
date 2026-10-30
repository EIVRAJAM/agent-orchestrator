//go:build !windows

package persistenthost

import (
	"context"
	"fmt"
	"io"
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
	var (
		stdout io.Writer
		stderr io.Writer
		extra  []*os.File
		env    = cfg.Env
	)
	if cfg.Diagnostics != nil {
		if diagnosticsErr := validateHostDiagnostics(cfg.Diagnostics); diagnosticsErr != nil {
			// Diagnostics must never prevent the host from starting or replace the
			// host's real startup error. Fall back to normal stdio.
			fmt.Fprintf(os.Stderr, "chat host diagnostics unavailable: %v\n", diagnosticsErr)
		} else {
			stdout = newLimitedWriter(cfg.Diagnostics.Stdout)
			stderr = newLimitedWriter(cfg.Diagnostics.Stderr)
			if cfg.Diagnostics.Events != nil {
				extra = []*os.File{cfg.Diagnostics.Events}
				if env == nil {
					env = os.Environ()
				}
				env = append(append([]string(nil), env...), hostEventsFDEnv+"=3")
			}
		}
	}
	cmd := exec.Command(exe, hostArgs(cfg)...) //nolint:forbidigo // detached chat host has specialized lifetime and session ownership
	cmd.Dir = cfg.Workdir
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.ExtraFiles = extra
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn detached chat host: %w", err)
	}
	// Setsid detaches the host from our session, but it remains our child until
	// Wait reaps it. Release alone leaves a zombie after each hibernation.
	go func() { _ = cmd.Wait() }()
	return nil
}
