package compress

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// Summarize runs the summarize CLI on a URL and returns the first paragraph
// as a compact description. Returns empty string if the CLI is unavailable or fails.
func Summarize(url string) string {
	// A bare exec.Command has no timeout; a stuck summarize process would hang
	// the fetch that called it.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "summarize", url, "--cli", "claude", "--plain", "--length", "short")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return firstParagraph(out.String())
}

// firstParagraph extracts the first non-empty block of text before a blank line.
func firstParagraph(s string) string {
	s = strings.TrimSpace(s)
	var para []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, trimmed)
	}
	return strings.Join(para, " ")
}
