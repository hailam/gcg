package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxDirEntries = 200

func init() {
	register(&Tool{
		Def: &mcp.Tool{
			Name: "list_dir",
			Description: "List the entries (files and subdirectories) of a directory " +
				"in the current git repository. Use this when you need to understand " +
				"the layout near a changed file — for example, to identify the package " +
				"or feature folder for the scope. The path must be repository-relative; " +
				"use \".\" for the repo root.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Repository-relative path to a directory. Use \".\" for the repo root."
					}
				},
				"required": ["path"]
			}`),
		},
		Handler: listDir,
	})
}

func listDir(_ context.Context, args map[string]any) (string, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("list_dir: missing path argument")
	}

	real, rootReal, err := resolveInsideRepo(path)
	if err != nil {
		return "", fmt.Errorf("list_dir: %w", err)
	}

	ignored, err := gitIgnored(rootReal, real)
	if err != nil {
		return "", fmt.Errorf("list_dir: %w", err)
	}
	if ignored {
		return "", fmt.Errorf("list_dir: %q is gitignored — not accessible", path)
	}

	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("list_dir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("list_dir: %q is not a directory; use read_file", path)
	}

	entries, err := os.ReadDir(real)
	if err != nil {
		return "", fmt.Errorf("list_dir: %w", err)
	}

	// Strip .git outright; filter the rest through .gitignore.
	names := make([]string, 0, len(entries))
	isDir := map[string]bool{}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		names = append(names, e.Name())
		isDir[e.Name()] = e.IsDir()
	}

	relDir, _ := filepath.Rel(rootReal, real)
	kept, err := filterGitIgnored(rootReal, relDir, names)
	if err != nil {
		return "", fmt.Errorf("list_dir: %w", err)
	}

	var lines []string
	for i, name := range kept {
		if i >= maxDirEntries {
			lines = append(lines, fmt.Sprintf("...[truncated, %d more entries]", len(kept)-maxDirEntries))
			break
		}
		if isDir[name] {
			lines = append(lines, name+"/")
		} else {
			lines = append(lines, name)
		}
	}
	return strings.Join(lines, "\n"), nil
}
