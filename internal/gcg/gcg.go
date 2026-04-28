// Package gcg orchestrates the gcg flow: detect a usable git work tree and
// staging area, ask the LLM for a commit subject for the staged diff,
// stream the answer to stdout, copy the cleaned result to the clipboard.
package gcg

import (
	"context"
	"fmt"
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
		fmt.Println("error checking staged changes:", err)
		return err
	}

	if !hasGitStagedChanges {
		fmt.Println("nothing staged — run `git add` first")
		return nil
	}

	app, err := load()
	if err != nil {
		fmt.Println("error loading config:", err)
		return err
	}

	rawDiff, err := git.StagedDiff()
	if err != nil {
		fmt.Println("error generating staged diff:", err)
		return err
	}

	prompt := diff.BuildPrompt(rawDiff, app.Cfg.Diff.MaxBytes)
	raw, err := llm.Generate(ctx, app.Cfg.LLM.Host, app.Cfg.LLM.Model, prompt, os.Stdout)
	if err != nil {
		fmt.Println("error generating commit message:", err)
		return err
	}

	cleaned := llm.PostProcess(raw)
	if cleaned == "" {
		fmt.Println("the model returned no usable subject")
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
