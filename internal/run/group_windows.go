package run

import (
	"os"
	"os/exec"
)

// Builds run only on the Mac; on Windows the client still links this package, so commands run
// as plain processes and the "group" is the process itself.

func newGroup(*exec.Cmd) {}

func interruptGroup(pid int) { killGroup(pid) }

func killGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func groupAlive(int) bool { return false }

func ownedByUs(int) bool { return false }
