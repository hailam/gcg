#!/usr/bin/env bash
# scripts/install.sh — install the gcg binary via `go install` and, if fish
# is available, drop the opinionated `gcg` fish function into the user's
# functions dir so it auto-loads in new sessions.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if ! command -v go >/dev/null 2>&1; then
    echo "error: 'go' is not installed — get it from https://go.dev/dl/" >&2
    exit 1
fi

echo "→ installing gcg binary via go install…"
(cd "$REPO_ROOT" && go install ./cmd/gcg)

GO_BIN="$(go env GOBIN)"
if [ -z "$GO_BIN" ]; then
    GO_BIN="$(go env GOPATH)/bin"
fi

if [ ! -x "$GO_BIN/gcg" ]; then
    echo "error: expected 'gcg' at $GO_BIN/gcg but it isn't there" >&2
    exit 1
fi
echo "  installed at $GO_BIN/gcg"

if ! command -v gcg >/dev/null 2>&1; then
    cat <<EOF

warning: $GO_BIN is not on your PATH; add it before using gcg:

  fish:  fish_add_path $GO_BIN
  bash:  echo 'export PATH="\$PATH:$GO_BIN"' >> ~/.bashrc
  zsh:   echo 'export PATH="\$PATH:$GO_BIN"' >> ~/.zshrc

EOF
fi

if command -v fish >/dev/null 2>&1; then
    FISH_FN_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/fish/functions"
    mkdir -p "$FISH_FN_DIR"
    cp "$SCRIPT_DIR/gcg-auto.fish" "$FISH_FN_DIR/gcg-auto.fish"
    echo "→ installed fish function 'gcg-auto' at $FISH_FN_DIR/gcg-auto.fish"
    echo "  auto-loaded on next fish session; bare 'gcg' stays as the binary"
fi

echo "✓ done"
