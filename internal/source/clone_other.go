//go:build !darwin

package source

import "errors"

// cloneTree has no copy-on-write implementation off darwin; Clone falls back to copying.
func cloneTree(src, dst string) error {
	return errors.New("copy-on-write clone is only available on darwin")
}
