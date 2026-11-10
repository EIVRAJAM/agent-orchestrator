//go:build windows

package pushobservation

import "os/exec"

func setHookProcessGroup(*exec.Cmd) {}

func hookProcessGroupAlive(int) bool { return false }
