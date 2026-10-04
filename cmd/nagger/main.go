// nagger — daily Telegram nudges from a unified set of periodic "nags": the
// Claude weekly-quota pace check, the monthly DeepSeek spend budget, and
// recurring manual-task reminders. Each nag is a periodic signal (interval +
// anchor + renderer); one snooze run evaluates them all, fires the due ones as a
// single combined message (grouped under headers), and dedups per-nag via
// ~/.local/share/nagger/state.json. Run hourly 08–22 by runit + snooze; the
// per-nag dedup makes re-runs idempotent.
//
// Inputs:
//   - ~/.local/share/nagger/rate-limits.json  (written by ~/.claude/statusline.sh
//     after every CC response — external producer, do not move)
//   - ~/.config/guild/nagger.json            (quota reset anchor, spend budget, reminders)
//   - ~/.config/deepseek/key_void             (DeepSeek API key, for the balance)
//   - ~/.local/share/nagger/state.json        (per-nag last-fired dates + the spend
//     ledger, owned here; migrated from the flat id→date map and, before that,
//     the legacy last-sent + reminders-state.json files)
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
// (the quota case). IntervalDays 0 = evaluated every run; Render does its own
// dedup (the spend case). Nags sharing a Group render under one header in the combined
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
var groupOrder = []string{"reminder", "quota", "spend"}

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

// naggerState is state.json: per-nag last-fired dates plus the spend nag's
// month ledger. The spend nag dedups on Spend.Fired (once per threshold per
// month), not on LastFired — a date key would hide a second crossing the same day.
type naggerState struct {
	LastFired map[string]string `json:"lastFired"`
	Spend     *spendLedger      `json:"spend,omitempty"`
}

// readState loads state.json. A flat id→date map (the format until 2026-09-29)
// becomes LastFired. If state.json doesn't exist yet, it migrates once from the
// pre-unification files (last-sent → "quota"; the reminders-state.json map merged in).
func readState() *naggerState {
	st := &naggerState{}
	if data, err := os.ReadFile(statePath()); err == nil {
		var probe map[string]json.RawMessage
		if json.Unmarshal(data, &probe) == nil {
			if _, nested := probe["lastFired"]; nested {
				_ = json.Unmarshal(data, st)
			} else {
				_ = json.Unmarshal(data, &st.LastFired)
			}
		}
		if st.LastFired == nil {
			st.LastFired = map[string]string{}
		}
		return st
	}
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
	return st
}

func writeState(m *naggerState) {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		log.Printf("warning: mkdir state dir: %v", err)
		return
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		log.Printf("warning: marshal state: %v", err)
		return
	}
	if err := os.WriteFile(statePath(), append(data, '\n'), 0o644); err != nil {
		log.Printf("warning: write state: %v", err)
	}
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

// --- DeepSeek monthly spend budget ---
//
// DeepSeek exposes a balance, not spend, so spend is derived from a month
// ledger of balance readings. The trap is top-ups: a mid-month top-up raises the
// balance and would read as negative spend, resetting the budget — so a rise is
// recorded as a top-up instead. The ledger measures the whole account, not this
// machine: if the key is ever used elsewhere, that spend counts too.

const deepseekBalanceURL = "https://api.deepseek.com/user/balance"

type spendLedger struct {
	Month        string    `json:"month"`         // YYYY-MM, local time
	StartBalance float64   `json:"start_balance"` // first reading of the month
	Topups       float64   `json:"topups"`        // balance rises seen this month
	LastBalance  float64   `json:"last_balance"`
	Fired        []float64 `json:"fired"` // warn fractions already sent this month
}

func deepseekBalance() (float64, error) {
	home, _ := os.UserHomeDir()
	key, err := os.ReadFile(filepath.Join(home, ".config", "deepseek", "key_void"))
	if err != nil {
		return 0, fmt.Errorf("read API key: %w", err)
	}
	req, err := http.NewRequest(http.MethodGet, deepseekBalanceURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("balance API returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		BalanceInfos []struct {
			Currency     string `json:"currency"`
			TotalBalance string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode balance: %w", err)
	}
	for _, b := range body.BalanceInfos {
		if b.Currency == "USD" {
			return strconv.ParseFloat(b.TotalBalance, 64)
		}
	}
	return 0, fmt.Errorf("no USD balance in the response")
}

// advanceLedger folds one balance reading into the month ledger and returns it
// with the spend so far this month. A new month starts over from this reading.
func advanceLedger(prev *spendLedger, balance float64, month string) (spendLedger, float64) {
	if prev == nil || prev.Month != month {
		return spendLedger{Month: month, StartBalance: balance, LastBalance: balance}, 0
	}
	l := *prev
	l.Fired = slices.Clone(prev.Fired)
	if balance > l.LastBalance {
		l.Topups += balance - l.LastBalance
	}
	l.LastBalance = balance
	return l, l.StartBalance + l.Topups - balance
}

// newlyCrossed returns, ascending, the warn fractions spend has reached that
// haven't fired this month.
func newlyCrossed(spend, quota float64, fractions, fired []float64) []float64 {
	var out []float64
	for _, f := range fractions {
		if spend >= f*quota && !slices.Contains(fired, f) {
			out = append(out, f)
		}
	}
	sort.Float64s(out)
	return out
}

// spendNags reads the balance once per run and returns the "spend" nag (fires
// once per newly crossed threshold; a jump across several fires once, naming the
// highest) or, when the balance can't be read, "spend-error" (at most daily).
// It advances st.Spend in place. commit marks the crossed thresholds fired —
// run it only after a successful send, so a failed send retries next run.
func spendNags(cfg config.NaggerConfig, st *naggerState, now time.Time) (nags []Nag, commit func()) {
	commit = func() {}
	quota := cfg.SpendQuotaUSD
	if quota <= 0 {
		return nil, commit
	}
	balance, err := deepseekBalance()
	if err != nil {
		msg := fmt.Sprintf("DeepSeek spend check failed: %v. Not urgent, but the budget warning is off until it's fixed.", err)
		return []Nag{{ID: "spend-error", Group: "spend", IntervalDays: 1,
			Render: func(time.Time) (string, bool) { return msg, true }}}, commit
	}
	ledger, spend := advanceLedger(st.Spend, balance, now.Format("2006-01"))
	st.Spend = &ledger

	fractions := cfg.WarnFractions
	if len(fractions) == 0 {
		fractions = []float64{0.5, 0.8, 0.9}
	}
	crossed := newlyCrossed(spend, quota, fractions, ledger.Fired)
	if len(crossed) == 0 {
		return nil, commit
	}
	top := crossed[len(crossed)-1]
	urgency := "Not urgent — a heads-up on pace."
	if top >= 1 {
		urgency = "Budget reached — decide whether to keep spending this month (the prepaid balance is the hard stop)."
	}
	msg := fmt.Sprintf("DeepSeek spend this month: $%.2f of the $%g budget (%.0f%%), past the %.0f%% mark. Balance $%.2f. %s",
		spend, quota, spend/quota*100, top*100, balance, urgency)
	commit = func() { st.Spend.Fired = append(st.Spend.Fired, crossed...) }
	return []Nag{{ID: "spend", Group: "spend", IntervalDays: 0,
		Render: func(time.Time) (string, bool) { return msg, true }}}, commit
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
		log.Printf("warning: could not load nagger config: %v", err)
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
	state := readState()
	spend, commitSpend := spendNags(cfg, state, now)
	nags = append(nags, spend...)

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
			writeState(state) // keep this run's balance reading in the spend ledger
		}
		return
	}

	msg := buildMessage(fired)

	if !enabled {
		fmt.Println("[dry-run] ENABLE_TELEGRAM != true — would send:")
		fmt.Println(msg)
		return
	}

	writeState(state) // the balance reading counts even if the send fails
	if err := buildSender().Send(msg); err != nil {
		log.Fatalf("send failed: %v", err)
	}
	for _, id := range firedIDs {
		state.LastFired[id] = today
	}
	commitSpend()
	writeState(state)
	fmt.Printf("Sent: %d nag(s) — %s\n", len(firedIDs), strings.Join(firedIDs, ", "))
}
