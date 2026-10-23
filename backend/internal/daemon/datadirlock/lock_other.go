//go:build !darwin && !linux && !windows

package datadirlock

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("exclusive data dir locks are unsupported on this platform")

func openLockFile(string) (*os.File, error) { return nil, errUnsupported }
func lockFile(*os.File) error               { return errUnsupported }
