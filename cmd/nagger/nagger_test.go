package main

import (
	"os"
	"testing"
	"time"

	"github.com/0x7461/guild/config"
)

func TestStateRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := &naggerState{LastFired: map[string]string{"quota": "2026-10-10"}}
	if err := writeState(st); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	got, err := readState()
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if got.LastFired["quota"] != "2026-10-10" {
		t.Errorf("LastFired[quota] = %q, want 2026-10-10", got.LastFired["quota"])
	}
}

func TestReadStateRejectsTruncated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// A half-written file used to read back as an empty map, which re-fired
	// every nag. It must be an error instead.
	if err := os.WriteFile(statePath(), []byte(`{"lastFired":{"quota":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readState(); err == nil {
		t.Fatal("readState accepted a truncated state.json; want an error")
	}
}

func TestClamp(t *testing.T) {
	tests := []struct {
		name      string
		v, lo, hi int
		want      int
	}{
		{"within", 5, 1, 7, 5},
		{"below", 0, 1, 7, 1},
		{"above", 9, 1, 7, 7},
		{"at low bound", 1, 1, 7, 1},
		{"at high bound", 7, 1, 7, 7},
		{"negative range", -9, -3, 3, -3},
		{"equal bounds", 4, 4, 4, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clamp(tt.v, tt.lo, tt.hi); got != tt.want {
				t.Errorf("clamp(%d, %d, %d) = %d, want %d", tt.v, tt.lo, tt.hi, got, tt.want)
			}
		})
	}
}

// getCycleDay prefers the API's resets_at timestamp: a rolling 7-day window
// where day 1 is just after a reset and day 7 just before the next. Margins of
// half a day keep the expected integer stable against clock drift in the test.
func TestGetCycleDayFromResetsAt(t *testing.T) {
	const day = int64(86400)
	now := time.Now().Unix()

	tests := []struct {
		name     string
		resetsAt int64
		want     int
	}{
		{"day 1 (reset 6.5d away)", now + 6*day + day/2, 1},
		{"day 2", now + 5*day + day/2, 2},
		{"day 3", now + 4*day + day/2, 3},
		{"day 4", now + 3*day + day/2, 4},
		{"day 5", now + 2*day + day/2, 5},
		{"day 6", now + 1*day + day/2, 6},
		{"day 7", now + day/2, 7},
		{"reset already past clamps to 7", now - day, 7},
		{"far-future reset clamps to 1", now + 100*day, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getCycleDay(tt.resetsAt, config.NaggerConfig{}); got != tt.want {
				t.Errorf("getCycleDay(%d, cfg) = %d, want %d", tt.resetsAt, got, tt.want)
			}
		})
	}
}

// With no reset timestamp the configured weekday/hour/timezone anchor is used.
// ResetHour 0 keeps each case deterministic: "now" is always at or after
// midnight of the reference day, so the elapsed-day count is exact.
func TestGetCycleDayFallbackAnchor(t *testing.T) {
	wd := (int(time.Now().UTC().Weekday()) + 6) % 7 // Monday=0, matching the config
	loc := time.FixedZone("local", 7*3600)
	lwd := (int(time.Now().In(loc).Weekday()) + 6) % 7

	tests := []struct {
		name string
		cfg  config.NaggerConfig
		want int
	}{
		{"reset is today", config.NaggerConfig{ResetWeekday: wd, ResetHour: 0, ResetTZOffset: 0}, 1},
		{"reset 3 days ago", config.NaggerConfig{ResetWeekday: (wd + 4) % 7, ResetHour: 0, ResetTZOffset: 0}, 4},
		{"reset 6 days ago is day 7 (weekday wraps)", config.NaggerConfig{ResetWeekday: (wd + 1) % 7, ResetHour: 0, ResetTZOffset: 0}, 7},
		{"timezone offset picks local weekday", config.NaggerConfig{ResetWeekday: lwd, ResetHour: 0, ResetTZOffset: 7}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getCycleDay(0, tt.cfg); got != tt.want {
				t.Errorf("getCycleDay(0, %+v) = %d, want %d", tt.cfg, got, tt.want)
			}
		})
	}
}

func TestDueNag(t *testing.T) {
	today := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		nag   Nag
		state map[string]string
		want  bool
	}{
		{
			name:  "never fired, no anchor: fires now (quota)",
			nag:   Nag{ID: "quota", IntervalDays: 1},
			state: map[string]string{},
			want:  true,
		},
		{
			name:  "never fired, anchor today: due",
			nag:   Nag{ID: "r", IntervalDays: 30, Anchor: "2026-03-10"},
			state: map[string]string{},
			want:  true,
		},
		{
			name:  "never fired, anchor in the past: due late",
			nag:   Nag{ID: "r", IntervalDays: 30, Anchor: "2026-01-01"},
			state: map[string]string{},
			want:  true,
		},
		{
			name:  "never fired, anchor in the future: not due",
			nag:   Nag{ID: "r", IntervalDays: 30, Anchor: "2026-04-01"},
			state: map[string]string{},
			want:  false,
		},
		{
			name:  "never fired, unparseable anchor: do not fire blindly",
			nag:   Nag{ID: "r", IntervalDays: 30, Anchor: "not-a-date"},
			state: map[string]string{},
			want:  false,
		},
		{
			name:  "fired today, interval 1: not due again this cycle",
			nag:   Nag{ID: "quota", IntervalDays: 1},
			state: map[string]string{"quota": "2026-03-10"},
			want:  false,
		},
		{
			name:  "fired yesterday, interval 1: due",
			nag:   Nag{ID: "quota", IntervalDays: 1},
			state: map[string]string{"quota": "2026-03-09"},
			want:  true,
		},
		{
			name:  "fired 2 days ago, interval 3: not yet",
			nag:   Nag{ID: "r", IntervalDays: 3},
			state: map[string]string{"r": "2026-03-08"},
			want:  false,
		},
		{
			name:  "fired 3 days ago, interval 3: due",
			nag:   Nag{ID: "r", IntervalDays: 3},
			state: map[string]string{"r": "2026-03-07"},
			want:  true,
		},
		{
			name:  "fired late, interval 7: not twice in a cycle",
			nag:   Nag{ID: "r", IntervalDays: 7},
			state: map[string]string{"r": "2026-03-10"},
			want:  false,
		},
		{
			name:  "unparseable last-fired falls back to anchor (future)",
			nag:   Nag{ID: "r", IntervalDays: 30, Anchor: "2026-04-01"},
			state: map[string]string{"r": "garbage"},
			want:  false,
		},
		{
			name:  "unparseable last-fired falls back to anchor (past)",
			nag:   Nag{ID: "r", IntervalDays: 30, Anchor: "2026-01-01"},
			state: map[string]string{"r": "garbage"},
			want:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dueNag(tt.nag, tt.state, today); got != tt.want {
				t.Errorf("dueNag(%+v, %v, %s) = %v, want %v", tt.nag, tt.state, today.Format("2006-01-02"), got, tt.want)
			}
		})
	}
}

// A reminder is due from its anchor onward and then once per EveryDays; a
// recorded fire suppresses re-firing inside the same cycle.
func TestReminderCadence(t *testing.T) {
	cfg := config.NaggerConfig{Reminders: []config.Reminder{{
		ID: "archive", Message: "Archive chores", EveryDays: 90, Anchor: "2026-01-05",
	}}}
	nags := reminderNags(cfg)
	if len(nags) != 1 {
		t.Fatalf("reminderNags returned %d nags, want 1", len(nags))
	}
	n := nags[0]

	if n.Anchor == "" {
		t.Fatal("reminder nag lost its anchor")
	}
	due := func(date string, state map[string]string) bool {
		d, err := time.Parse("2006-01-02", date)
		if err != nil {
			t.Fatalf("bad test date %q: %v", date, err)
		}
		return dueNag(n, state, d)
	}

	never := map[string]string{}
	if !due("2026-01-05", never) {
		t.Error("reminder not due on its anchor")
	}
	if !due("2026-01-07", never) {
		t.Error("missed reminder should fire late, within the cycle")
	}
	if due("2026-01-04", never) {
		t.Error("reminder fired before its anchor")
	}

	fired := map[string]string{"archive": "2026-01-05"}
	if due("2026-01-10", fired) {
		t.Error("reminder fired twice in one cycle")
	}
	if due("2026-04-04", fired) {
		t.Error("reminder fired one day short of its interval")
	}
	if !due("2026-04-05", fired) {
		t.Error("reminder not due exactly 90 days after its last fire")
	}
}

// Invalid reminder entries never become nags: an ID, a positive interval and an
// explicit anchor are all required (fire-now is quota-only).
func TestReminderNagsSkipsInvalid(t *testing.T) {
	cfg := config.NaggerConfig{Reminders: []config.Reminder{
		{ID: "ok", Message: "ok", EveryDays: 30, Anchor: "2026-01-01"},
		{ID: "", Message: "no id", EveryDays: 30, Anchor: "2026-01-01"},
		{ID: "no-interval", Message: "x", EveryDays: 0, Anchor: "2026-01-01"},
		{ID: "no-anchor", Message: "x", EveryDays: 30, Anchor: ""},
	}}
	nags := reminderNags(cfg)
	if len(nags) != 1 || nags[0].ID != "ok" {
		t.Fatalf("reminderNags = %v, want just the valid 'ok' nag", nags)
	}
}
