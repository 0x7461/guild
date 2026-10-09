// Package config loads per-bot JSON config from ~/.config/guild/<name>.json.
// If the file doesn't exist, callers keep their hardwired defaults.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// FeedEntry mirrors rss.FeedConfig for JSON serialization.
type FeedEntry struct {
	Name            string `json:"name"`
	URL             string `json:"url"`
	MaxItems        int    `json:"max_items"`
	DiscussionLabel string `json:"discussion_label,omitempty"`
	Favored         bool   `json:"favored,omitempty"`
}

// CurateConfig controls the LLM ranking pass between dedup and send.
type CurateConfig struct {
	Enabled         bool   `json:"enabled"`
	Target          int    `json:"target"`           // how many items to keep after ranking
	Backend         string `json:"backend"`          // "claude-code" or "ollama"
	Model           string `json:"model"`            // backend-specific model id
	FallbackBackend string `json:"fallback_backend"` // optional second backend
	FallbackModel   string `json:"fallback_model"`
	TimeoutSeconds  int    `json:"timeout_seconds"` // per-backend wall-clock cap
}

// ScoutConfig holds configuration for the combined GitHub + HN scout bot.
type ScoutConfig struct {
	GitHub struct {
		Period    string `json:"period"`
		Summarize bool   `json:"summarize"`
		Limit     int    `json:"limit"`
	} `json:"github"`
	HN struct {
		Days       int    `json:"days"`
		Annotate   bool   `json:"annotate"`
		Backend    string `json:"backend"` // "ollama" or "claude-code"
		Model      string `json:"model"`
		TimeoutSec int    `json:"timeout_seconds"`
	} `json:"hn"`
	Formatter struct {
		Title string `json:"title"`
	} `json:"formatter"`
}

// SummarizeConfig controls the per-item one-line summary pass before send.
type SummarizeConfig struct {
	Enabled        bool   `json:"enabled"`
	Backend        string `json:"backend"`         // "ollama" or "claude-code"
	Model          string `json:"model"`           // backend-specific model id
	TimeoutSeconds int    `json:"timeout_seconds"` // wall-clock cap for the batched call
}

// PaperboyConfig holds configuration for paperboy, the RSS digest bot.
type PaperboyConfig struct {
	Source struct {
		MaxDelivery int             `json:"max_delivery"`
		Feeds       []FeedEntry     `json:"feeds"`
		Curate      CurateConfig    `json:"curate"`
		Summarize   SummarizeConfig `json:"summarize"`
	} `json:"source"`
}

// NaggerConfig holds the weekly-quota-reset anchor for the nagger bot.
// Used only by the fallback cycle-day calc; the live path prefers the API's
// resets_at from ~/.local/share/nagger/rate-limits.json.
type NaggerConfig struct {
	ResetWeekday  int `json:"reset_weekday"`   // Monday=0
	ResetHour     int `json:"reset_hour"`      // 0-23
	ResetTZOffset int `json:"reset_tz_offset"` // hours from UTC

	// QuotaEnabled gates the daily Claude-quota pace nag. Defaults to true (set
	// in cmd/nagger's config literal, which Load unmarshals over) — set
	// "quota_enabled": false to silence it while there is no subscription to
	// pace. Reminders keep firing either way.
	QuotaEnabled bool `json:"quota_enabled"`

	Reminders []Reminder `json:"reminders"` // recurring manual-task nudges (quarterly archive chores, etc.)

	// SpendQuotaUSD is the monthly DeepSeek spend budget; 0 disables the spend
	// nag. WarnFractions are the budget fractions that each warn once per month
	// (default 0.5, 0.8, 0.9 — set in cmd/nagger).
	SpendQuotaUSD float64   `json:"spend_quota_usd"`
	WarnFractions []float64 `json:"warn_fractions"`
}

// Reminder is a recurring manual-task nudge fired by nagger on a fixed-day
// cadence. Fires when today >= (last-fired + EveryDays), or >= Anchor when
// never fired — so a missed run still fires late rather than skipping a cycle.
// Last-fired state lives in ~/.local/share/nagger/state.json (lastFired), keyed
// by ID; the config here is immutable and hand-editable.
type Reminder struct {
	ID        string `json:"id"`
	Message   string `json:"message"`
	EveryDays int    `json:"every_days"`
	Anchor    string `json:"anchor"` // ISO date (YYYY-MM-DD); first due date when no prior fire recorded
}

// Load reads ~/.config/guild/<name>.json into v.
// If the file does not exist, v is unchanged and nil is returned.
func Load(name string, v any) error {
	path := filepath.Join(dir(), name+".json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Save writes v as JSON to ~/.config/guild/<name>.json, creating the dir.
func Save(name string, v any) error {
	d := dir()
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, name+".json"), append(data, '\n'), 0o644)
}

func dir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "guild")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "guild")
}
