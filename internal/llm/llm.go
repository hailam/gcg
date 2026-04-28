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
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ollama/ollama/api"

	"github.com/hailam/gcg/internal/term"
	"github.com/hailam/gcg/internal/tools"
)

//go:embed conventional-commits.md
var conventionalCommitsSpec string

const systemRules = `You generate git commit subjects from staged diffs.

You have access to tools that can read files and list directories from the
current git repository. Prefer using them whenever the diff alone leaves
you uncertain about ANY of these:
  * What an exported symbol referenced by the diff actually does (its full
    signature, callers, or whether it is part of a public API).
  * The right scope when a path is in an unfamiliar layout — list_dir the
    parent to see what siblings exist.
  * Whether a removed/renamed identifier is genuinely public-facing
    (read callers, route files, or migration history).
An extra tool call is cheaper than a wrong commit subject — when in doubt,
read.

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
summary, a list of changes, multiple lines, or any prose outside the
JSON. Do not wrap the JSON in markdown code fences (no `+"```"+`json or `+"```"+`).
Output ONLY the bare JSON object, starting with `+"`{`"+` and ending with `+"`}`"+`.

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
  CRITICAL: the description is JUST the summary. Do NOT include the
  type or scope prefix in it. gcg prepends "<type>[(<scope>)]: "
  itself when assembling the final subject, so writing
  "refactor: implement X" or "feat(api): add Y" inside the
  description value will produce a duplicated prefix in the output.
  Correct: description="implement two-phase tool-calling flow".
  Wrong:   description="refactor: implement two-phase tool-calling flow".

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
	desc := strings.Join(strings.Fields(p.Description), " ")
	// Some models leak the CC type prefix back into the description
	// (e.g. description="refactor: implement X"), which would produce
	// "refactor(scope): refactor: implement X" once we assemble. Strip
	// any leading <type>(<scope>)?!?: pattern from the description.
	desc = descPrefixRe.ReplaceAllString(desc, "")

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
	sb.WriteString(desc)
	return sb.String()
}

var descPrefixRe = regexp.MustCompile(
	`^(?:feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(?:\([\w./-]+\))?!?:\s*`,
)

var validCCType = map[string]bool{
	"feat": true, "fix": true, "docs": true, "style": true,
	"refactor": true, "perf": true, "test": true, "build": true,
	"ci": true, "chore": true, "revert": true,
}

// maxToolIterations caps the chat-loop length so a misbehaving model can't
// spin in tool calls forever.
const maxToolIterations = 5

// Generate sends userPrompt to the Ollama instance at host using model and
// drives a chat loop that handles any tool calls in-process. When stream
// is a TTY, live thinking content is rendered into a rolling viewport
// with an embedded spinner; tool invocations are echoed inline. The
// final subject string is returned — Generate does NOT print it, so the
// caller owns final-output formatting.
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

	useUI := stream != nil && term.IsTerminal(stream)
	think := api.ThinkValue{Value: true}

	slog.Debug("llm.Generate start",
		"model", model, "host", host,
		"prompt_bytes", len(userPrompt), "tools", len(apiTools), "ui", useUI)

	// Phase 1 — tool use. Format is intentionally NOT set here because in
	// Ollama's grammar layer it biases the sampler against tool calls
	// (the schema doesn't include tool-call shape, so the model heads
	// straight for the schema-matching answer). Letting the model be free
	// here gives tools a fair chance to fire.
	//
	// Streaming is enabled on a TTY so we can render thinking chunks
	// live in a viewport. Reasoning models like gemma4:e4b deliver
	// tool_calls cleanly in their terminal chunk; the callback below
	// accumulates ToolCalls across all chunks so a streamed terminal
	// chunk works the same as a single non-streamed response.
	var phase1Content string
	phase1Done := false
	for range maxToolIterations {
		var sb strings.Builder
		var toolCalls []api.ToolCall

		var sp *term.Spinner
		var vp *term.Viewport
		if useUI {
			sp = term.NewSpinnerPool(stream, term.MsgsThinking)
		}

		phase1Stream := useUI
		req := &api.ChatRequest{
			Model:    model,
			Messages: messages,
			Tools:    apiTools,
			Stream:   &phase1Stream,
			Think:    &think,
		}
		chatErr := client.Chat(ctx, req, func(resp api.ChatResponse) error {
			sb.WriteString(resp.Message.Content)
			if len(resp.Message.ToolCalls) > 0 {
				toolCalls = append(toolCalls, resp.Message.ToolCalls...)
			}
			if useUI && resp.Message.Thinking != "" {
				// First thinking chunk: hand off from the standalone
				// spinner to the viewport (which has its own spinner
				// row) so they don't fight over the same writer.
				if sp != nil {
					sp.Stop()
					sp = nil
				}
				if vp == nil {
					vp = term.NewViewport(stream, 4, term.MsgsThinking)
				}
				_, _ = vp.Write([]byte(resp.Message.Thinking))
			}
			return nil
		})
		if sp != nil {
			sp.Stop()
		}
		if vp != nil {
			vp.Stop()
		}
		if chatErr != nil {
			return "", classifyErr(chatErr, model)
		}

		if len(toolCalls) == 0 {
			phase1Content = sb.String()
			messages = append(messages, api.Message{
				Role:    "assistant",
				Content: phase1Content,
			})
			phase1Done = true
			break
		}

		// Tool calls: append the assistant turn, execute tools, feed
		// results back as Role:"tool" messages.
		messages = append(messages, api.Message{
			Role:      "assistant",
			Content:   sb.String(),
			ToolCalls: toolCalls,
		})
		for _, tc := range toolCalls {
			args := tc.Function.Arguments.ToMap()
			if stream != nil {
				line := fmt.Sprintf("[%s%s]", tc.Function.Name, formatArgs(args))
				fmt.Fprintln(stream, term.Cyan(stream, line))
			}
			start := time.Now()
			result, execErr := tools.Execute(ctx, tc.Function.Name, args)
			dur := time.Since(start)
			if execErr != nil {
				slog.Debug("tool error",
					"name", tc.Function.Name, "args", args,
					"error", execErr, "duration_ms", dur.Milliseconds())
				result = "Error: " + execErr.Error()
			} else {
				slog.Debug("tool call",
					"name", tc.Function.Name, "args", args,
					"result_bytes", len(result), "duration_ms", dur.Milliseconds())
			}
			messages = append(messages, api.Message{
				Role:    "tool",
				Content: result,
			})
		}
	}

	// Optimization: if Phase 1 already produced parseable JSON (the
	// system prompt asks for it; well-behaved models comply), use it
	// directly and skip Phase 2. The caller owns presentation of the
	// final subject.
	if phase1Done && phase1Content != "" {
		if subject, err := extractSubject(phase1Content); err == nil {
			slog.Debug("phase 1 short-circuit", "subject", subject)
			return subject, nil
		}
	}
	slog.Debug("phase 1 → phase 2", "phase1_done", phase1Done, "phase1_content_bytes", len(phase1Content))

	// Phase 2 — structuring. Format constraint forces a JSON object
	// matching subjectSchema. Tools are NOT declared here because the
	// flow has already gathered the context it needs in Phase 1.
	messages = append(messages, api.Message{
		Role:    "user",
		Content: "Now output ONLY the final JSON object as instructed: a single object with type, scope, breaking, and description. No prose, no tool calls.",
	})

	var sp *term.Spinner
	var vp *term.Viewport
	if useUI {
		sp = term.NewSpinnerPool(stream, term.MsgsStructuring)
	}
	var sb strings.Builder
	req := &api.ChatRequest{
		Model:    model,
		Messages: messages,
		Stream:   &streamFlag,
		Format:   json.RawMessage(subjectSchema),
		Think:    &think,
	}
	chatErr := client.Chat(ctx, req, func(resp api.ChatResponse) error {
		sb.WriteString(resp.Message.Content)
		if useUI && resp.Message.Thinking != "" {
			if sp != nil {
				sp.Stop()
				sp = nil
			}
			if vp == nil {
				vp = term.NewViewport(stream, 4, term.MsgsStructuring)
			}
			_, _ = vp.Write([]byte(resp.Message.Thinking))
		}
		return nil
	})
	if sp != nil {
		sp.Stop()
	}
	if vp != nil {
		vp.Stop()
	}
	if chatErr != nil {
		return "", classifyErr(chatErr, model)
	}
	slog.Debug("phase 2 done", "response_bytes", sb.Len())

	subject, err := extractSubject(sb.String())
	if err != nil {
		return "", err
	}
	return subject, nil
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
// ccParts and assembles the canonical subject string.
//
// The Format constraint *should* give us a clean JSON object, but in
// practice models leak around it: markdown code fences (```json ... ```),
// preamble text, trailing commentary. We try three parse strategies in
// order — raw, fence-stripped, and brace-bounded — before falling back to
// scanning for a Conventional-Commits-shaped line.
func extractSubject(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("model returned empty response")
	}

	strategies := []string{"raw", "fence-stripped", "object-extracted"}
	for i, candidate := range []string{raw, stripCodeFence(raw), extractJSONObject(raw)} {
		if candidate == "" {
			continue
		}
		var parts ccParts
		if err := json.Unmarshal([]byte(candidate), &parts); err != nil || parts.Description == "" {
			continue
		}
		if !validCCType[parts.Type] {
			return "", fmt.Errorf("model produced invalid type %q (raw: %q)", parts.Type, raw)
		}
		slog.Debug("extractSubject parsed", "strategy", strategies[i])
		return parts.Subject(), nil
	}

	// Last resort: a CC-shaped line anywhere in the output.
	for line := range strings.SplitSeq(raw, "\n") {
		t := strings.TrimSpace(line)
		if ccSubjectRe.MatchString(t) {
			slog.Debug("extractSubject parsed", "strategy", "regex-fallback")
			return t, nil
		}
	}
	return "", fmt.Errorf("model output is not a usable subject (raw: %q)", raw)
}

// stripCodeFence removes a leading ```... line and a trailing ``` so a
// fenced JSON block becomes parseable.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// extractJSONObject returns the substring from the first '{' to the last
// '}' — a permissive way to recover an object embedded in surrounding
// prose. Doesn't validate balance; the json.Unmarshal does that.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	end := strings.LastIndexByte(s, '}')
	if end <= start {
		return ""
	}
	return s[start : end+1]
}

// ccSubjectRe matches a Conventional-Commits-shaped line. The type
// alternation mirrors validCCType / subjectSchema so the fallback can't
// accept prose like "thinking: ..." or "looking: ..." that happens to
// share the lowercase-prefix-colon shape.
var ccSubjectRe = regexp.MustCompile(
	`^(?:feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(?:\([\w./-]+\))?!?:\s+\S`,
)

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
		slog.Debug("classifyErr", "branch", "status_404", "model", model, "original", err.Error())
		return fmt.Errorf("model %q is not available locally — run: ollama pull %s", model, model)
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "could not connect"),
		strings.Contains(msg, "dial tcp"):
		slog.Debug("classifyErr", "branch", "connection", "original", err.Error())
		return fmt.Errorf("ollama is not reachable — start it with: ollama serve")
	case strings.Contains(msg, "model") && strings.Contains(msg, "not found"):
		slog.Debug("classifyErr", "branch", "model_not_found", "model", model, "original", err.Error())
		return fmt.Errorf("model %q is not available locally — run: ollama pull %s", model, model)
	}
	slog.Debug("classifyErr", "branch", "passthrough", "original", err.Error())
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
