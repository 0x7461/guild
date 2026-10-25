package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func assertChunksFit(t *testing.T, chunks []string) {
	t.Helper()
	for i, c := range chunks {
		if len(c) > telegramMaxLen {
			t.Fatalf("chunk %d is %d bytes, over the %d limit", i, len(c), telegramMaxLen)
		}
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d is not valid UTF-8 (split mid-rune)", i)
		}
	}
}

func TestSplitMessageShortPassesThrough(t *testing.T) {
	msg := "<b>Scout</b>\n\none item"
	chunks := splitMessage(msg)
	if len(chunks) != 1 || chunks[0] != msg {
		t.Fatalf("expected the message untouched, got %d chunks", len(chunks))
	}
}

// One annotated repo entry can exceed a whole message on its own; the splitter
// used to place it in a chunk regardless, which Telegram rejected as
// "message is too long" (2026-10-25).
func TestSplitMessageOversizedSegment(t *testing.T) {
	entry := strings.Repeat("a long annotation line\n", 600) // ~13 KB
	msg := "<b>GitHub</b>\n\n" + entry + repoSeparator + "<b>HN</b>\n\nshort"

	chunks := splitMessage(msg)
	if len(chunks) < 4 {
		t.Fatalf("expected the oversized entry to split, got %d chunks", len(chunks))
	}
	assertChunksFit(t, chunks)

	joined := strings.Join(chunks, "\n")
	for _, want := range []string{"GitHub", "short"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("content %q lost while splitting", want)
		}
	}
}

func TestSplitMessageRuneSafety(t *testing.T) {
	// 3-byte runes, no newlines at all, so only the rune-boundary path applies.
	msg := strings.Repeat("—", telegramMaxLen) // ~12 KB
	chunks := splitMessage(msg)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	assertChunksFit(t, chunks)
}

func TestSplitMessageRespectsBreakSentinel(t *testing.T) {
	a := "<b>Part A</b>\n\n" + strings.Repeat("x\n", 3000)
	b := "<b>Part B</b>"
	chunks := splitMessage(a + "\x00\x00BREAK\x00\x00" + b)
	assertChunksFit(t, chunks)
}
