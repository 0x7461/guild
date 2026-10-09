package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/0x7461/guild/config"
)

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
			name:  "interval 0 (spend): evaluated every run",
			nag:   Nag{ID: "spend", IntervalDays: 0},
			state: map[string]string{"spend": "2026-03-10"},
			want:  true,
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

func TestAdvanceLedgerFirstRun(t *testing.T) {
	l, spend := advanceLedger(nil, 12.5, "2026-03")
	want := spendLedger{Month: "2026-03", StartBalance: 12.5, LastBalance: 12.5}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("advanceLedger(nil, 12.5, 2026-03) = %+v, want %+v", l, want)
	}
	if spend != 0 {
		t.Errorf("first-run spend = %v, want 0", spend)
	}
}

func TestAdvanceLedgerNewMonthRestarts(t *testing.T) {
	prev := &spendLedger{
		Month: "2026-03", StartBalance: 10, Topups: 5, LastBalance: 8,
		Fired: []float64{0.5, 0.8},
	}
	l, spend := advanceLedger(prev, 20, "2026-04")

	want := spendLedger{Month: "2026-04", StartBalance: 20, LastBalance: 20}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("new-month ledger = %+v, want %+v (ledger must restart)", l, want)
	}
	if spend != 0 {
		t.Errorf("new-month spend = %v, want 0", spend)
	}
	if len(prev.Fired) != 2 || prev.Topups != 5 || prev.StartBalance != 10 {
		t.Errorf("advanceLedger mutated the previous month's ledger: %+v", prev)
	}
}

func TestAdvanceLedgerSpend(t *testing.T) {
	month := "2026-03"
	prev := &spendLedger{Month: month, StartBalance: 10, LastBalance: 10}

	l, spend := advanceLedger(prev, 7.5, month)
	if l.Topups != 0 {
		t.Errorf("topups on a decrease = %v, want 0", l.Topups)
	}
	if l.LastBalance != 7.5 {
		t.Errorf("last balance = %v, want 7.5", l.LastBalance)
	}
	if spend != 2.5 {
		t.Errorf("spend after a decrease = %v, want 2.5", spend)
	}
}

// A balance rise is a top-up, never negative spend. A rise to the starting
// balance yields exactly zero spend, not a negative value.
func TestAdvanceLedgerRiseIsTopup(t *testing.T) {
	month := "2026-03"
	prev := &spendLedger{Month: month, StartBalance: 10, LastBalance: 10}

	l, spend := advanceLedger(prev, 12, month)
	if l.Topups != 2 {
		t.Errorf("topups after a rise = %v, want 2", l.Topups)
	}
	if l.LastBalance != 12 {
		t.Errorf("last balance = %v, want 12", l.LastBalance)
	}
	if spend != 0 {
		t.Errorf("spend after a rise to start+topup = %v, want 0 (never negative)", spend)
	}

	// A later decrease then measures real spend against the topped-up budget.
	l2, spend2 := advanceLedger(&l, 9, month)
	if l2.Topups != 2 {
		t.Errorf("topups after a subsequent decrease = %v, want 2", l2.Topups)
	}
	if spend2 != 3 {
		t.Errorf("spend after a decrease = %v, want 3", spend2)
	}
}

// A multi-reading sequence: spend accumulates through a mid-month top-up.
func TestAdvanceLedgerSequence(t *testing.T) {
	month := "2026-03"
	readings := []float64{20, 15, 12, 25, 20}
	wantSpend := []float64{0, 5, 8, 8, 13}

	var l *spendLedger
	for i, b := range readings {
		next, spend := advanceLedger(l, b, month)
		if spend != wantSpend[i] {
			t.Errorf("reading %d (balance %v): spend = %v, want %v", i, b, spend, wantSpend[i])
		}
		l = &next
	}
	if l.Topups != 13 {
		t.Errorf("total top-ups = %v, want 13", l.Topups)
	}
}

// Cloning Fired matters: the send path appends only on success, so a retry must
// not mutate the previous ledger.
func TestAdvanceLedgerClonesFired(t *testing.T) {
	prev := &spendLedger{Month: "2026-03", StartBalance: 10, LastBalance: 10, Fired: []float64{0.5}}
	l, _ := advanceLedger(prev, 9, "2026-03")
	l.Fired = append(l.Fired, 0.8)

	if len(prev.Fired) != 1 || prev.Fired[0] != 0.5 {
		t.Errorf("advanceLedger aliased Fired: prev = %v", prev.Fired)
	}
	if len(l.Fired) != 2 {
		t.Errorf("returned ledger Fired = %v, want two entries", l.Fired)
	}
}

func TestNewlyCrossed(t *testing.T) {
	fractions := []float64{0.5, 0.8, 0.9}

	tests := []struct {
		name  string
		spend float64
		quota float64
		fired []float64
		want  []float64
	}{
		{"no spend", 0, 100, nil, nil},
		{"below all", 20, 100, nil, nil},
		{"exactly at 50% fires", 50, 100, nil, []float64{0.5}},
		{"just past 50%", 50.01, 100, nil, []float64{0.5}},
		{"crosses two at once", 85, 100, nil, []float64{0.5, 0.8}},
		{"over budget crosses all", 150, 100, nil, []float64{0.5, 0.8, 0.9}},
		{"already fired 50, now 85", 85, 100, []float64{0.5}, []float64{0.8}},
		{"all fired, nothing new", 95, 100, []float64{0.5, 0.8, 0.9}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newlyCrossed(tt.spend, tt.quota, fractions, tt.fired)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("newlyCrossed(%v, %v, %v, %v) = %v, want %v",
					tt.spend, tt.quota, fractions, tt.fired, got, tt.want)
			}
		})
	}
}

// Unsorted config fractions still come back ascending, and each fraction fires
// at most once per month (it is filtered against fired).
func TestNewlyCrossedUnsortedAndOncePerMonth(t *testing.T) {
	got := newlyCrossed(95, 100, []float64{0.9, 0.5, 0.8}, nil)
	want := []float64{0.5, 0.8, 0.9}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unsorted fractions = %v, want ascending %v", got, want)
	}

	got = newlyCrossed(95, 100, []float64{0.9, 0.5, 0.8}, []float64{0.5, 0.9})
	want = []float64{0.8}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("already-fired fractions = %v, want %v", got, want)
	}
}
