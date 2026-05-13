// Package gcg orchestrates the gcg flow: detect a usable git work tree and
// staging area, ask the LLM for a commit subject (and optional body) for
// the staged diff, stream the loading/thinking UI to stderr, and emit
// the clean commit message to stdout (so a wrapper can capture it with
// `set msg (gcg ...)`). The cleaned subject is also copied to the
// clipboard unless --no-clip is set.
package gcg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/atotto/clipboard"

	"github.com/hailam/gcg/internal/bootstrap"
	"github.com/hailam/gcg/internal/diff"
	"github.com/hailam/gcg/internal/git"
	"github.com/hailam/gcg/internal/llm"
	"github.com/hailam/gcg/internal/term"
)

// Options configures one invocation of Run. All fields are independent;
// the zero value matches the historical interactive behavior (clipboard
// on, subject only, default thinking).
type Options struct {
	// NoClip disables the system-clipboard write. Useful when a wrapper
	// is going to consume the commit message from stdout directly.
	NoClip bool

	// Body asks the LLM to emit a bullet-point body in addition to the
	// subject line. The body is appended to stdout (subject \n\n body)
	// and, when the clipboard is enabled, the clipboard payload too.
	Body bool

	// Edit hands off to `git commit -e -F -` with the generated message
	// pre-filled, opening $EDITOR for review. Save+close commits; close
	// empty (or with the comment-only template) aborts. Implies NoClip
	// — once you're committing through gcg there's nothing to paste.
	Edit bool

	// Think overrides the Ollama thinking level: "", "true", "false",
	// "high", "medium", "low". Empty defaults to true; --body promotes
	// the default to "high" automatically.
	Think string
}

// Run executes the gcg flow. The load callback is only invoked when there
// is actual work to do, so the no-op cases (outside a git tree, nothing
// staged) never read config. Returns nil for those cases after writing a
// one-line note to stdout.
//
// stderr gets the streaming UI (spinner, thinking viewport, tool calls,
// pretty preview, "copied" confirmation). stdout gets ONLY the final
// commit message — subject, then a blank line and the body if --body was
// set — so a wrapper can capture it with `set msg (gcg ...)`.
func Run(ctx context.Context, load func() (*bootstrap.App, error), opts Options) error {
	if !git.InWorkTree() {
		fmt.Fprintln(os.Stderr, "not inside a git work tree — nothing to do")
		return nil
	}

	hasGitStagedChanges, err := git.HasStagedChanges()
	if err != nil {
		return fmt.Errorf("checking staged changes: %w", err)
	}

	if !hasGitStagedChanges {
		fmt.Fprintln(os.Stderr, "nothing staged — run `git add` first")
		return nil
	}

	app, err := load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Preflight Ollama once we know we have real work to do but before
	// spending time on diff/prompt/UI — Ollama is a hard requirement, so
	// a misconfigured host or unpulled model should surface here with a
	// precise remediation hint, not as a generic mid-stream failure.
	if err := llm.Preflight(ctx, app.Cfg.LLM.Host, app.Cfg.LLM.Model); err != nil {
		return err
	}

	rawDiff, err := git.StagedDiff()
	if err != nil {
		return fmt.Errorf("generating staged diff: %w", err)
	}
	slog.Debug("staged diff", "bytes", len(rawDiff))

	prompt := diff.BuildPrompt(ctx, rawDiff, app.Cfg.Diff.MaxBytes)
	slog.Debug("prompt built", "bytes", len(prompt), "max_bytes", app.Cfg.Diff.MaxBytes)

	res, err := llm.Generate(ctx, app.Cfg.LLM.Host, app.Cfg.LLM.Model, prompt, os.Stderr, llm.Options{
		Body:  opts.Body,
		Think: opts.Think,
	})
	if err != nil {
		return fmt.Errorf("generating commit message: %w", err)
	}

	subject := llm.PostProcess(res.Subject)
	slog.Debug("post-processed subject", "raw", res.Subject, "cleaned", subject)
	if subject == "" {
		return fmt.Errorf("the model returned no usable subject")
	}

	// commitMsg is what a wrapper would feed back into `git commit -F -`:
	// subject, then (when --body is on) a blank line and the bullet body.
	commitMsg := subject
	if res.Body != "" {
		commitMsg = subject + "\n\n" + res.Body
	}

	// --edit takes precedence over --no-clip flow: we're handing off to
	// git, so the clipboard write would just be visual noise. The
	// pretty preview still lands on stderr so the user sees what was
	// generated before $EDITOR takes over.
	clipEnabled := !opts.NoClip && !opts.Edit
	if clipEnabled {
		if err := clipboard.WriteAll(commitMsg); err != nil {
			return fmt.Errorf("clipboard: %w", err)
		}
	}

	// stderr gets the pretty two-row preview (the subject in bright
	// bold-cyan, the body dimmed, the side-effect confirmation in dim
	// green). stdout gets the plain message so a wrapper can capture it
	// cleanly — except under --edit, where git owns stdout while
	// $EDITOR is running.
	fmt.Fprintln(os.Stderr, term.BoldCyan(os.Stderr, subject))
	if res.Body != "" {
		fmt.Fprintln(os.Stderr, term.Dim(os.Stderr, res.Body))
	}
	if clipEnabled {
		fmt.Fprintln(os.Stderr, term.DimGreen(os.Stderr, "✓ copied to clipboard"))
	}

	if opts.Edit {
		return runGitCommitEdit(ctx, commitMsg)
	}

	fmt.Fprintln(os.Stdout, commitMsg)
	return nil
}

// runGitCommitEdit invokes `git commit -e -F -` with msg piped into
// stdin. git reads the message, closes stdin, then opens $EDITOR with the
// message pre-filled — $EDITOR (vim, etc.) operates against /dev/tty
// directly so the closed stdin doesn't matter. Save+close commits;
// closing on an empty/unchanged template aborts (git exit code 1), which
// we surface as a plain non-error so the wrapper doesn't print a
// scary-looking failure for a deliberate user cancel.
func runGitCommitEdit(ctx context.Context, msg string) error {
	cmd := exec.CommandContext(ctx, "git", "commit", "-e", "-F", "-")
	cmd.Stdin = strings.NewReader(msg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var ex *exec.ExitError
	if errors.As(err, &ex) {
		fmt.Fprintln(os.Stderr, term.Dim(os.Stderr, "(gcg) commit aborted"))
		os.Exit(ex.ExitCode())
	}
	return fmt.Errorf("git commit: %w", err)
}
