//go:build unix

package run

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func newGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func interruptGroup(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGINT) }

func killGroup(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func ownedByUs(pgid int) bool {
	var st syscall.Stat_t
	if err := syscall.Stat(fmt.Sprintf("/proc/%d", pgid), &st); err == nil {
		return int(st.Uid) == os.Getuid()
	}
	// macOS has no /proc: kill(-pgid, 0) only succeeds for groups we may signal, i.e. ours.
	return syscall.Kill(-pgid, 0) == nil
}
