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
	"unicode"
	"unicode/utf8"

	"github.com/ollama/ollama/api"

	"github.com/hailam/gcg/internal/term"
	"github.com/hailam/gcg/internal/tools"
)

//go:embed conventional-commits.md
var conventionalCommitsSpec string

const systemRules = `You generate git commit subjects from staged diffs.

What the user message contains:
  * "Files changed:" — every staged path with its kind (regular,
    lockfile, generated). Generated bodies are intentionally omitted.
  * "Repository layout near changes:" — the siblings of every parent
    directory of a changed file. This is canonical scope information;
    do NOT call list_dir on a path already enumerated here.
  * Diff bodies — unified diffs with 20 lines of context per hunk, so
    most hunks already include the surrounding function signature,
    imports, and sibling cases. Lockfile bodies are capped and labeled
    low-priority.

You also have read_file and list_dir tools that can reach anywhere in the
repository (subject to the gitignore boundary below). Default to using
them when the answer depends on something not already shown above. Do
not stop at the diff surface — exported symbols routinely have callers
the diff doesn't show, and a wrong scope or a missed breaking-change
flag is worse than an extra tool call.

You MUST call read_file BEFORE producing your final answer when any of
these apply:
  * The diff removes or renames an exported symbol (capitalized
    identifier in Go; "export"/"export default" in JS/TS; public method
    in PHP/Java). Read at least one sibling file in the same directory
    (use the layout section to pick one) to check whether callers
    survive that aren't part of the staged change. This decides
    breaking=true/false.
  * The diff changes the signature of an exported function/method —
    parameters, return type, receiver, or visibility. Read enough of
    the file (use start_line around the hunk) to confirm the full
    declaration and whether it crosses a package/API boundary.
  * The staged paths span an unfamiliar layout that the layout section
    does not clarify (e.g. a deep feature folder you haven't seen) —
    read a sibling file's package/namespace declaration to learn the
    convention before guessing the scope.

Skip extra tool calls only when the diff plus the included context
already answer the type/scope/breaking question unambiguously — for
example, a docs-only change, a comment-only change, a self-contained
internal refactor whose only public surface is already visible in the
20-line context, or a config/ci-only change.

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
- description: imperative ("add", not "added"). Start with a
  lowercase letter ("add user endpoint", not "Add user endpoint").
  Keep it short — the whole assembled subject should fit in 72
  characters. Describe what changed and why it matters, not how.
  Stay grounded in the actual staged paths and diff bodies — do not
  invent feature names, modules, or scopes that are not present in
  the change. No trailing period. CRITICAL: the description is JUST
  the summary. Do NOT include the type or scope prefix in it. gcg
  prepends "<type>[(<scope>)]: " itself when assembling the final
  subject, so writing "refactor: implement X" or "feat(api): add Y"
  inside the description value will produce a duplicated prefix in
  the output. Correct: description="implement two-phase tool-calling
  flow". Wrong: description="refactor: implement two-phase
  tool-calling flow".

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
			"description": "Imperative-mood summary of what changed and why it matters. Starts with a lowercase letter. \"add\", not \"added\". No trailing period."
		}
	},
	"required": ["type","scope","breaking","description"]
}`

// subjectAndBodySchema extends subjectSchema with a body field — an array
// of bullet strings — for --body mode. Bullets are short imperative
// phrases grounded in the diff; the model must not invent unrelated
// content. Two-to-six bullets is the sweet spot.
const subjectAndBodySchema = `{
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
			"description": "Imperative-mood summary of what changed and why it matters. Starts with a lowercase letter. \"add\", not \"added\". No trailing period."
		},
		"body": {
			"type": "array",
			"minItems": 1,
			"maxItems": 6,
			"items": {
				"type": "string",
				"description": "One bullet — a short imperative phrase describing a single concrete change visible in the diff. Starts with a lowercase letter. No leading dash, no trailing period."
			},
			"description": "Two to six bullet points expanding the subject. Each bullet describes ONE concrete change present in the staged diff."
		}
	},
	"required": ["type","scope","breaking","description","body"]
}`

// bodyRules is appended to the system prompt when --body is requested. It
// tells the model the shape of the bullets and that they must be grounded
// in the diff, not invented. Kept short so it doesn't crowd out the main
// rules.
const bodyRules = `

BODY MODE (additional output requirement):
You must also emit a "body" field — an array of 2 to 6 short bullet
strings. Each bullet:
  * Describes ONE concrete change that is visibly present in the staged
    diff. Do not invent features, modules, or behavior that is not in
    the diff.
  * Is imperative, lowercase-first, no leading dash, no trailing period.
    Example: "expose /users route", not "- Exposed /users route.".
  * Is short (target under 72 characters).
Order bullets from most to least important. If the change is genuinely a
one-liner with nothing else to add, still produce a single bullet that
restates the subject in slightly different words rather than padding.`

// ccParts mirrors the JSON schema. The model fills it in; gcg builds the
// final subject string itself, so formatting drift (capitalization,
// punctuation) is impossible. Body is only populated when the request
// asked for it (--body mode); the rest of the time it's nil.
type ccParts struct {
	Type        string   `json:"type"`
	Scope       string   `json:"scope"`
	Breaking    bool     `json:"breaking"`
	Description string   `json:"description"`
	Body        []string `json:"body,omitempty"`
}

func (p ccParts) Subject() string {
	desc := strings.Join(strings.Fields(p.Description), " ")
	// Some models leak the CC type prefix back into the description
	// (e.g. description="refactor: implement X"), which would produce
	// "refactor(scope): refactor: implement X" once we assemble. Strip
	// any leading <type>(<scope>)?!?: pattern from the description.
	desc = descPrefixRe.ReplaceAllString(desc, "")
	desc = lowercaseFirst(desc)

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

// lowercaseFirst returns s with its first rune lowercased. Conventional
// Commits style — and gcg's prompt — wants imperative-mood descriptions
// like "add user endpoint", not "Add user endpoint". Some models still
// title-case the first word; this is the last-mile fix.
func lowercaseFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	if !unicode.IsUpper(r) {
		return s
	}
	return string(unicode.ToLower(r)) + s[size:]
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

// Preflight verifies Ollama is reachable at host and that model is present
// in its local catalog before any prompt building or streaming UI work
// happens. One round-trip via /api/tags covers both failure modes — server
// down (or wrong OLLAMA_HOST) and model not pulled — so the user gets a
// precise remediation message instead of a generic chat-call failure.
func Preflight(ctx context.Context, host, model string) error {
	u, err := url.Parse(host)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid llm.host %q — set OLLAMA_HOST to a URL like http://localhost:11434", host)
	}
	client := api.NewClient(u, http.DefaultClient)

	slog.Debug("preflight start", "host", host, "model", model)
	resp, err := client.List(ctx)
	if err != nil {
		slog.Debug("preflight failed", "host", host, "error", err)
		return fmt.Errorf("ollama is not reachable at %s — start it with `ollama serve`, or set OLLAMA_HOST to point at a running instance (original: %w)", host, err)
	}

	for _, m := range resp.Models {
		if m.Name == model || m.Model == model {
			slog.Debug("preflight ok", "host", host, "model", model, "catalog_size", len(resp.Models))
			return nil
		}
	}
	slog.Debug("preflight model missing", "host", host, "model", model, "catalog_size", len(resp.Models))
	return fmt.Errorf("ollama is reachable at %s but model %q is not available locally — run: ollama pull %s", host, model, model)
}

// Options controls one call to Generate. Zero value gives the historical
// behavior: subject-only output, default thinking, streaming UI when the
// stream is a TTY.
type Options struct {
	// Body asks the model to also emit a bullet-point body. The returned
	// Result.Body is the assembled multi-line body (one bullet per line,
	// prefixed with "- "); empty otherwise.
	Body bool

	// Think overrides the Ollama thinking level. Valid values: "", "true",
	// "false", "high", "medium", "low". Empty defaults to true; --body
	// mode promotes the default to "high" (think more) unless the caller
	// has set Think explicitly.
	Think string
}

// Result is what Generate hands back: the assembled subject line, and an
// optional body. Body is empty unless Options.Body was set.
type Result struct {
	Subject string
	Body    string
}

// Generate sends userPrompt to the Ollama instance at host using model and
// drives a chat loop that handles any tool calls in-process. When stream
// is a TTY, live thinking content is rendered into a rolling viewport
// with an embedded spinner; tool invocations are echoed inline. The
// final subject (and optional body) is returned — Generate does NOT print
// them, so the caller owns final-output formatting.
func Generate(ctx context.Context, host, model, userPrompt string, stream io.Writer, opts Options) (Result, error) {
	u, err := url.Parse(host)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return Result{}, fmt.Errorf("invalid llm.host %q", host)
	}
	client := api.NewClient(u, http.DefaultClient)

	apiTools, err := buildOllamaTools()
	if err != nil {
		return Result{}, fmt.Errorf("build tools: %w", err)
	}
	streamFlag := stream != nil

	sysPrompt := systemPrompt
	if opts.Body {
		sysPrompt = systemPrompt + bodyRules
	}
	messages := []api.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userPrompt},
	}

	useUI := stream != nil && term.IsTerminal(stream)
	think := resolveThink(opts)

	slog.Debug("llm.Generate start",
		"model", model, "host", host,
		"prompt_bytes", len(userPrompt), "tools", len(apiTools), "ui", useUI,
		"body", opts.Body, "think", fmt.Sprintf("%v", think.Value))

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
					vp = term.NewViewport(stream, 4, nil)
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
			return Result{}, classifyErr(chatErr, model)
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
		if parts, fromJSON, err := extractParts(phase1Content, opts.Body); err == nil {
			res := assembleResult(parts, fromJSON)
			slog.Debug("phase 1 short-circuit", "subject", res.Subject)
			return res, nil
		}
	}
	slog.Debug("phase 1 → phase 2", "phase1_done", phase1Done, "phase1_content_bytes", len(phase1Content))

	// Phase 2 — structuring. Format constraint forces a JSON object
	// matching the appropriate schema. Tools are NOT declared here
	// because the flow has already gathered the context it needs in
	// Phase 1.
	finalSchema := subjectSchema
	finalInstruction := "Now output ONLY the final JSON object as instructed: a single object with type, scope, breaking, and description. No prose, no tool calls."
	if opts.Body {
		finalSchema = subjectAndBodySchema
		finalInstruction = "Now output ONLY the final JSON object as instructed: a single object with type, scope, breaking, description, and body (an array of 2-6 bullet strings grounded in the diff). No prose, no tool calls."
	}
	messages = append(messages, api.Message{
		Role:    "user",
		Content: finalInstruction,
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
		Format:   json.RawMessage(finalSchema),
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
		return Result{}, classifyErr(chatErr, model)
	}
	slog.Debug("phase 2 done", "response_bytes", sb.Len())

	parts, fromJSON, err := extractParts(sb.String(), opts.Body)
	if err != nil {
		return Result{}, err
	}
	return assembleResult(parts, fromJSON), nil
}

// resolveThink converts opts.Think into the Ollama ThinkValue. Empty
// defaults to true (default thinking); --body promotes the default to
// "high" so the model gets more budget to ground its bullets in the
// diff. An explicit Think wins over the body-mode promotion.
func resolveThink(opts Options) api.ThinkValue {
	switch opts.Think {
	case "":
		if opts.Body {
			return api.ThinkValue{Value: "high"}
		}
		return api.ThinkValue{Value: true}
	case "true":
		return api.ThinkValue{Value: true}
	case "false":
		return api.ThinkValue{Value: false}
	case "high", "medium", "low":
		return api.ThinkValue{Value: opts.Think}
	default:
		slog.Debug("resolveThink: unknown value, falling back to default", "value", opts.Think)
		if opts.Body {
			return api.ThinkValue{Value: "high"}
		}
		return api.ThinkValue{Value: true}
	}
}

// assembleResult builds the user-facing Result from parsed parts: the
// assembled subject line, plus a "- bullet" body block when bullets were
// produced. fromJSON is true when parts came from a real JSON parse and
// should be re-assembled via ccParts.Subject; false when parts came from
// the regex-fallback path (parts.Description already holds the full
// "<type>(<scope>)!?: desc" line and must be used verbatim).
func assembleResult(p ccParts, fromJSON bool) Result {
	subject := p.Description
	if fromJSON {
		subject = p.Subject()
	}
	r := Result{Subject: subject}
	if len(p.Body) == 0 {
		return r
	}
	var sb strings.Builder
	for i, b := range p.Body {
		b = strings.TrimSpace(b)
		b = strings.TrimPrefix(b, "-")
		b = strings.TrimPrefix(b, "*")
		b = strings.TrimSpace(b)
		b = strings.TrimSuffix(b, ".")
		b = lowercaseFirst(b)
		if b == "" {
			continue
		}
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("- ")
		sb.WriteString(b)
	}
	r.Body = sb.String()
	return r
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

// extractParts parses the model's structured-output response into
// ccParts. wantBody controls whether the absence of the body array is a
// hard failure: in --body mode the caller needs bullets, so missing
// bullets force a retry path; in default mode the body field is
// ignored.
//
// The Format constraint *should* give us a clean JSON object, but in
// practice models leak around it: markdown code fences (```json ... ```),
// preamble text, trailing commentary. We try three parse strategies in
// order — raw, fence-stripped, and brace-bounded — before falling back to
// scanning for a Conventional-Commits-shaped line.
//
// The bool return is true when the result came from JSON parsing (and
// should be re-assembled via ccParts.Subject); false when it came from
// the regex fallback (Description already holds the full assembled line
// and must be used verbatim).
func extractParts(raw string, wantBody bool) (ccParts, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ccParts{}, false, fmt.Errorf("model returned empty response")
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
			return ccParts{}, false, fmt.Errorf("model produced invalid type %q (raw: %q)", parts.Type, raw)
		}
		if wantBody && len(parts.Body) == 0 {
			// JSON parsed but the model skipped the body — let the
			// caller fall through to phase 2 / re-prompt.
			continue
		}
		slog.Debug("extractParts parsed", "strategy", strategies[i], "body_bullets", len(parts.Body))
		return parts, true, nil
	}

	// Last resort: a CC-shaped line anywhere in the output. Body is
	// unrecoverable from this fallback; if the caller wanted bullets,
	// surface that as a usability failure instead of returning a subject
	// without the body the user asked for.
	for line := range strings.SplitSeq(raw, "\n") {
		t := strings.TrimSpace(line)
		if ccSubjectRe.MatchString(t) {
			if wantBody {
				return ccParts{}, false, fmt.Errorf("model returned a subject line but no body (raw: %q)", raw)
			}
			slog.Debug("extractParts parsed", "strategy", "regex-fallback")
			return ccParts{Description: t}, false, nil
		}
	}
	return ccParts{}, false, fmt.Errorf("model output is not a usable subject (raw: %q)", raw)
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
