# Tool calling

gcg gives the LLM read-only access to the current git repository so it can
fill in context the staged diff alone doesn't carry — a function's full
signature, a related type definition, the layout near a changed file. The
mechanism is a deliberate **two-phase chat**: tools and structured output
each get their own pass, because they don't compose well in Ollama's
grammar layer.

## Why two phases

Two reasons, both empirical:

**(1) `format` and `tools` fight each other.** Ollama's `format` field
constrains the output to match a JSON Schema at the **grammar (sampling)
layer**. Tool calls, however, are a separate response field on
`api.ChatResponse` — they don't fit the schema shape. When `format` is
set, the sampler is biased toward emitting tokens that keep the output
valid for the schema, which means it skips the tool-call path. We
confirmed at the raw Ollama API level that `gemma4:e4b` fires tools
correctly when `format` is unset (verified via `curl /api/chat`).

**(2) Phase 1 disables streaming.** Ollama's streaming chat with tools is
unreliable for some models (the gemma family in particular): `tool_calls`
only appear in the terminal chunk, and depending on how the streamed
callback path consumes chunks they can be missed. Non-streaming gives a
single response with `tool_calls` intact. Phase 2's streaming behavior is
a non-issue because we don't display streamed content there anyway (the
content is JSON tokens, which would be visual noise — the spinner is the
feedback channel and the assembled subject prints once at the end).

So:

- **Phase 1 — tool use**: `tools` set, `format` unset, `stream` false.
  Model is free to call tools and the response is guaranteed-complete.
- **Phase 2 — structuring**: `format` set, `tools` unset. Model is
  forced to produce a `{type, scope, breaking, description}` JSON object.

The wiring lives in `internal/llm/llm.go` (`Generate` function).

## Flow

```
                  ┌──────────────────────────────────────────┐
                  │ Phase 1 — tool use                       │
                  │ tools = [read_file, list_dir]            │
                  │ format = (unset)                         │
                  └─────────────────┬────────────────────────┘
                                    │
              ┌─────────────────────┴─────────────────────┐
              ▼                                           ▼
    ┌───────────────────┐                     ┌─────────────────────┐
    │ ToolCalls present │                     │ Content only        │
    │                   │                     │                     │
    │ execute each →    │                     │ try extractSubject  │
    │ append result as  │                     │  on the content     │
    │ Role:"tool"       │                     │                     │
    │ loop (capped at 5)│                     │ parses ✓ → return   │
    └─────────┬─────────┘                     │ parses ✗ → Phase 2  │
              │                               └──────────┬──────────┘
              └─────────► next iteration                 │
                                                         ▼
                  ┌──────────────────────────────────────────┐
                  │ Phase 2 — structuring                    │
                  │ tools = (unset)                          │
                  │ format = subjectSchema                   │
                  │                                          │
                  │ extra user msg: "Now output ONLY the     │
                  │ JSON object as instructed."              │
                  │                                          │
                  │ → grammar-enforced JSON                  │
                  │ → extractSubject → return subject        │
                  └──────────────────────────────────────────┘
```

The Phase 1 cap (5 iterations) bounds runtime against a misbehaving
model. If the cap is hit, gcg falls through to Phase 2 anyway — the
conversation history is intact, the structuring pass still produces a
final answer.

## The schema

`subjectSchema` (defined in `internal/llm/llm.go`):

```json
{
  "type": "object",
  "properties": {
    "type": {
      "type": "string",
      "enum": ["feat","fix","docs","style","refactor","perf","test","build","ci","chore","revert"]
    },
    "scope":       {"type": "string"},
    "breaking":    {"type": "boolean"},
    "description": {"type": "string"}
  },
  "required": ["type","scope","breaking","description"]
}
```

The `enum` constraint on `type` is the structural win: the model **cannot**
emit `"feature"` or `"Feat"` — the grammar rejects it. gcg then assembles
the canonical subject as `<type>[(<scope>)][!]: <description>` itself, so
formatting drift (capitalization, punctuation, spacing) is impossible.

## Available tools

Tools are defined in `internal/tools/`. Each tool is described with an
`*mcp.Tool` from the official MCP Go SDK so the schema is canonical, but
invocation is in-process — there is no JSON-RPC transport.

### `read_file(path, [start_line], [line_count])`

Reads the contents of a file in the repository.

- `path` (required) — repository-relative.
- `start_line` (optional, 1-based) — where to start reading. Defaults to 1.
- `line_count` (optional) — how many lines. Defaults to as many as fit
  in 64 KB.

Files larger than 1 MB are refused outright. For files between 64 KB and
1 MB, the model navigates by passing `start_line` and `line_count`; the
response includes a header like `[lines 1-200 of 5000; file size 312 KB.
To continue, call read_file with start_line=201.]`.

### `list_dir(path)`

Lists the entries of a directory.

- `path` (required) — repository-relative; use `"."` for the repo root.

Up to 200 entries are returned. Subdirectories are suffixed with `/`.

## Safety

The model has read-only access **only** to files that are part of the
repository AND not matched by `.gitignore`. The guards are enforced in
code, not just prompted:

- Path must be repository-relative (no absolute paths).
- Path must not escape the repo root via `..` — checked on the cleaned
  path *before* calling `EvalSymlinks` (otherwise `EvalSymlinks` would
  fail with a misleading "no such file" error for non-existent paths
  outside the repo).
- Path must not escape via symlinks — re-checked after `EvalSymlinks`.
- Path must not be inside `.git/`.
- Path must not match `.gitignore` (`git check-ignore -q -- <rel>`).

Listings additionally filter out `.git/` and any entries matched by
`.gitignore` (single batched `git check-ignore --stdin` call).

These checks run regardless of the system prompt — model misbehavior
cannot read sensitive files.

The system prompt also tells the model the constraint exists, so it
doesn't waste tool calls trying to bypass it. Both layers are present.

## Why a model might (correctly) skip tools

**Tool use is a model decision, not something gcg forces.** Most diffs
are self-contained. A well-formed `git diff --cached` already shows:

- Hunk markers (`@@ -100,5 +100,8 @@`) with line numbers.
- Both old and new content per-line.
- File paths in the `diff --git a/x b/x` headers.
- gcg's `Files changed:` summary header listing every staged path with a
  classification (regular / lockfile / generated).

For commit-subject generation, that's almost always enough. Tools are a
fallback for edge cases — e.g., the model wants to see a function's
caller, the surrounding type definition, or a sibling file referenced by
the change. **Don't be surprised when most invocations don't fire any
tool calls — that's the model correctly judging the diff is sufficient.**
The infrastructure is wired and ready; the model decides when the marginal
benefit of an extra read outweighs the latency.

## When tools genuinely help

Concrete cases observed:

- **Type-of-change ambiguity**: a 5-line diff modifies an exported
  function's body. Is it a behavior fix, a perf change, or a refactor?
  Reading the function's surrounding code and tests can clarify.
- **Scope inference for unfamiliar layouts**: the diff touches
  `app/Domains/Operations/Actions/AbcCadenceCycleCountService.php`. The
  model can `list_dir` the parent to see whether the scope should be
  `operations` (siblings are mostly operations files) or something
  narrower.
- **Breaking-change detection**: an exported symbol is removed. The
  model can read callers to confirm whether it's truly part of the
  public API or only internal.

## Configuration

Tools are always enabled. The `Format`/`Tools` toggle between phases is
internal — there's no flag to skip Phase 2 (you'd lose the schema
guarantee) or to skip Phase 1 (you'd lose tool use).

If you ever need to disable tools entirely (debugging, latency
sensitivity), the cleanest entry point is removing the tool registrations
in `internal/tools/readfile.go` and `internal/tools/listdir.go` (or
making `tools.All()` return an empty slice). gcg degrades gracefully:
Phase 1 runs without tools, Phase 2 still produces the structured
answer.
