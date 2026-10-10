package rss

import (
	"fmt"
	"strings"

	"github.com/0x7461/guild/bot"
)

// Formatter renders RSS items as Telegram HTML under a single "Today's picks"
// heading — every item has been through the ranker, so there is no longer a
// bypassed-blogs category to separate out. Items are clustered by feed, and
// long runs are split into numbered messages so each stays under the Telegram
// 4096-char per-message cap.
type Formatter struct{}

const (
	maxMessageChars = 4000
	picksHeader     = "🔥 <b>Today's picks</b>"
)

// Format satisfies bot.Formatter — joins all messages (not used in main path).
func (f *Formatter) Format(items []bot.Item) string {
	return strings.Join(f.FormatAll(items), "\n\n")
}

// FormatAll returns one or more Telegram HTML messages.
func (f *Formatter) FormatAll(items []bot.Item) []string {
	if len(items) == 0 {
		return nil
	}

	return splitMessage(picksHeader, renderLines(clusterByFeed(items)))
}

// clusterByFeed groups items from the same feed together while preserving
// the relative order of feeds based on first appearance in the input.
// e.g. [HN-A, Simon-X, HN-B, Hackaday-1] -> [HN-A, HN-B, Simon-X, Hackaday-1].
// This keeps the curator's ranking signal (best feed first) without scattering
// same-source items across the message.
func clusterByFeed(items []bot.Item) []bot.Item {
	var feedOrder []string
	groups := map[string][]bot.Item{}
	for _, it := range items {
		feed := it.Meta["feed"]
		if _, ok := groups[feed]; !ok {
			feedOrder = append(feedOrder, feed)
		}
		groups[feed] = append(groups[feed], it)
	}
	out := make([]bot.Item, 0, len(items))
	for _, feed := range feedOrder {
		out = append(out, groups[feed]...)
	}
	return out
}

func renderLines(items []bot.Item) []string {
	lines := make([]string, 0, len(items))
	for _, it := range items {
		feed := it.Meta["feed"]
		line := fmt.Sprintf("• [%s] <a href=\"%s\">%s</a>",
			escapeHTML(feed), escapeHTML(it.URL), escapeHTML(it.Title))
		if disc := it.Meta["discussion"]; disc != "" {
			label := it.Meta["discussion_label"]
			if label == "" {
				label = "Discussion"
			}
			line += fmt.Sprintf(" · <a href=\"%s\">%s</a>", escapeHTML(disc), escapeHTML(label))
		}
		if summary := it.Meta["summary"]; summary != "" {
			line += fmt.Sprintf("\n  <i>%s</i>", escapeHTML(summary))
		}
		lines = append(lines, line)
	}
	return lines
}

// splitMessage chunks lines so each rendered message stays under
// maxMessageChars. Header is repeated; "(i/N)" is appended when N>1.
func splitMessage(header string, lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	var chunks [][]string
	var cur []string
	curLen := len(header) + 1 // header + newline
	for _, line := range lines {
		add := len(line) + 1 // line + newline
		// +10 reserve for possible "(NN/NN)" suffix on the header
		if curLen+add+10 > maxMessageChars && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur = nil
			curLen = len(header) + 1
		}
		cur = append(cur, line)
		curLen += add
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}

	out := make([]string, len(chunks))
	for i, chunk := range chunks {
		h := header
		if len(chunks) > 1 {
			h = fmt.Sprintf("%s (%d/%d)", header, i+1, len(chunks))
		}
		out[i] = h + "\n" + strings.Join(chunk, "\n")
	}
	return out
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	// Escaping the quote keeps href="…" intact when a URL or label contains one.
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
