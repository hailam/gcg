// Package llm wraps the Ollama chat API: send a user prompt with the
// hardcoded gcg system prompt, drive a tool-calling loop until the model
// produces a final answer, and post-process the result. Tools are
// MCP-shaped definitions registered in internal/tools and dispatched
// in-process.
package llm

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/ollama/ollama/api"

	"github.com/hailam/gcg/internal/term"
	"github.com/hailam/gcg/internal/tools"
)

//go:embed conventional-commits.md
var conventionalCommitsSpec string

const systemRules = `You generate git commit subjects from staged diffs.

You have access to tools that can read files and list directories from the
current git repository. Use them only when the staged diff doesn't show
enough surrounding context (e.g. to see a function's full signature, a
type definition, or to identify the right scope from sibling files). Don't
read speculatively — most diffs are self-contained.

The tools only expose files that are part of this repository AND not
matched by .gitignore. Files inside .git/ and ignored paths (build
artifacts, credentials, local dotfiles, vendored dependencies) are
strictly off-limits — the tools will refuse them. Do not try to bypass
this; there is nothing to read in those locations that matters for a
commit subject.

Your final answer MUST be a JSON object of the form:
  {"subject": "<the commit subject>"}

Do not produce a code review, a summary, a list of changes, markdown
formatting, multiple lines, or any prose. The "subject" string itself is
the entire output the user cares about.

The subject value must follow these rules:
- Exactly one line.
- Conventional Commits format: <type>[(<scope>)][!]: <description>
- Pick the type that best fits the change. Never omit the type prefix.
- Include a scope when the change clearly belongs to one area. Identify it
  from the staged file paths: a single Go package → its name (e.g.
  internal/git/* → fix(git):), a top-level module → that name (e.g.
  cmd/gcg/* → fix(gcg):), a feature folder for other stacks (e.g.
  app/Domains/Operations/* → feat(operations):). Omit only when the change
  spans unrelated areas.
- For commits introducing or rolling out a system (feature flags, auth
  layer, etc.), prefer that as the scope: feat(pennant):, feat(auth):.
- Mark breaking changes with ! immediately before the colon.
- Imperative mood ("add", not "added").
- 72 characters or fewer total.
- No trailing period, quotes, or backticks.
- Describe what changed and why it matters, not how it was implemented.

Conventional Commits 1.0.0 reference — consult this when picking type or scope:

`

var systemPrompt = systemRules + conventionalCommitsSpec

// subjectSchema constrains the model's content output to a JSON object
// with a single "subject" string. Ollama enforces this at the grammar
// layer, so the model cannot emit prose, markdown, or multi-line reviews
// even when the staged diff is overwhelming. Tool calls are unaffected
// (they're a separate response path).
const subjectSchema = `{
	"type": "object",
	"properties": {
		"subject": {
			"type": "string",
			"description": "Conventional Commits subject line, one line, 72 chars or fewer"
		}
	},
	"required": ["subject"]
}`

// maxToolIterations caps the chat-loop length so a misbehaving model can't
// spin in tool calls forever.
const maxToolIterations = 5

// Generate sends userPrompt to the Ollama instance at host using model and
// drives a chat loop that handles any tool calls in-process. Each chunk is
// written to stream as it arrives (pass nil to skip streaming output).
// Returns the final-turn assistant content (the actual subject) — earlier
// turns' content is streamed for visibility but not returned.
func Generate(ctx context.Context, host, model, userPrompt string, stream io.Writer) (string, error) {
	u, err := url.Parse(host)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid llm.host %q", host)
	}
	client := api.NewClient(u, http.DefaultClient)

	apiTools, err := buildOllamaTools()
	if err != nil {
		return "", fmt.Errorf("build tools: %w", err)
	}
	streamFlag := stream != nil

	messages := []api.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	useSpinner := stream != nil && term.IsTerminal(stream)

	for range maxToolIterations {
		var sb strings.Builder
		var toolCalls []api.ToolCall

		var sp *term.Spinner
		if useSpinner {
			sp = term.NewSpinner(stream, fmt.Sprintf("consulting %s…", model))
		}

		req := &api.ChatRequest{
			Model:    model,
			Messages: messages,
			Tools:    apiTools,
			Stream:   &streamFlag,
			Format:   json.RawMessage(subjectSchema),
		}

		// We accumulate the model's content silently rather than streaming
		// it to the user. The content is JSON (per the Format constraint)
		// and would just look like noisy `{"subj` token fragments. The
		// spinner provides feedback during the wait; the extracted subject
		// is printed as one clean line after parsing.
		chatErr := client.Chat(ctx, req, func(resp api.ChatResponse) error {
			sb.WriteString(resp.Message.Content)
			if len(resp.Message.ToolCalls) > 0 {
				toolCalls = append(toolCalls, resp.Message.ToolCalls...)
			}
			return nil
		})
		if sp != nil {
			sp.Stop()
		}
		if chatErr != nil {
			return "", classifyErr(chatErr, model)
		}

		if len(toolCalls) == 0 {
			subject, err := extractSubject(sb.String())
			if err != nil {
				return "", err
			}
			if stream != nil {
				fmt.Fprintln(stream, subject)
			}
			return subject, nil
		}

		// Append the assistant turn (with its tool calls) to history.
		messages = append(messages, api.Message{
			Role:      "assistant",
			Content:   sb.String(),
			ToolCalls: toolCalls,
		})

		// Execute each tool call and append its result as a tool message.
		for _, tc := range toolCalls {
			args := tc.Function.Arguments.ToMap()
			if stream != nil {
				line := fmt.Sprintf("[%s%s]", tc.Function.Name, formatArgs(args))
				fmt.Fprintln(stream, term.Cyan(stream, line))
			}
			result, execErr := tools.Execute(ctx, tc.Function.Name, args)
			if execErr != nil {
				result = "Error: " + execErr.Error()
			}
			messages = append(messages, api.Message{
				Role:    "tool",
				Content: result,
			})
		}
	}

	return "", fmt.Errorf("tool-call iteration limit (%d) exceeded", maxToolIterations)
}

// buildOllamaTools converts the registered MCP tool definitions into the
// structured Tool/ToolFunction/ToolPropertiesMap shape that Ollama's API
// requires. The MCP InputSchema is JSON Schema in a json.RawMessage; we
// re-parse it into a small intermediate struct and copy fields across.
func buildOllamaTools() (api.Tools, error) {
	available := tools.All()
	out := make(api.Tools, 0, len(available))
	for _, t := range available {
		var rawSchema []byte
		switch s := t.Def.InputSchema.(type) {
		case json.RawMessage:
			rawSchema = s
		case []byte:
			rawSchema = s
		default:
			b, err := json.Marshal(s)
			if err != nil {
				return nil, fmt.Errorf("tool %q: marshal schema: %w", t.Def.Name, err)
			}
			rawSchema = b
		}

		var schema struct {
			Type       string   `json:"type"`
			Required   []string `json:"required"`
			Properties map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(rawSchema, &schema); err != nil {
			return nil, fmt.Errorf("tool %q: parse schema: %w", t.Def.Name, err)
		}

		props := api.NewToolPropertiesMap()
		for name, p := range schema.Properties {
			props.Set(name, api.ToolProperty{
				Type:        api.PropertyType{p.Type},
				Description: p.Description,
			})
		}

		out = append(out, api.Tool{
			Type: "function",
			Function: api.ToolFunction{
				Name:        t.Def.Name,
				Description: t.Def.Description,
				Parameters: api.ToolFunctionParameters{
					Type:       schema.Type,
					Required:   schema.Required,
					Properties: props,
				},
			},
		})
	}
	return out, nil
}

// extractSubject parses the model's structured-output response and
// returns the subject string. The Format constraint guarantees valid JSON
// matching subjectSchema, so the parse should always succeed; if it
// doesn't (model misbehavior, Ollama oddity), fall back to scanning for
// the first Conventional-Commits-shaped line in the raw output.
func extractSubject(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("model returned empty response")
	}
	var parsed struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed.Subject != "" {
		return parsed.Subject, nil
	}
	// Fallback: a CC-shaped first line is better than nothing.
	for line := range strings.SplitSeq(raw, "\n") {
		t := strings.TrimSpace(line)
		if ccSubjectRe.MatchString(t) {
			return t, nil
		}
	}
	return "", fmt.Errorf("model output is not a usable subject (raw: %q)", raw)
}

var ccSubjectRe = regexp.MustCompile(`^[a-z]+(\([\w./-]+\))?!?:\s+\S`)

func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return "()"
	}
	parts := make([]string, 0, len(args))
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(parts)
	return "(" + strings.Join(parts, ", ") + ")"
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
