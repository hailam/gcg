package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// chunkBudgetBytes caps each individual response from read_file so a
	// huge file doesn't blow out the model's context window in one shot.
	chunkBudgetBytes = 64 * 1024

	// hardFileCap is the absolute file-size limit. Files larger than this
	// are refused outright — at this point you should be reading via grep
	// or a different tool, not stuffing it into an LLM.
	hardFileCap = 1024 * 1024

	// maxLineCount caps an explicit line_count request.
	maxLineCount = 5000
)

func init() {
	register(&Tool{
		Def: &mcp.Tool{
			Name: "read_file",
			Description: "Read the contents of a file in the current git repository. " +
				"By default returns the whole file, or the first ~64KB chunk if larger, with a " +
				"header indicating how to fetch the next chunk. Use the optional start_line and " +
				"line_count parameters to read a specific range — for example, after seeing diff " +
				"hunk markers (@@ -100,5 +100,8 @@) you can call read_file with start_line=95 and " +
				"line_count=20 to read context around the change. Path must be repository-relative.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Repository-relative path to the file."
					},
					"start_line": {
						"type": "integer",
						"description": "Optional 1-based line number to start reading from. Defaults to 1."
					},
					"line_count": {
						"type": "integer",
						"description": "Optional number of lines to read. If omitted, reads as many as fit in 64KB."
					}
				},
				"required": ["path"]
			}`),
		},
		Handler: readFile,
	})
}

func readFile(_ context.Context, args map[string]any) (string, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("read_file: missing path argument")
	}

	startLine := max(intArg(args, "start_line", 1), 1)
	lineCount := max(intArg(args, "line_count", 0), 0)
	lineCount = min(lineCount, maxLineCount)

	real, rootReal, err := resolveInsideRepo(path)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}

	ignored, err := gitIgnored(rootReal, real)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	if ignored {
		return "", fmt.Errorf("read_file: %q is gitignored — not accessible", path)
	}

	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("read_file: %q is a directory; use list_dir", path)
	}
	if info.Size() > hardFileCap {
		return "", fmt.Errorf("read_file: file too large (%d bytes; absolute max %d)", info.Size(), hardFileCap)
	}

	data, err := os.ReadFile(real)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	// strings.Split on text ending in \n leaves a trailing empty element;
	// drop it so totalLines reflects what a human would count.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	totalLines := len(lines)

	if startLine > totalLines {
		return fmt.Sprintf("[file has %d lines; start_line=%d is past the end]", totalLines, startLine), nil
	}

	// Whole-file fast path: no range requested and the whole thing fits.
	if startLine == 1 && lineCount == 0 && len(data) <= chunkBudgetBytes {
		return string(data), nil
	}

	// Slice [startLine-1, end), bounded by lineCount and chunkBudgetBytes.
	bytes := 0
	end := startLine - 1
	emitted := make([]string, 0, lineCount)
	for end < totalLines {
		if lineCount > 0 && end-(startLine-1) >= lineCount {
			break
		}
		ln := lines[end]
		next := bytes + len(ln) + 1
		if next > chunkBudgetBytes && len(emitted) > 0 {
			break
		}
		emitted = append(emitted, ln)
		bytes = next
		end++
	}

	hint := ""
	if end < totalLines {
		hint = fmt.Sprintf(" To continue, call read_file with start_line=%d.", end+1)
	}
	hdr := fmt.Sprintf("[lines %d-%d of %d; file size %d bytes.%s]\n",
		startLine, end, totalLines, info.Size(), hint)
	return hdr + strings.Join(emitted, "\n"), nil
}

// intArg extracts an integer argument from a tool-call args map. JSON
// numbers come back as float64; some clients may emit json.Number or
// strings, so accept those too.
func intArg(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i)
		}
	}
	return def
}
