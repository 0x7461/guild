// nagger — daily Telegram nudges from a unified set of periodic "nags": the
// Claude weekly-quota pace check and recurring manual-task reminders. Each nag is
// a periodic signal (interval + anchor + renderer); one snooze run evaluates them
// all, fires the due ones as a single combined message (grouped under headers),
// and dedups per-nag via ~/.local/share/nagger/state.json. Run hourly 08–22 by
// runit + snooze; the per-nag dedup makes re-runs idempotent.
//
// Inputs:
//   - ~/.local/share/nagger/rate-limits.json  (written by ~/.claude/statusline.sh
//     after every CC response — external producer, do not move)
//   - ~/.config/guild/nagger.json            (quota reset anchor, reminders)
//   - ~/.local/share/nagger/state.json        (per-nag last-fired dates, owned here;
//     migrated from the flat id→date map and, before that, the legacy last-sent +
//     reminders-state.json files)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0x7461/guild/bot"
	"github.com/0x7461/guild/config"
	"github.com/0x7461/guild/senders/telegram"
)

// Pace cycle: index = day-1 → (label, target % weekly usage by end of that day).
var cycle = []struct {
	Label  string
	Target int
}{
	{"Heavy Build", 18},
	{"Heavy Build", 34},
	{"Heavy Build", 50},
	{"Refinement", 63},
	{"Refinement", 75},
	{"Buffer", 85},
	{"Careful Sprint", 95},
}

func stateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "nagger")
}

type rateLimits struct {
	SevenDay         int   `json:"seven_day"`
	SevenDayResetsAt int64 `json:"seven_day_resets_at"`
}

func readRateLimits() (*rateLimits, bool) {
	data, err := os.ReadFile(filepath.Join(stateDir(), "rate-limits.json"))
	if err != nil {
		return nil, false
	}
	var rl rateLimits
	if err := json.Unmarshal(data, &rl); err != nil {
		return nil, false
	}
	return &rl, true
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// getCycleDay prefers the API's actual reset timestamp (rolling 7-day window);
// falls back to the configured weekday/hour anchor when the cache is missing.
func getCycleDay(resetsAt int64, cfg config.NaggerConfig) int {
	if resetsAt > 0 {
		secsRemaining := float64(resetsAt) - float64(time.Now().Unix())
		daysElapsed := 7 - secsRemaining/86400
		return clamp(int(daysElapsed)+1, 1, 7)
	}
	loc := time.FixedZone("local", cfg.ResetTZOffset*3600)
	now := time.Now().In(loc)
	weekday := (int(now.Weekday()) + 6) % 7 // Go: Sunday=0 → Monday=0
	daysSince := (weekday - cfg.ResetWeekday + 7) % 7
	lastReset := time.Date(now.Year(), now.Month(), now.Day()-daysSince, cfg.ResetHour, 0, 0, 0, loc)
	if now.Before(lastReset) {
		lastReset = lastReset.AddDate(0, 0, -7)
	}
	days := int(now.UTC().Sub(lastReset.UTC()).Hours() / 24)
	return clamp(days+1, 1, 7)
}

func formatReset(ts int64) string {
	if ts == 0 {
		return ""
	}
	delta := time.Until(time.Unix(ts, 0))
	if delta <= 0 {
		return "resets now"
	}
	days := int(delta.Hours()) / 24
	hours := int(delta.Hours()) % 24
	mins := int(delta.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("resets in %dd %dh", days, hours)
	}
	return fmt.Sprintf("resets in %dh %dm", hours, mins)
}

// --- Unified nag engine ---

// Nag is one periodic signal. It fires when today >= last-fired + IntervalDays;
// when never fired, it fires at >= Anchor, or immediately when Anchor is empty
// (the quota case). Nags sharing a Group render under one header in the combined
// message. Render returns the message block and ok=false to skip firing.
type Nag struct {
	ID           string
	Group        string
	IntervalDays int
	Anchor       string
	Render       func(now time.Time) (string, bool)
}

// groupHeader gives a shared header for nags of a group; groups absent here
// render their blocks headerless. groupOrder fixes section order in the message.
var groupHeader = map[string]string{
	"reminder": "🔔 Quarterly archive reminder — not urgent, do when convenient:",
}
var groupOrder = []string{"reminder", "quota"}

func parseDay(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

// dueNag reports whether nag n is due as of today (a UTC-midnight date).
func dueNag(n Nag, state map[string]string, today time.Time) bool {
	if lf, ok := parseDay(state[n.ID]); ok {
		return !today.Before(lf.AddDate(0, 0, n.IntervalDays))
	}
	if n.Anchor == "" {
		return true // never fired, no anchor → fire now (quota)
	}
	if a, ok := parseDay(n.Anchor); ok {
		return !today.Before(a)
	}
	return false // anchor set but unparseable → don't fire blindly
}

func statePath() string { return filepath.Join(stateDir(), "state.json") }

// naggerState is state.json: per-nag last-fired dates.
type naggerState struct {
	LastFired map[string]string `json:"lastFired"`
}

// readState loads state.json. A flat id→date map (the format until 2026-09-29)
// becomes LastFired. If state.json doesn't exist yet, it migrates once from the
// pre-unification files (last-sent → "quota"; the reminders-state.json map merged in).
func readState() (*naggerState, error) {
	st := &naggerState{}
	data, err := os.ReadFile(statePath())
	if err == nil {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(data, &probe); err != nil {
			// A truncated state.json used to read back as an empty map, so every
			// reminder re-fired. Refuse instead.
			return nil, fmt.Errorf("state.json is not valid JSON: %w", err)
		}
		if _, nested := probe["lastFired"]; nested {
			if err := json.Unmarshal(data, st); err != nil {
				return nil, fmt.Errorf("state.json is not valid JSON: %w", err)
			}
		} else if err := json.Unmarshal(data, &st.LastFired); err != nil {
			return nil, fmt.Errorf("state.json is not valid JSON: %w", err)
		}
		if st.LastFired == nil {
			st.LastFired = map[string]string{}
		}
		return st, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read state.json: %w", err)
	}
	// First run: inherit the pre-unification files.
	st.LastFired = map[string]string{}
	if b, err := os.ReadFile(filepath.Join(stateDir(), "last-sent")); err == nil {
		if d := strings.TrimSpace(string(b)); d != "" {
			st.LastFired["quota"] = d
		}
	}
	if b, err := os.ReadFile(filepath.Join(stateDir(), "reminders-state.json")); err == nil {
		legacy := map[string]string{}
		if json.Unmarshal(b, &legacy) == nil {
			for k, v := range legacy {
				st.LastFired[k] = v
			}
		}
	}
	return st, nil
}

func writeState(m *naggerState) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	// Write to a temp file and rename: a crash mid-write left a truncated
	// state.json, which read back as empty and re-fired every nag.
	tmp, err := os.CreateTemp(stateDir(), "state.json.tmp-*")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, statePath()); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

type firedBlock struct {
	group string
	block string
}

// buildMessage joins due blocks into one message: grouped blocks share a header,
// sections ordered by groupOrder (then any leftover groups, first-seen order).
func buildMessage(fired []firedBlock) string {
	byGroup := map[string][]string{}
	var seen []string
	for _, f := range fired {
		if _, ok := byGroup[f.group]; !ok {
			seen = append(seen, f.group)
		}
		byGroup[f.group] = append(byGroup[f.group], f.block)
	}
	emit := func(g string) string {
		if h, ok := groupHeader[g]; ok {
			return h + "\n" + strings.Join(byGroup[g], "\n")
		}
		return strings.Join(byGroup[g], "\n")
	}
	var sections []string
	done := map[string]bool{}
	for _, g := range groupOrder {
		if len(byGroup[g]) > 0 {
			sections = append(sections, emit(g))
			done[g] = true
		}
	}
	for _, g := range seen {
		if !done[g] {
			sections = append(sections, emit(g))
		}
	}
	return strings.Join(sections, "\n\n")
}

func buildSender() *telegram.Sender {
	token := bot.FirstNonEmpty(os.Getenv("BOT_NAGGER__TOKEN"), os.Getenv("TELEGRAM_BOT_TOKEN"))
	var chatID int64
	chatStr := bot.FirstNonEmpty(os.Getenv("BOT_NAGGER__CHAT"), os.Getenv("TELEGRAM_CHAT_ID"))
	if chatStr != "" {
		if _, err := fmt.Sscanf(chatStr, "%d", &chatID); err != nil {
			log.Fatalf("invalid chat ID %q: %v", chatStr, err)
		}
	}
	if token == "" || chatID == 0 {
		log.Fatal("ENABLE_TELEGRAM=true but BOT_NAGGER__TOKEN/BOT_NAGGER__CHAT (or TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID) is missing")
	}
	return &telegram.Sender{Token: token, ChatID: chatID}
}

// quotaNag is the daily Claude-quota pace check — an interval-1 nag with a
// live-computed message (cycle day, target %, actual usage, reset time).
func quotaNag(cfg config.NaggerConfig) Nag {
	return Nag{
		ID: "quota", Group: "quota", IntervalDays: 1, Anchor: "",
		Render: func(now time.Time) (string, bool) {
			var resetsAt int64
			var actual *int
			if rl, ok := readRateLimits(); ok {
				resetsAt = rl.SevenDayResetsAt
				a := rl.SevenDay
				actual = &a
			}
			day := getCycleDay(resetsAt, cfg)
			c := cycle[day-1]
			floor := int(float64(c.Target) * 0.7)

			var paceLine string
			switch {
			case actual == nil:
				paceLine = "Actual: unknown (open CC to refresh)"
			case *actual < floor:
				paceLine = fmt.Sprintf("Actual: %d%% — under-using 💸 (target %d%%, floor %d%%)", *actual, c.Target, floor)
			case *actual <= c.Target:
				paceLine = fmt.Sprintf("Actual: %d%% — on pace ✓", *actual)
			default:
				paceLine = fmt.Sprintf("Actual: %d%% — over pace ⚠️ (target %d%%)", *actual, c.Target)
			}
			lines := []string{
				fmt.Sprintf("📊 Claude quota check — Day %d/7 (%s)", day, c.Label),
				fmt.Sprintf("Target: ~%d%% weekly usage by end of today.", c.Target),
				paceLine,
			}
			if rs := formatReset(resetsAt); rs != "" {
				lines = append(lines, strings.ToUpper(rs[:1])+rs[1:]+".")
			}
			return strings.Join(lines, "\n"), true
		},
	}
}

// reminderNags expands the config reminders into nags. A reminder needs an
// explicit anchor (never the fire-now empty-anchor path — that's quota-only).
func reminderNags(cfg config.NaggerConfig) []Nag {
	var nags []Nag
	for _, r := range cfg.Reminders {
		if r.ID == "" || r.EveryDays <= 0 || r.Anchor == "" {
			continue
		}
		msg := "• " + r.Message
		nags = append(nags, Nag{
			ID: r.ID, Group: "reminder", IntervalDays: r.EveryDays, Anchor: r.Anchor,
			Render: func(now time.Time) (string, bool) { return msg, true },
		})
	}
	return nags
}

func main() {
	bot.LoadEnv("nagger")

	cfg := config.NaggerConfig{ResetWeekday: 0, ResetHour: 11, ResetTZOffset: 7, QuotaEnabled: true} // migrated defaults
	if err := config.Load("nagger", &cfg); err != nil {
		// A malformed nagger.json used to be logged and ignored, silently
		// restoring defaults (quota nag back on, spend budget 0 = off).
		log.Fatalf("nagger: could not load nagger config: %v — refusing to run with defaults", err)
	}

	enabled := os.Getenv("ENABLE_TELEGRAM") == "true"
	loc := time.FixedZone("local", cfg.ResetTZOffset*3600)
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")
	todayDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	nags := reminderNags(cfg)
	if cfg.QuotaEnabled {
		nags = append([]Nag{quotaNag(cfg)}, nags...)
	}
	state, err := readState()
	if err != nil {
		log.Fatalf("nagger: %v — refusing to run with unreadable state (it would re-send every nag)", err)
	}

	var fired []firedBlock
	var firedIDs []string
	for _, n := range nags {
		if !dueNag(n, state.LastFired, todayDate) {
			continue
		}
		if block, ok := n.Render(now); ok {
			fired = append(fired, firedBlock{group: n.Group, block: block})
			firedIDs = append(firedIDs, n.ID)
		}
	}

	if len(fired) == 0 {
		fmt.Println("Nothing due — skipping.")
		if enabled {
			if err := writeState(state); err != nil {
				log.Fatalf("nagger: %v", err)
			}
		}
		return
	}

	msg := buildMessage(fired)

	if !enabled {
		fmt.Println("[dry-run] ENABLE_TELEGRAM != true — would send:")
		fmt.Println(msg)
		return
	}

	if err := writeState(state); err != nil {
		log.Printf("warning: %v", err)
	}
	if err := buildSender().Send(msg); err != nil {
		log.Fatalf("send failed: %v", err)
	}
	for _, id := range firedIDs {
		state.LastFired[id] = today
	}
	if err := writeState(state); err != nil {
		// Exiting non-zero matters: printing Sent after a failed write left the
		// run looking clean and re-sent every nag the next hour.
		log.Fatalf("nagger: %v (message sent but state not saved; the next run may re-send)", err)
	}
	fmt.Printf("Sent: %d nag(s) — %s\n", len(firedIDs), strings.Join(firedIDs, ", "))
}
