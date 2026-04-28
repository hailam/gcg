# gcg

Generate a Conventional Commits subject for your staged diff via local Ollama. Streams the suggestion to stdout, copies the cleaned subject to your clipboard.

## Install

```sh
bash scripts/install.sh
```

Installs the `gcg` binary via `go install` and (if fish is detected) drops a `gcg-auto` function into `~/.config/fish/functions/`.

Requires a running local Ollama (`ollama serve`) with the model pulled (default `gemma4:e4b`; override with `GCG_MODEL`).

## Use

- `gcg` — generate the subject for your already-staged changes. Paste from clipboard into your commit.
- `gcg-auto` (fish) — `git add -A`, run `gcg`, then `git commit -e -m <subject>` so you can review in `$EDITOR` before saving.
