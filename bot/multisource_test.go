package bot

import (
	"errors"
	"testing"
)

type stubSource struct {
	items []Item
	err   error
}

func (s stubSource) Fetch() ([]Item, error) { return s.items, s.err }

// Every source failing must surface as an error. Before 2026-10-24 it returned
// an empty slice and nil, so scout logged "[OK] Done!" and exited 0 having
// delivered nothing — a skipped week that read as a quiet one.
func TestMultiSourceErrorsWhenEverySourceFails(t *testing.T) {
	m := &MultiSource{Sources: []NamedSource{
		{Name: "a", Source: stubSource{err: errors.New("no route to host")}},
		{Name: "b", Source: stubSource{err: errors.New("no route to host")}},
	}}

	if _, err := m.Fetch(); err == nil {
		t.Fatal("expected an error when every source failed, got nil")
	}
}

// One working source is enough: a single flaky feed must not sink the digest.
func TestMultiSourceToleratesOneFailure(t *testing.T) {
	m := &MultiSource{Sources: []NamedSource{
		{Name: "a", Source: stubSource{err: errors.New("boom")}},
		{Name: "b", Source: stubSource{items: []Item{{Title: "kept"}}}},
	}}

	items, err := m.Fetch()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 || items[0].Title != "kept" {
		t.Fatalf("expected the surviving item, got %+v", items)
	}
	if items[0].Meta["source"] != "b" {
		t.Fatalf("surviving item not tagged with its source: %+v", items[0].Meta)
	}
}

// A genuinely empty day (no items, no errors) is not a failure.
func TestMultiSourceAllSucceedEmpty(t *testing.T) {
	m := &MultiSource{Sources: []NamedSource{
		{Name: "a", Source: stubSource{}},
	}}

	items, err := m.Fetch()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no items, got %d", len(items))
	}
}
