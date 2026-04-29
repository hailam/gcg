// Package gcg orchestrates the gcg flow: detect a usable git work tree and
// staging area, ask the LLM for a commit subject for the staged diff,
// stream the answer to stdout, copy the cleaned result to the clipboard.
package gcg

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/atotto/clipboard"

	"github.com/hailam/gcg/internal/bootstrap"
	"github.com/hailam/gcg/internal/diff"
	"github.com/hailam/gcg/internal/git"
	"github.com/hailam/gcg/internal/llm"
	"github.com/hailam/gcg/internal/term"
)

// Run executes the gcg flow. The load callback is only invoked when there
// is actual work to do, so the no-op cases (outside a git tree, nothing
// staged) never read config. Returns nil for those cases after writing a
// one-line note to stdout.
func Run(ctx context.Context, load func() (*bootstrap.App, error)) error {
	if !git.InWorkTree() {
		fmt.Println("not inside a git work tree — nothing to do")
		return nil
	}

	hasGitStagedChanges, err := git.HasStagedChanges()
	if err != nil {
		return fmt.Errorf("checking staged changes: %w", err)
	}

	if !hasGitStagedChanges {
		fmt.Println("nothing staged — run `git add` first")
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

	raw, err := llm.Generate(ctx, app.Cfg.LLM.Host, app.Cfg.LLM.Model, prompt, os.Stdout)
	if err != nil {
		return fmt.Errorf("generating commit message: %w", err)
	}

	cleaned := llm.PostProcess(raw)
	slog.Debug("post-processed subject", "raw", raw, "cleaned", cleaned)
	if cleaned == "" {
		return fmt.Errorf("the model returned no usable subject")
	}

	if err := clipboard.WriteAll(cleaned); err != nil {
		return fmt.Errorf("clipboard: %w", err)
	}
	// Two-row final: the subject is the deliverable (bright, eye-
	// catching); the clipboard confirmation is the side-effect (dim).
	fmt.Fprintln(os.Stdout, term.BoldCyan(os.Stdout, cleaned))
	fmt.Fprintln(os.Stdout, term.DimGreen(os.Stdout, "✓ copied to clipboard"))
	return nil
}
