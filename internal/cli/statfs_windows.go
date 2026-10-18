package cli

type syscallStatfs struct{}

// free is only used by `doctor --host` on the build Mac; on Windows the space is unknown.
func (syscallStatfs) free(string) (uint64, bool) { return 0, false }
