// Package scout formats a mixed GitHub-trending + Hacker News digest,
// grouped into one section per source.
package scout

import (
	"fmt"
	"strings"

	"github.com/0x7461/guild/bot"
)

// Formatter renders items grouped by Meta["source"], in first-seen order.
type Formatter struct {
	Title string
}

func (f *Formatter) Format(items []bot.Item) string {
	// No items means no digest: a header-only message reads as a delivered
	// digest and hides the empty fetch.
	if len(items) == 0 {
		return ""
	}
	// Group by source, preserving the order sources first appear.
	var order []string
	groups := map[string][]bot.Item{}
	for _, it := range items {
		src := it.Meta["source"]
		if src == "" {
			src = "Other"
		}
		if _, seen := groups[src]; !seen {
			order = append(order, src)
		}
		groups[src] = append(groups[src], it)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<b>%s</b>\n\n", escapeHTML(f.Title)))

	for si, src := range order {
		if si > 0 {
			sb.WriteString(bot.MessageBreak) // each source becomes its own message
		}
		sb.WriteString(fmt.Sprintf("<b>━ %s ━</b>\n\n", escapeHTML(src)))
		for i, item := range groups[src] {
			if item.Meta["hn_type"] != "" {
				writeHN(&sb, i+1, item)
			} else {
				writeGitHub(&sb, i+1, item)
			}
			sb.WriteString("\n---\n\n")
		}
	}

	return sb.String()
}

func writeHN(sb *strings.Builder, n int, item bot.Item) {
	sb.WriteString(fmt.Sprintf("<b>%d. %s</b>  <code>%s</code>\n", n, escapeHTML(item.Title), escapeHTML(item.Meta["hn_type"])))
	stat := fmt.Sprintf("%s points · %s comments", item.Meta["points"], item.Meta["comments"])
	sb.WriteString(fmt.Sprintf("<a href=\"%s\">%s</a>\n", escapeHTML(item.URL), escapeHTML(stat)))
	if summary := item.Meta["summary"]; summary != "" {
		sb.WriteString(fmt.Sprintf("<i>%s</i>\n", escapeHTML(summary)))
	}
	if sent := item.Meta["sentiment"]; sent != "" {
		sb.WriteString(fmt.Sprintf("Thread: %s\n", escapeHTML(sent)))
	}
}

func writeGitHub(sb *strings.Builder, n int, item bot.Item) {
	sb.WriteString(fmt.Sprintf("<b>%d. %s</b>\n", n, escapeHTML(item.Title)))
	sb.WriteString(fmt.Sprintf("<a href=\"%s\">View on GitHub</a>\n", escapeHTML(item.URL)))
	if item.Description != "" {
		sb.WriteString(fmt.Sprintf("%s\n", escapeHTML(item.Description)))
	}
	if summary := item.Meta["summary"]; summary != "" {
		sb.WriteString(fmt.Sprintf("<i>%s</i>\n", escapeHTML(summary)))
	}
	var tail []string
	if lang := item.Meta["language"]; lang != "" {
		tail = append(tail, "<code>"+escapeHTML(lang)+"</code>")
	}
	if stars := item.Meta["stars"]; stars != "" {
		tail = append(tail, "⭐ "+escapeHTML(stars))
	}
	if len(tail) > 0 {
		sb.WriteString(strings.Join(tail, " · ") + "\n")
	}
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	// Escaping the quote keeps href="…" intact when a URL contains one.
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
