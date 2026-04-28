// Package tools defines the in-process MCP-shaped tools the gcg LLM can
// invoke when the staged diff alone doesn't carry enough context.
//
// Each tool is described with an *mcp.Tool from the official MCP Go SDK so
// the schema is canonical, but invocation is in-process — there is no
// JSON-RPC transport. The bridge in internal/llm converts these defs to
// Ollama's api.Tool format and dispatches ToolCalls back through Execute.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Handler is the in-process implementation of a tool.
type Handler func(ctx context.Context, args map[string]any) (string, error)

// Tool pairs an MCP definition with its in-process handler.
type Tool struct {
	Def     *mcp.Tool
	Handler Handler
}

var registry = map[string]*Tool{}

func register(t *Tool) {
	registry[t.Def.Name] = t
}

// All returns every registered tool. Order is not specified.
func All() []*Tool {
	out := make([]*Tool, 0, len(registry))
	for _, t := range registry {
		out = append(out, t)
	}
	return out
}

// Execute looks up a tool by name and runs its handler with the given args.
func Execute(ctx context.Context, name string, args map[string]any) (string, error) {
	t, ok := registry[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return t.Handler(ctx, args)
}

// resolveInsideRepo turns a repository-relative path into an absolute path
// after verifying it doesn't escape the repository root via .. or symlinks
// and isn't inside .git/. Returns the resolved path and the canonical
// repo root (both with symlinks resolved) so callers can run further
// git checks without re-shelling rev-parse.
func resolveInsideRepo(path string) (real, rootReal string, err error) {
	if filepath.IsAbs(path) {
		return "", "", fmt.Errorf("path must be repository-relative, got %q", path)
	}

	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", "", fmt.Errorf("not in a git repo: %w", err)
	}
	root := strings.TrimSpace(string(out))

	rootReal, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve repo root: %w", err)
	}

	// filepath.Join normalizes the path, collapsing .. components. If the
	// result lands outside rootReal, the user gave a traversal attack —
	// catch this BEFORE EvalSymlinks (which would error with a misleading
	// "no such file" message for non-existent paths outside the repo).
	abs := filepath.Join(rootReal, path)
	if escapesRoot(rootReal, abs) {
		return "", "", fmt.Errorf("path %q escapes repository root", path)
	}
	if isGitInternal(rootReal, abs) {
		return "", "", fmt.Errorf("path %q is inside .git/ — not accessible", path)
	}

	// Resolve symlinks. A symlink inside the repo could still point
	// outside (or into .git), so re-check after resolution.
	real, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	if escapesRoot(rootReal, real) {
		return "", "", fmt.Errorf("path %q escapes repository root via symlink", path)
	}
	if isGitInternal(rootReal, real) {
		return "", "", fmt.Errorf("path %q resolves into .git/ — not accessible", path)
	}
	return real, rootReal, nil
}

func escapesRoot(rootReal, target string) bool {
	rel, err := filepath.Rel(rootReal, target)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isGitInternal(rootReal, target string) bool {
	rel, err := filepath.Rel(rootReal, target)
	if err != nil {
		return false
	}
	return rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator))
}

// gitIgnored reports whether the given absolute path is matched by the
// repository's .gitignore rules. Uses `git check-ignore`, which exits 0
// when ignored and 1 when not.
func gitIgnored(rootReal, absPath string) (bool, error) {
	rel, err := filepath.Rel(rootReal, absPath)
	if err != nil {
		return false, err
	}
	cmd := exec.Command("git", "-C", rootReal, "check-ignore", "-q", "--", rel)
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	var ex *exec.ExitError
	if errors.As(err, &ex) && ex.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git check-ignore: %w", err)
}

// filterGitIgnored returns entries minus those matched by .gitignore. The
// entries are repo-relative paths under dirRel (e.g. "internal/git.go" for
// listing the "internal" dir entry "git.go", with dirRel="internal").
// Uses `git check-ignore --stdin` so cost is one git invocation per call.
func filterGitIgnored(rootReal, dirRel string, entries []string) ([]string, error) {
	if len(entries) == 0 {
		return entries, nil
	}
	cmd := exec.Command("git", "-C", rootReal, "check-ignore", "--stdin", "--")
	var input strings.Builder
	for _, e := range entries {
		full := e
		if dirRel != "" && dirRel != "." {
			full = filepath.Join(dirRel, e)
		}
		input.WriteString(full)
		input.WriteByte('\n')
	}
	cmd.Stdin = strings.NewReader(input.String())
	out, err := cmd.Output()
	if err != nil {
		var ex *exec.ExitError
		if errors.As(err, &ex) && ex.ExitCode() == 1 {
			return entries, nil // no entries ignored
		}
		return nil, fmt.Errorf("git check-ignore: %w", err)
	}
	ignored := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Output mirrors the input form (rel-to-root); store the basename
		// so we can match against entries that are basenames.
		ignored[filepath.Base(line)] = true
	}
	kept := make([]string, 0, len(entries))
	for _, e := range entries {
		if !ignored[e] {
			kept = append(kept, e)
		}
	}
	return kept, nil
}
