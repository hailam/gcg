package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileRejectsEscapes(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr string
	}{
		{"absolute path", "/etc/passwd", "must be repository-relative"},
		{"parent traversal", "../../../etc/passwd", "escapes repository root"},
		{"single dotdot", "..", "escapes repository root"},
		{"hidden traversal", "internal/../../etc/passwd", "escapes repository root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Execute(context.Background(), "read_file", map[string]any{"path": tc.path})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func TestReadFileSucceedsOnRepoFile(t *testing.T) {
	out, err := Execute(context.Background(), "read_file", map[string]any{"path": "go.mod"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "module github.com/hailam/play-commit") {
		t.Errorf("expected module declaration, got: %s", out)
	}
}

func TestListDirSucceedsOnRepoRoot(t *testing.T) {
	out, err := Execute(context.Background(), "list_dir", map[string]any{"path": "."})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"cmd/", "internal/", "config/", "go.mod"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in listing, got: %s", want, out)
		}
	}
}

func TestListDirRejectsEscapes(t *testing.T) {
	_, err := Execute(context.Background(), "list_dir", map[string]any{"path": "../.."})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "escapes repository root") {
		t.Errorf("expected escapes-root error, got: %v", err)
	}
}

func TestReadFileNavigationHeader(t *testing.T) {
	out, err := Execute(context.Background(), "read_file", map[string]any{
		"path":       "go.mod",
		"start_line": float64(1),
		"line_count": float64(2),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(out, "[lines 1-2 of ") {
		t.Errorf("expected navigation header, got: %s", out)
	}
	if !strings.Contains(out, "module github.com/hailam/play-commit") {
		t.Errorf("expected module declaration in body, got: %s", out)
	}
}

func TestReadFilePastEnd(t *testing.T) {
	out, err := Execute(context.Background(), "read_file", map[string]any{
		"path":       "go.mod",
		"start_line": float64(99999),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "is past the end") {
		t.Errorf("expected past-end message, got: %s", out)
	}
}

func TestReadFileRejectsGitInternal(t *testing.T) {
	_, err := Execute(context.Background(), "read_file", map[string]any{"path": ".git/HEAD"})
	if err == nil {
		t.Fatal("expected error for .git/ access, got nil")
	}
	if !strings.Contains(err.Error(), ".git") {
		t.Errorf("expected .git rejection, got: %v", err)
	}
}

func TestReadFileRejectsGitignored(t *testing.T) {
	// .env is in .gitignore. Create one transiently and verify rejection.
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("setup rev-parse: %v", err)
	}
	root := strings.TrimSpace(string(out))
	tmp := filepath.Join(root, ".env.toolstest")
	if err := os.WriteFile(tmp, []byte("SECRET=nope\n"), 0o644); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	defer os.Remove(tmp)

	_, err = Execute(context.Background(), "read_file", map[string]any{"path": ".env.toolstest"})
	if err == nil {
		t.Fatal("expected error for gitignored file, got nil")
	}
	if !strings.Contains(err.Error(), "gitignored") {
		t.Errorf("expected gitignored rejection, got: %v", err)
	}
}

func TestListDirHidesGitAndIgnored(t *testing.T) {
	out, err := Execute(context.Background(), "list_dir", map[string]any{"path": "."})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, ".git/") || strings.Contains(out, ".git\n") {
		t.Errorf(".git should not appear in listing, got: %s", out)
	}
	if strings.Contains(out, ".env\n") || strings.HasSuffix(out, ".env") {
		t.Errorf(".env-style ignored entries should be filtered, got: %s", out)
	}
}
