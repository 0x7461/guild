// Package curate ranks RSS items by user interest using an LLM.
// Two backends: claude-code (shell out to `claude -p`) and ollama (HTTP).
// A ChainCurator tries them in order; if all fail, callers pass items through
// unchanged so the digest still ships.
package curate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/0x7461/guild/bot"
)

// claudeDeadOnce prints the dead-lane warning once per process. The claude-code
// path is kept rather than deleted (CC may come back), but since 2026-10-11
// `claude -p` answers every call with "Your organization has disabled Claude
// subscription access for Claude Code", so a run that reaches it gets no output.
// Loud, because the caller's error path otherwise looks like any other failure.
var claudeDeadOnce sync.Once

func warnClaudeDead() {
	claudeDeadOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "curate: claude-code lane is dead since 2026-10-11 "+
			"(the subscription refuses `claude -p`); this run cannot succeed — set the backend to ollama\n")
	})
}

// Curator ranks items and returns at most `target` of them, ordered best-first.
type Curator interface {
	Curate(items []bot.Item, target int) ([]bot.Item, error)
	Name() string
}

// ChainCurator tries each backend in order. First success wins.
type ChainCurator struct {
	Curators []Curator
}

func (c *ChainCurator) Name() string { return "chain" }

func (c *ChainCurator) Curate(items []bot.Item, target int) ([]bot.Item, error) {
	var errs []string
	for _, cur := range c.Curators {
		out, err := cur.Curate(items, target)
		if err == nil {
			fmt.Printf("curate: %s ranked %d -> %d items\n", cur.Name(), len(items), len(out))
			return out, nil
		}
		fmt.Printf("curate: %s failed: %v\n", cur.Name(), err)
		errs = append(errs, fmt.Sprintf("%s: %v", cur.Name(), err))
	}
	return nil, fmt.Errorf("all curators failed: %s", strings.Join(errs, "; "))
}

// ClaudeCodeCurator shells out to `claude -p` (without --bare — see the note in
// Curate: --bare would skip auth discovery too). **Dead lane since 2026-10-11** —
// kept for a possible CC return; the first call warns.
type ClaudeCodeCurator struct {
	Model   string        // "haiku" | "sonnet" | "opus" — or full model ID
	Timeout time.Duration // hard wall-clock cap
}

func (c *ClaudeCodeCurator) Name() string { return "claude-code/" + c.Model }

var claudeModelMap = map[string]string{
	"haiku":  "claude-haiku-4-5-20251001",
	"sonnet": "claude-sonnet-4-6",
	"opus":   "claude-opus-4-7",
}

func (c *ClaudeCodeCurator) Curate(items []bot.Item, target int) ([]bot.Item, error) {
	if len(items) == 0 {
		return nil, nil
	}
	warnClaudeDead()
	model := c.Model
	if full, ok := claudeModelMap[model]; ok {
		model = full
	}

	prompt := buildPrompt(items, target)

	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()

	// Note: --bare skips auth discovery as well as CLAUDE.md/settings, so we
	// keep it off here. Daily once-a-day cost of full discovery is fine.
	args := []string{"-p", prompt, "--model", model, "--output-format", "text", "--allowedTools", ""}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = os.TempDir()
	cmd.Env = filterEnv("CLAUDECODE")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			// claude -p writes quota/auth errors to stdout, not stderr
			msg = strings.TrimSpace(string(output))
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("claude -p: %s", msg)
	}

	return applyRanking(items, target, string(output))
}

// Shared Ollama tuning. Both defaults are traps rather than merely small:
// Ollama silently TRUNCATES a prompt longer than num_ctx (default 8192, or 4096
// on /v1), and returns an EMPTY response with done_reason "length" when
// num_predict is reached. Neither surfaces as an error, so both read downstream
// as "the model gave us nothing useful". Measured 2026-09-24 on gemma4:e4b: a
// 120-item curate prompt is ~9.7k tokens and needs well over 300 output tokens.
const (
	ollamaBaseURL    = "http://localhost:11434"
	ollamaNumCtx     = 32768
	ollamaNumPredict = 2000
)

// OllamaCurator hits a local Ollama server via /api/generate.
type OllamaCurator struct {
	Model   string        // e.g. "gemma4:e4b"
	BaseURL string        // default http://localhost:11434
	Timeout time.Duration // hard wall-clock cap
}

func (o *OllamaCurator) Name() string { return "ollama/" + o.Model }

func (o *OllamaCurator) Curate(items []bot.Item, target int) ([]bot.Item, error) {
	if len(items) == 0 {
		return nil, nil
	}
	baseURL := o.BaseURL
	if baseURL == "" {
		baseURL = ollamaBaseURL
	}

	prompt := buildPrompt(items, target)

	reqBody, _ := json.Marshal(map[string]any{
		"model":  o.Model,
		"prompt": prompt,
		"stream": false,
		"think":  false,
		"format": "json",
		"options": map[string]any{
			"temperature": 0.2,
			"num_ctx":     ollamaNumCtx,
			"num_predict": ollamaNumPredict,
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/generate", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("ollama: decode: %w", err)
	}

	return applyRanking(items, target, parsed.Response)
}

// buildPrompt assembles the ranking prompt from the SystemPrompt template.
func buildPrompt(items []bot.Item, target int) string {
	var sb strings.Builder
	for i, it := range items {
		desc := stripHTML(it.Description)
		if len(desc) > 200 {
			desc = strings.TrimSpace(desc[:200]) + "…"
		}
		mark := ""
		if it.Meta["favored"] == "true" {
			mark = "★ "
		}
		sb.WriteString(fmt.Sprintf("  [%d] %s(%s) %s — %s\n", i, mark, it.Meta["feed"], it.Title, desc))
	}
	return fmt.Sprintf(SystemPrompt, target, sb.String())
}

// applyRanking parses model output and returns items in ranked order.
// Items not mentioned by the model are dropped. Tolerant of preamble/fences.
func applyRanking(items []bot.Item, target int, output string) ([]bot.Item, error) {
	indices, err := parseRanked(output, len(items))
	if err != nil {
		return nil, fmt.Errorf("parse: %w (raw: %q)", err, truncate(output, 200))
	}
	if len(indices) < target/2 {
		return nil, fmt.Errorf("too few items returned: got %d, want >=%d", len(indices), target/2)
	}
	if len(indices) > target {
		indices = indices[:target]
	}
	out := make([]bot.Item, 0, len(indices))
	for _, idx := range indices {
		out = append(out, items[idx])
	}
	return out, nil
}

var jsonObjRE = regexp.MustCompile(`(?s)\{[^{}]*"ranked"[^{}]*\}`)

func parseRanked(output string, total int) ([]int, error) {
	match := jsonObjRE.FindString(output)
	if match == "" {
		return nil, fmt.Errorf("no JSON object with 'ranked' key")
	}
	var parsed struct {
		Ranked []int `json:"ranked"`
	}
	if err := json.Unmarshal([]byte(match), &parsed); err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var clean []int
	for _, idx := range parsed.Ranked {
		if idx < 0 || idx >= total || seen[idx] {
			continue
		}
		seen[idx] = true
		clean = append(clean, idx)
	}
	return clean, nil
}

var htmlTagRE = regexp.MustCompile(`<[^>]*>`)

func stripHTML(s string) string {
	s = htmlTagRE.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

func filterEnv(exclude string) []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, exclude+"=") {
			env = append(env, e)
		}
	}
	return env
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
