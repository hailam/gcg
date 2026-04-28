// Package diff turns a `git diff --cached` blob into the user message sent
// to the LLM: a `Files changed:` header listing every staged path, followed
// by source-code diff bodies, followed by lockfile diff bodies (capped and
// marked low-priority). Generated artifacts are listed but their bodies
// are dropped — they carry no useful signal for a commit subject.
package diff

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// maxLockfileBytes caps each individual lockfile diff body. Lockfile diffs
// can be enormous (large dep graphs), and they're a low-priority signal:
// when both source and lockfile changes are present, the system prompt
// prefers the source changes for picking the commit type.
const maxLockfileBytes = 4 * 1024

// BuildPrompt produces the user message described in the package doc.
// maxBytes <= 0 disables the overall truncation.
func BuildPrompt(rawDiff string, maxBytes int) string {
	sections := splitByFile(rawDiff)

	var listing strings.Builder
	listing.WriteString("Files changed:\n")
	var sourceBodies []string
	var lockfileBodies []string

	for _, s := range sections {
		switch classify(s.path) {
		case kindGenerated:
			fmt.Fprintf(&listing, "- %s (omitted: generated)\n", s.path)
		case kindLockfile:
			body := s.body
			truncated := false
			if len(body) > maxLockfileBytes {
				body = body[:maxLockfileBytes] + "\n...[lockfile body truncated]\n"
				truncated = true
			}
			if truncated {
				fmt.Fprintf(&listing, "- %s (lockfile, body truncated)\n", s.path)
			} else {
				fmt.Fprintf(&listing, "- %s (lockfile)\n", s.path)
			}
			lockfileBodies = append(lockfileBodies, body)
		default:
			fmt.Fprintf(&listing, "- %s\n", s.path)
			sourceBodies = append(sourceBodies, s.body)
		}
	}

	var bodies strings.Builder
	for _, b := range sourceBodies {
		bodies.WriteString(b)
	}
	if len(lockfileBodies) > 0 {
		bodies.WriteString("\n--- Lockfile changes (low-priority signal; prefer source changes for picking the commit type) ---\n")
		for _, b := range lockfileBodies {
			bodies.WriteString(b)
		}
	}

	full := listing.String() + "\n" + bodies.String()
	if maxBytes > 0 && len(full) > maxBytes {
		full = full[:maxBytes] + "\n...[truncated]\n"
	}
	return full
}

type section struct {
	path string
	body string
}

var headerRe = regexp.MustCompile(`^diff --git a/(.+?) b/(.+)$`)

func splitByFile(diff string) []section {
	if diff == "" {
		return nil
	}
	var sections []section
	var cur section
	var buf strings.Builder

	flush := func() {
		if cur.path == "" {
			return
		}
		cur.body = buf.String()
		sections = append(sections, cur)
	}

	for line := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			cur = section{path: extractPath(line)}
			buf.Reset()
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()
	return sections
}

func extractPath(diffHeader string) string {
	if m := headerRe.FindStringSubmatch(diffHeader); len(m) >= 3 {
		return m[2]
	}
	parts := strings.Fields(diffHeader)
	if len(parts) >= 4 {
		return strings.TrimPrefix(parts[3], "b/")
	}
	return ""
}

type fileKind int

const (
	kindRegular fileKind = iota
	kindLockfile
	kindGenerated
)

var lockfileNames = map[string]bool{
	"go.sum":            true,
	"package-lock.json": true,
	"yarn.lock":         true,
	"pnpm-lock.yaml":    true,
	"composer.lock":     true,
	"Gemfile.lock":      true,
	"Cargo.lock":        true,
	"Pipfile.lock":      true,
	"poetry.lock":       true,
	"bun.lockb":         true,
	"bun.lock":          true,
	"flake.lock":        true,
	"mix.lock":          true,
}

var generatedRe = regexp.MustCompile(
	`(?:\.pb\.go|\.pb\.gw\.go|_pb\.go|_gen\.go|\.gen\.go|\.generated\.[a-zA-Z0-9]+|_generated\.[a-zA-Z0-9]+)$`,
)

func classify(path string) fileKind {
	if path == "" {
		return kindRegular
	}
	if lockfileNames[filepath.Base(path)] {
		return kindLockfile
	}
	if generatedRe.MatchString(path) {
		return kindGenerated
	}
	return kindRegular
}
