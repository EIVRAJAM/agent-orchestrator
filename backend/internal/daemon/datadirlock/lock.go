// Package datadirlock owns the kernel-backed exclusive lock for one AO data
// directory. Running.json and health checks are discovery signals; this lock is
// the authority that only one daemon can own a data directory at a time.
package datadirlock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileName is the lock file inside AO_DATA_DIR. A residual file is expected
// after a crash and is not evidence that a process still owns the lock.
const FileName = "daemon.lock"

// ErrLocked reports that another process currently owns the data directory.
var ErrLocked = errors.New("AO data directory is already owned by another daemon")

// LockedError identifies the data directory whose kernel lock is occupied.
type LockedError struct {
	Path string
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, ErrLocked)
}

func (e *LockedError) Unwrap() error { return ErrLocked }

// Lock owns an open descriptor/handle for the lifetime of a daemon. Closing it
// is the only release operation; the kernel also releases it on process exit.
type Lock struct {
	path string
	file *os.File

	once     sync.Once
	closeErr error
}

// Path returns the lock file path for a data directory.
func Path(dataDir string) string {
	return filepath.Join(dataDir, FileName)
}

// Acquire creates the parent directory as needed and takes a non-blocking
// exclusive lock on path. The returned Lock must remain open for the daemon's
// lifetime. Metadata in the file is diagnostic only; ownership is the kernel
// lock held by file.
func Acquire(path string) (*Lock, error) {
	if path == "" {
		return nil, errors.New("data dir lock: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("data dir lock: create parent %s: %w", filepath.Dir(path), err)
	}

	file, err := openLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("data dir lock: open %s: %w", path, err)
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		if errors.Is(err, ErrLocked) {
			return nil, &LockedError{Path: path}
		}
		return nil, fmt.Errorf("data dir lock: acquire %s: %w", path, err)
	}

	lock := &Lock{path: path, file: file}
	if err := writeMetadata(lock); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func writeMetadata(lock *Lock) error {
	metadata := struct {
		PID        int       `json:"pid"`
		AcquiredAt time.Time `json:"acquiredAt"`
		Version    int       `json:"version"`
	}{
		PID:        os.Getpid(),
		AcquiredAt: time.Now().UTC(),
		Version:    1,
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("data dir lock: encode metadata: %w", err)
	}
	payload = append(payload, '\n')
	if err := lock.file.Truncate(0); err != nil {
		return fmt.Errorf("data dir lock: truncate metadata: %w", err)
	}
	if _, err := lock.file.Seek(0, 0); err != nil {
		return fmt.Errorf("data dir lock: seek metadata: %w", err)
	}
	if _, err := lock.file.Write(payload); err != nil {
		return fmt.Errorf("data dir lock: write metadata: %w", err)
	}
	if err := lock.file.Sync(); err != nil {
		return fmt.Errorf("data dir lock: sync metadata: %w", err)
	}
	return nil
}

// Close releases the kernel lock and closes the descriptor/handle. It is
// idempotent and leaves the residual lock file in place.
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	l.once.Do(func() {
		l.closeErr = l.file.Close()
	})
	return l.closeErr
}

// Path returns the locked file path.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}
