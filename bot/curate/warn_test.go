package curate

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0x7461/guild/bot"
)

// The claude-code lane cannot run since 2026-10-11. It stays selectable, so the
// warning is the only thing standing between a config choice and a silent
// fallback — assert it fires.
func TestClaudeCodeCuratorWarnsLaneIsDead(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	// The call itself is expected to fail (the CLI refuses); the warning is the point.
	_, _ = (&ClaudeCodeCurator{Model: "haiku", Timeout: 5 * time.Second}).Curate([]bot.Item{{}}, 1)
	os.Stderr = old
	w.Close()
	out, _ := io.ReadAll(r)

	if !strings.Contains(string(out), "dead since 2026-10-11") {
		t.Fatalf("no dead-lane warning on stderr; got %q", out)
	}
}
