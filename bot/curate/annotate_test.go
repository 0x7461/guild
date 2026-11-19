package curate

import (
	"testing"
)

func idx(i int) *int { return &i }

func TestPairAnnotations(t *testing.T) {
	tests := []struct {
		name string
		anns []Annotation
		n    int
		ok   bool
	}{
		{"covers exactly, in order", []Annotation{{Index: idx(0)}, {Index: idx(1)}, {Index: idx(2)}}, 3, true},
		{"covers exactly, shuffled", []Annotation{{Index: idx(2)}, {Index: idx(0)}, {Index: idx(1)}}, 3, true},
		{"short tail", []Annotation{{Index: idx(0)}, {Index: idx(1)}}, 3, false},
		{"too many", []Annotation{{Index: idx(0)}, {Index: idx(1)}, {Index: idx(2)}, {Index: idx(3)}}, 3, false},
		{"duplicate index", []Annotation{{Index: idx(0)}, {Index: idx(1)}, {Index: idx(1)}}, 3, false},
		{"out of range", []Annotation{{Index: idx(0)}, {Index: idx(1)}, {Index: idx(3)}}, 3, false},
		{"nil index", []Annotation{{Index: nil}, {Index: idx(1)}, {Index: idx(2)}}, 3, false},
		{"zero items, zero annotations", nil, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pairs, ok := pairAnnotations(tt.anns, tt.n)
			if ok != tt.ok {
				t.Fatalf("pairAnnotations() ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			for i, p := range pairs {
				if p.Index == nil || *p.Index != i {
					t.Fatalf("pairs[%d].Index = %v, want %d", i, p.Index, i)
				}
			}
		})
	}
}

func TestParseAnnotationsIndex(t *testing.T) {
	anns, err := parseAnnotations(`[{"index": 0, "summary": "a"}, {"index": 1, "sentiment": "civil"}]`)
	if err != nil {
		t.Fatalf("parseAnnotations: %v", err)
	}
	if len(anns) != 2 {
		t.Fatalf("len = %d, want 2", len(anns))
	}
	if anns[0].Index == nil || *anns[0].Index != 0 || anns[0].Summary != "a" {
		t.Fatalf("anns[0] = %+v, want index 0 summary a", anns[0])
	}
	if anns[1].Index == nil || *anns[1].Index != 1 || anns[1].Sentiment != "civil" {
		t.Fatalf("anns[1] = %+v, want index 1 sentiment civil", anns[1])
	}
}
