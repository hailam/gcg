function gcg-auto --description "Auto-stage every change, generate a Conventional Commits subject, then git commit -e with it pre-filled"
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

    # Generate the subject. The binary prints to stdout and copies the
    # cleaned subject to the system clipboard.
    gcg
    set -l rc $status
    if test $rc -ne 0
        return $rc
    end

    set -l msg (pbpaste)
    if test -z "$msg"
        echo (set_color red)"(gcg-auto) clipboard is empty — can't commit"(set_color normal)
        return 1
    end

    # `-e` opens $EDITOR with the message pre-filled. Save+close to
    # commit; close empty to abort. The last-mile review is yours.
    git commit -e -m "$msg"
end
