function gcg-auto --description "Auto-stage every change, generate a Conventional Commits message, then git commit -e with it pre-filled. Pass --body for a bullet-point body."
    # Outside a git repo: pass through so gcg prints its standard
    # "not in a git work tree" notice.
    if not git rev-parse --is-inside-work-tree >/dev/null 2>&1
        gcg
        return $status
    end

    # Stage everything respecting .gitignore. New files included.
    # If your .gitignore is loose, fix that — don't water this down.
    git add -A 2>/dev/null

    if git diff --cached --quiet >/dev/null 2>&1
        echo (set_color brblack)"(gcg-auto) no changes to commit"(set_color normal)
        return 0
    end

    # --no-clip: gcg's stdout is now the canonical channel for the
    # commit message; we don't need the clipboard round-trip.
    # The streaming UI (spinner, thinking viewport, tool calls, pretty
    # preview) still lands on stderr so the user sees it live.
    # Forward any caller-supplied flags (e.g. --body, --think=high).
    set -l msg (gcg --no-clip $argv)
    set -l rc $status
    if test $rc -ne 0
        return $rc
    end

    if test -z "$msg"
        echo (set_color red)"(gcg-auto) gcg produced no message — can't commit"(set_color normal)
        return 1
    end

    # auto means auto — commit straight through with no editor stop.
    # `-F -` reads the full message (multi-line body and all) from
    # stdin, which `-m` can't safely round-trip through a fish
    # variable. If you want a last-mile review, use `gcg` directly and
    # paste from the clipboard.
    printf '%s\n' $msg | git commit -F -
end
