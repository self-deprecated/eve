//go:build darwin

package source

import "golang.org/x/sys/unix"

// cloneTree asks APFS for a copy-on-write clone of the whole tree in one call. CLONE_NOFOLLOW
// clones a symlink at src itself rather than its target; symlinks inside the tree are always
// cloned as links. It fails (ENOTSUP, EXDEV) off APFS or across volumes, and Clone then copies.
func cloneTree(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
