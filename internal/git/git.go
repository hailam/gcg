// Package git is a thin wrapper around the local `git` binary for the
// reads gcg needs: work-tree detection, staged-change detection, and
// staged-diff capture.
package git

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// InWorkTree reports whether the current directory is inside a git work tree.
func InWorkTree() bool {
	out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// HasStagedChanges reports whether the index differs from HEAD.
//
// `git diff --cached --quiet` exits 0 when there are no staged changes and 1
// when there are; any other exit code is a real error.
func HasStagedChanges() (bool, error) {
	err := exec.Command("git", "diff", "--cached", "--quiet").Run()
	if err == nil {
		return false, nil
	}
	var ex *exec.ExitError
	if errors.As(err, &ex) && ex.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("git diff --cached --quiet: %w", err)
}

// StagedDiff returns the output of `git diff --cached`.
func StagedDiff() (string, error) {
	out, err := exec.Command("git", "diff", "--cached").Output()
	if err != nil {
		return "", fmt.Errorf("git diff --cached: %w", err)
	}
	return string(out), nil
}
