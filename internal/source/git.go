package source

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitTimeout bounds every git invocation; listing a large work tree takes well under this.
const gitTimeout = 2 * time.Minute

// git runs git in dir with a minimal, explicit environment so a user's GIT_* variables cannot
// change what gets packed. It returns stdout; ok is false when git is missing or fails.
func git(dir string, args ...string) (out []byte, ok bool) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(bin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, false
	}
	return stdout.Bytes(), true
}

func gitEnv(bin string) []string {
	env := []string{
		"PATH=" + filepath.Dir(bin) + ":/usr/bin:/bin",
		"LC_ALL=C",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	}
	// HOME lets git read the user's config, which may mark the repository safe.directory.
	if home, err := os.UserHomeDir(); err == nil {
		env = append(env, "HOME="+home)
	}
	return env
}

// inGitWorkTree reports whether dir lies inside a git work tree.
func inGitWorkTree(dir string) bool {
	out, ok := git(dir, "rev-parse", "--is-inside-work-tree")
	return ok && strings.TrimSpace(string(out)) == "true"
}

// gitFiles lists tracked and untracked-but-not-ignored files below dir, relative to dir.
func gitFiles(dir string) ([]string, bool) {
	out, ok := git(dir, "ls-files", "-co", "--exclude-standard", "-z")
	if !ok {
		return nil, false
	}
	var paths []string
	for p := range strings.SplitSeq(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, true
}

// GitInfo returns the provenance of root's work tree: the HEAD commit, the current branch ("" when
// detached) and whether there are uncommitted changes. ok is false outside a git work tree, when
// git is unavailable, or when the repository has no commits yet.
func GitInfo(root string) (commit, branch string, dirty, ok bool) {
	out, ok := git(root, "rev-parse", "--verify", "HEAD")
	if !ok {
		return "", "", false, false
	}
	commit = strings.TrimSpace(string(out))
	if out, ok := git(root, "symbolic-ref", "--quiet", "--short", "HEAD"); ok {
		branch = strings.TrimSpace(string(out))
	}
	if out, ok := git(root, "status", "--porcelain", "--untracked-files=normal"); ok {
		dirty = len(bytes.TrimSpace(out)) > 0
	}
	return commit, branch, dirty, true
}
