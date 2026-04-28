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

Your final answer MUST be a JSON object with exactly these four fields:
  {
    "type":        "<one of feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert>",
    "scope":       "<single noun, or \"\" when the change spans unrelated areas>",
    "breaking":    <true or false>,
    "description": "<imperative-mood summary>"
  }

gcg assembles the final subject from these parts as
"<type>[(<scope>)][!]: <description>" — do not concatenate yourself, do
not include punctuation in the parts. Do not produce a code review, a
summary, a list of changes, markdown formatting, multiple lines, or any
prose outside the JSON.

Field rules:
- type: pick the one that best fits the primary intent. Multi-feature
  commits typically take the dominant type (feat for new behavior,
  refactor for restructures, chore for misc).
- scope: identify the area from staged paths. A single Go package → its
  name (internal/git/* → "git"); a top-level module → that name
  (cmd/gcg/* → "gcg"); a feature folder for other stacks (e.g.
  app/Domains/Operations/* → "operations"). For commits introducing or
  rolling out a system (feature flags, auth layer), prefer that system
  as the scope: "pennant", "auth", "billing". Use "" only when the
  change genuinely spans unrelated areas.
- breaking: true if and only if this change is backward-incompatible.
  Strong signals to set breaking=true:
    * Removal of an exported function, type, method, constant, or variable.
    * Change to an exported function/method signature (parameters, return type).
    * Removal or rename of a public HTTP route, RPC method, or CLI flag.
    * Removal or rename of a database column referenced by application code.
    * Change to a published config schema or environment-variable contract.
    * Removal of a published feature, plugin hook, or extension point.
  NOT breaking: adding new exported symbols, internal-only refactors,
  changes to tests, docs, build/CI, vendored deps. When in doubt and the
  diff only adds or modifies internal code, set breaking=false.
- description: imperative ("add", not "added"). Keep it short — the
  whole assembled subject should fit in 72 characters. Describe what
  changed and why it matters, not how. No trailing period.

Conventional Commits 1.0.0 reference — consult this when picking type or scope:

`

var systemPrompt = systemRules + conventionalCommitsSpec

// subjectSchema constrains the model's content output to the four parts of
// a Conventional Commits subject. Ollama enforces this at the grammar
// layer, so the model cannot emit prose, markdown, multi-line reviews, or
// invalid types — and gcg owns the punctuation when assembling the final
// string. Tool calls are unaffected (they're a separate response path).
const subjectSchema = `{
	"type": "object",
	"properties": {
		"type": {
			"type": "string",
			"enum": ["feat","fix","docs","style","refactor","perf","test","build","ci","chore","revert"],
			"description": "Conventional Commits type — the one that best fits the primary intent of the change"
		},
		"scope": {
			"type": "string",
			"description": "Single noun for the area touched (package name, module, feature folder). Use empty string when the change spans unrelated areas."
		},
		"breaking": {
			"type": "boolean",
			"description": "True only when this is a backward-incompatible change"
		},
		"description": {
			"type": "string",
			"description": "Imperative-mood summary of what changed and why it matters. \"add\", not \"added\". No trailing period."
		}
	},
	"required": ["type","scope","breaking","description"]
}`

// ccParts mirrors the JSON schema. The model fills it in; gcg builds the
// final subject string itself, so formatting drift (capitalization,
// punctuation) is impossible.
type ccParts struct {
	Type        string `json:"type"`
	Scope       string `json:"scope"`
	Breaking    bool   `json:"breaking"`
	Description string `json:"description"`
}

func (p ccParts) Subject() string {
	var sb strings.Builder
	sb.WriteString(p.Type)
	if p.Scope != "" {
		sb.WriteString("(")
		sb.WriteString(p.Scope)
		sb.WriteString(")")
	}
	if p.Breaking {
		sb.WriteString("!")
	}
	sb.WriteString(": ")
	// strings.Fields collapses any internal whitespace (incl. stray
	// newlines) into single spaces, so a multi-line description from the
	// model gets flattened into a clean one-liner.
	sb.WriteString(strings.Join(strings.Fields(p.Description), " "))
	return sb.String()
}

var validCCType = map[string]bool{
	"feat": true, "fix": true, "docs": true, "style": true,
	"refactor": true, "perf": true, "test": true, "build": true,
	"ci": true, "chore": true, "revert": true,
}

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

// extractSubject parses the model's structured-output response into
// ccParts and assembles the canonical subject string. The Format
// constraint guarantees valid JSON matching subjectSchema, so the parse
// should always succeed; if it doesn't (Ollama oddity, model unusable),
// fall back to scanning for the first Conventional-Commits-shaped line in
// the raw output.
func extractSubject(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("model returned empty response")
	}
	var parts ccParts
	if err := json.Unmarshal([]byte(raw), &parts); err == nil && parts.Description != "" {
		if !validCCType[parts.Type] {
			return "", fmt.Errorf("model produced invalid type %q (raw: %q)", parts.Type, raw)
		}
		return parts.Subject(), nil
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
