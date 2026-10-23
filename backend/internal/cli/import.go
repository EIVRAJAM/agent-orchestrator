package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemon/datadirlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/legacyimport"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type importOptions struct {
	from   string
	dryRun bool
	yes    bool
	json   bool
}

// importAfterLock is a test seam for proving that import holds the data-dir
// lock for the whole operation. Production leaves it nil.
var importAfterLock func()

func newImportCommand(ctx *commandContext) *cobra.Command {
	var opts importOptions
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import projects from a legacy AO install",
		Long: "Import reads the legacy Agent Orchestrator flat-file store " +
			"(~/.agent-orchestrator) read-only and ports its projects and per-project " +
			"settings into the rewrite database. Legacy files are never modified, and " +
			"a re-run skips rows that already exist, so it is safe to run more than once.\n\n" +
			"The daemon must be stopped: it is the sole writer of the database.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.runImport(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.from, "from", "", "Legacy AO root to read (default ~/.agent-orchestrator)")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Parse and report the planned import without writing")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Skip the confirmation prompt (for non-interactive use)")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output the import report as JSON")
	return cmd
}

func (c *commandContext) runImport(cmd *cobra.Command, opts importOptions) (retErr error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Import is a direct SQLite writer. Take the same kernel lock as the daemon
	// before any run-file inspection or store open, and hold it for the whole
	// import (including dry-run, which still opens/migrates the store).
	dataDirLock, err := datadirlock.Acquire(datadirlock.Path(cfg.DataDir))
	if err != nil {
		if errors.Is(err, datadirlock.ErrLocked) {
			return usageError{fmt.Errorf("the AO data directory %s is in use by another process; stop the daemon with `ao stop` before importing", cfg.DataDir)}
		}
		return fmt.Errorf("acquire data directory lock: %w", err)
	}
	defer func() {
		if closeErr := dataDirLock.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("release data directory lock: %w", closeErr)
		}
	}()
	if importAfterLock != nil {
		importAfterLock()
	}

	// running.json is discovery/compatibility only now; the kernel lock is the
	// authority. Malformed JSON is safe to replace under this ownership, but an
	// access/path/I/O error still means the handshake cannot be inspected.
	if live, err := runfile.CheckStale(cfg.RunFilePath); err != nil {
		if !errors.Is(err, runfile.ErrMalformed) {
			return fmt.Errorf("inspect run-file: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: run-file is malformed; continuing under data-directory lock\n")
	} else if live != nil {
		return usageError{fmt.Errorf("the AO daemon is running (pid %d); stop it first with `ao stop` before importing", live.PID)}
	}

	root := opts.from
	if root == "" {
		root = legacyimport.DefaultLegacyRootDir()
	}
	// Surface a parse error instead of masking it as "no data" (issue #2186):
	// a broken legacy store is distinct from an absent or empty one. Return the
	// error so cmd/ao/main.go renders it once; printing here too would duplicate
	// it on stderr.
	if parseErr := legacyimport.LegacyConfigError(cmd.Context(), root); parseErr != nil {
		return fmt.Errorf("legacy config at %s: %w", root, parseErr)
	}
	if !legacyimport.HasLegacyData(root) {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "No legacy AO projects found at %s. Nothing to import.\n", root)
		return err
	}

	if !opts.dryRun && !opts.yes {
		ok, err := confirm(c.deps.In, cmd.OutOrStdout(),
			fmt.Sprintf("Import projects from %s?", root), true)
		if err != nil {
			return err
		}
		if !ok {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "Import cancelled.")
			return err
		}
	}

	rep, err := c.executeImport(cmd.Context(), cfg, legacyimport.Options{
		Root:   root,
		DryRun: opts.dryRun,
	})
	if err != nil {
		return err
	}

	if opts.json {
		return writeJSON(cmd.OutOrStdout(), rep)
	}
	return writeImportSummary(cmd.OutOrStdout(), rep)
}

// executeImport opens the rewrite store, runs the import, and closes the store.
// It is the one CLI path that opens the database directly: the import is a
// one-time bootstrap that must run with the daemon stopped. The caller holds
// the same daemon data-directory lock for the entire operation, so it cannot
// race a daemon or another import; it cannot go through the loopback API.
func (c *commandContext) executeImport(ctx context.Context, cfg config.Config, opts legacyimport.Options) (legacyimport.Report, error) {
	store, err := sqlite.Open(cfg.DataDir)
	if err != nil {
		return legacyimport.Report{}, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()
	return legacyimport.Run(ctx, store, opts)
}

func writeImportSummary(w io.Writer, rep legacyimport.Report) error {
	var b strings.Builder
	if rep.DryRun {
		b.WriteString("Dry run -- no changes written.\n")
	}
	fmt.Fprintf(&b, "Projects:  %d imported, %d already present\n", rep.ProjectsImported, rep.ProjectsSkipped)
	if len(rep.Notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, n := range rep.Notes {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// confirm prompts for a yes/no answer. When stdin is not an interactive
// terminal it returns the default without prompting, so headless invocations
// behave deterministically.
func confirm(in io.Reader, out io.Writer, prompt string, defaultYes bool) (bool, error) {
	suffix := " [Y/n] "
	if !defaultYes {
		suffix = " [y/N] "
	}
	if !stdinIsInteractive(in) {
		return defaultYes, nil
	}
	if _, err := io.WriteString(out, prompt+suffix); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		// EOF with no input: fall back to the default.
		return defaultYes, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return defaultYes, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, nil
	}
}

// stdinIsInteractive reports whether in is an interactive terminal. It only
// treats the real os.Stdin as potentially interactive; a piped reader or test
// buffer is non-interactive. It is a var so tests can exercise prompt paths
// without a real terminal, mirroring start.go's appScanLocations.
var stdinIsInteractive = func(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// A terminal is a character device, but so is /dev/null, which is what
	// `docker run` without -i hands a process. Treating that as a terminal makes
	// a prompt "answer itself" with the default at EOF, so exclude it. Checking
	// for a real tty needs an ioctl (golang.org/x/term); this covers the only
	// non-tty character device that shows up as stdin in practice.
	if devNull, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, devNull) {
		return false
	}
	return true
}
