package curate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/0x7461/guild/bot"
)

// Annotation is the per-item enrichment returned by the model.
type Annotation struct {
	Summary   string `json:"summary"`
	Sentiment string `json:"sentiment"` // civil | mixed | heated | toxic
}

const annotatePrompt = `For each discussion-board post below, return:
- "summary": one neutral sentence on what the post and its discussion are about.
- "sentiment": the overall tone of the comment thread, exactly one of:
  civil, mixed, heated, toxic.

Base sentiment on the comments provided. "toxic" = personal attacks, hostility,
or pervasive bad faith; "heated" = sharp disagreement but on substance;
"mixed" = a blend; "civil" = constructive and respectful.

Return a JSON array only — no prose, no markdown fences — one object per post in
the same order:
[{"summary": "...", "sentiment": "..."}]

Posts:
%s`

// Annotate best-effort fills Meta["summary"] and Meta["sentiment"] for each
// item from its Title and Meta["context"] (e.g. top comments). On any failure
// it logs and leaves items unchanged, so callers can always ship the digest.
func Annotate(items []bot.Item, backend, model string, timeout time.Duration) {
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
		if ctx := strings.TrimSpace(it.Meta["context"]); ctx != "" {
			sb.WriteString("comments:\n" + ctx + "\n")
		}
		sb.WriteString("\n")
	}
	prompt := fmt.Sprintf(annotatePrompt, sb.String())

	out, err := runText(backend, model, timeout, prompt)
	if err != nil {
		fmt.Printf("annotate: %v\n", err)
		return
	}
	anns, err := parseAnnotations(out)
	if err != nil {
		fmt.Printf("annotate: parse: %v (raw: %q)\n", err, truncate(out, 200))
		return
	}
	for i := range items {
		if i >= len(anns) {
			break
		}
		if anns[i].Summary != "" {
			items[i].Meta["summary"] = anns[i].Summary
		}
		if anns[i].Sentiment != "" {
			items[i].Meta["sentiment"] = strings.ToLower(strings.TrimSpace(anns[i].Sentiment))
		}
	}
}

// runText dispatches one text pass to the named backend. Routing is explicit:
// an unrecognised backend is an error, never a silent default — the backends
// differ in what they do with the prompt, not just in price.
func runText(backend, model string, timeout time.Duration, prompt string) (string, error) {
	switch backend {
	case "ollama":
		return runOllamaText(model, timeout, prompt)
	case "openai", "opencode":
		return runOpenAIText(model, timeout, prompt)
	case "claude-code":
		warnClaudeDead()
		return runClaudeText(model, timeout, prompt)
	case "":
		return "", fmt.Errorf("no backend configured")
	default:
		return "", fmt.Errorf("unknown backend %q", backend)
	}
}

// runOllamaText runs one text pass against a local Ollama model.
//
// num_predict is explicit: Ollama returns an EMPTY response with
// done_reason "length" when the cap is hit, and an empty string reaches the
// caller as "no JSON array found" rather than as a truncation — a silent
// quality loss. num_ctx likewise: the default is 8192 (4096 on /v1) and
// Ollama truncates the prompt past it without erroring.
func runOllamaText(model string, timeout time.Duration, prompt string) (string, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
		"think":  false,
		"options": map[string]any{
			"temperature": 0.2,
			"num_ctx":     ollamaNumCtx,
			"num_predict": ollamaNumPredict,
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", ollamaBaseURL+"/api/generate", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama: HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Response   string `json:"response"`
		DoneReason string `json:"done_reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("ollama: decode: %w", err)
	}
	if strings.TrimSpace(parsed.Response) == "" {
		return "", fmt.Errorf("ollama: empty response (done_reason %q) — raise num_predict", parsed.DoneReason)
	}
	return parsed.Response, nil
}

// runOpenAIText runs one text pass on the OpenAI-compatible lane (OpenCode Go).
// Added 2026-10-24 so scout's HN annotation no longer needs a local Ollama: the
// X1C has no dGPU and no models. The key comes from OPENCODE_API_KEY, which the
// bot picks up from guild's .env via bot.LoadEnv.
func runOpenAIText(model string, timeout time.Duration, prompt string) (string, error) {
	out, err := openAIChat(openAIChatRequest{
		Model:   model,
		APIKey:  os.Getenv("OPENCODE_API_KEY"),
		Timeout: timeout,
		Prompt:  prompt,
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("openai: empty response")
	}
	return out, nil
}

// runClaudeText shells out to `claude -p` and returns its text output. It mirrors
// ClaudeCodeCurator's error handling: claude writes quota/auth errors to stdout,
// not stderr, so fall back to stdout content when stderr is empty.
func runClaudeText(model string, timeout time.Duration, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{"-p", prompt, "--model", model, "--output-format", "text", "--allowedTools", ""}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = "/tmp"
	cmd.Env = filterEnv("CLAUDECODE")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(output))
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("claude -p: %s", msg)
	}
	return string(output), nil
}

// firstJSONArray returns the first '['-anchored substring that decodes as a JSON
// value. The old greedy `(?s)\[.*\]` regex spanned from a '[' in the model's
// prose to the last ']', so decode failed and the annotations were skipped.
func firstJSONArray(output string) ([]byte, bool) {
	for i := 0; i < len(output); i++ {
		if output[i] != '[' {
			continue
		}
		var raw json.RawMessage
		if err := json.NewDecoder(strings.NewReader(output[i:])).Decode(&raw); err != nil {
			continue
		}
		if len(raw) > 0 && raw[0] == '[' {
			return raw, true
		}
	}
	return nil, false
}

func parseAnnotations(output string) ([]Annotation, error) {
	match, ok := firstJSONArray(output)
	if !ok {
		return nil, fmt.Errorf("no JSON array found")
	}
	var anns []Annotation
	if err := json.Unmarshal(match, &anns); err != nil {
		return nil, err
	}
	return anns, nil
}
