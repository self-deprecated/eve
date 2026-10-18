//go:build unix

package cli

import "syscall"

type syscallStatfs struct{}

// free returns the bytes available to unprivileged users on dir's filesystem.
func (syscallStatfs) free(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
