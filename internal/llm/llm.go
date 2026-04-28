// Package llm wraps the Ollama chat API: send a user prompt with the
// hardcoded gcg system prompt, stream chunks to a writer, return the full
// assembled string. Also owns post-processing that trims model output to
// the gcg subject contract.
package llm

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ollama/ollama/api"
)

//go:embed conventional-commits.md
var conventionalCommitsSpec string

const systemRules = `You write git commit message subjects from staged diffs.

Rules:
- Output exactly one line.
- Always use Conventional Commits format: <type>[(<scope>)][!]: <description>
- Pick the type that best fits the change. Never omit the type prefix.
- Include a scope when the change clearly belongs to one area. Identify it
  from the staged file paths: a single Go package → its name (e.g.
  internal/git/* → fix(git):), a top-level module → that name (e.g.
  cmd/cli/* → fix(cli):). Omit only when the change spans unrelated areas.
- Mark breaking changes with ! immediately before the colon (e.g. feat(api)!: ...).
- Imperative mood ("add", not "added").
- 72 characters or fewer total (the whole subject).
- No trailing period, quotes, backticks, or preamble.
- Describe what changed and why it matters, not how it was implemented.

Conventional Commits 1.0.0 reference — consult this when picking type or scope:

`

var systemPrompt = systemRules + conventionalCommitsSpec

// Generate sends userPrompt to the Ollama instance at host using model.
// Each chunk is written to stream as it arrives (pass nil to skip
// streaming output). The full assembled response is returned.
func Generate(ctx context.Context, host, model, userPrompt string, stream io.Writer) (string, error) {
	u, err := url.Parse(host)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid llm.host %q", host)
	}
	client := api.NewClient(u, http.DefaultClient)

	streamFlag := true
	req := &api.ChatRequest{
		Model:  model,
		Stream: &streamFlag,
		Messages: []api.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}

	var sb strings.Builder
	chatErr := client.Chat(ctx, req, func(resp api.ChatResponse) error {
		if stream != nil {
			_, _ = io.WriteString(stream, resp.Message.Content)
		}
		sb.WriteString(resp.Message.Content)
		return nil
	})
	if stream != nil {
		_, _ = io.WriteString(stream, "\n")
	}
	if chatErr != nil {
		return "", classifyErr(chatErr, model)
	}
	return sb.String(), nil
}

func classifyErr(err error, model string) error {
	if err == nil {
		return nil
	}
	var statusErr *api.StatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == 404 {
		return fmt.Errorf("model %q is not available locally — run: ollama pull %s", model, model)
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "could not connect"),
		strings.Contains(msg, "dial tcp"):
		return fmt.Errorf("ollama is not reachable — start it with: ollama serve")
	case strings.Contains(msg, "model") && strings.Contains(msg, "not found"):
		return fmt.Errorf("model %q is not available locally — run: ollama pull %s", model, model)
	}
	return err
}

// PostProcess trims raw model output to a single-line subject: drops
// preambles, wrapping fences/quotes, and a trailing period.
func PostProcess(raw string) string {
	line := ""
	for l := range strings.SplitSeq(raw, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "```") {
			continue
		}
		line = t
		break
	}

	preambles := []string{
		"subject:", "commit message:", "commit subject:", "message:",
		"commit:", "conventional commit:", "conventional commit message:",
	}
	for {
		prev := line

		for _, pre := range preambles {
			if strings.HasPrefix(strings.ToLower(line), pre) {
				line = strings.TrimSpace(line[len(pre):])
			}
		}

		if len(line) >= 6 && strings.HasPrefix(line, "```") && strings.HasSuffix(line, "```") {
			line = strings.TrimSpace(line[3 : len(line)-3])
		}

		for _, q := range []string{"`", `"`, `'`} {
			if len(line) >= 2 && strings.HasPrefix(line, q) && strings.HasSuffix(line, q) {
				line = strings.TrimSpace(line[1 : len(line)-1])
			}
		}

		line = strings.TrimSpace(strings.TrimSuffix(line, "."))

		if line == prev {
			break
		}
	}
	return line
}
