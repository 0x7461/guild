package curate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/0x7461/guild/bot"
)

const summarizePrompt = `For each article below, write one neutral sentence summarizing what it is about, for a reader scanning a news digest. Base it only on the title and description provided — do not speculate beyond them.

Return a JSON array of strings only — no prose, no markdown fences — one summary per article in the same order:
["...", "..."]

Articles:
%s`

// Summarize best-effort fills Meta["summary"] for each item from its Title and
// Description in a single batched model call. On any failure it logs and leaves
// items unchanged, so callers can always ship the digest (mirrors Annotate).
func Summarize(items []bot.Item, backend, model string, timeout time.Duration) {
	if len(items) == 0 {
		return
	}
	if backend == "claude-code" {
		if full, ok := claudeModelMap[model]; ok {
			model = full
		}
	}

	var sb strings.Builder
	for i, it := range items {
		sb.WriteString(fmt.Sprintf("[%d] %s\n", i, it.Title))
		if d := strings.TrimSpace(it.Description); d != "" {
			sb.WriteString(d + "\n")
		}
		sb.WriteString("\n")
	}
	prompt := fmt.Sprintf(summarizePrompt, sb.String())

	out, err := runText(backend, model, timeout, prompt)
	if err != nil {
		fmt.Printf("summarize: %v\n", err)
		return
	}
	match, ok := firstJSONArray(out)
	if !ok {
		fmt.Printf("summarize: no JSON array (raw: %q)\n", truncate(out, 200))
		return
	}
	var summaries []string
	if err := json.Unmarshal(match, &summaries); err != nil {
		fmt.Printf("summarize: parse: %v (raw: %q)\n", err, truncate(out, 200))
		return
	}
	for i := range items {
		if i >= len(summaries) {
			break
		}
		if s := strings.TrimSpace(summaries[i]); s != "" {
			items[i].Meta["summary"] = s
		}
	}
}
