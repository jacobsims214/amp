package domain

import (
	"encoding/json"
	"testing"
)

// AMP #3009: merge_for says which ticket's PR merge a person's step tracks.
// Every representation a write path can be handed must read the same way, and
// a value nothing can read must be refused there rather than silently stored
// as a link that does nothing (the reason the key exists is to remove a
// duplicate card; a link that doesn't read is a duplicate nobody can see).
func TestParseMergeFor(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int
		ok   bool
	}{
		{"absent", nil, 0, true},
		{"number", float64(2302), 2302, true},
		{"whole float", float64(7), 7, true},
		{"int", 42, 42, true},
		{"int64", int64(43), 43, true},
		{"int32", int32(44), 44, true},
		{"float32", float32(45), 45, true},
		{"json.Number", json.Number("46"), 46, true},
		{"digit string", "2304", 2304, true},
		{"hashed string", "#2304", 2304, true},
		{"spaced string", "  2304  ", 2304, true},
		{"empty string is no link", "", 0, true},

		{"negative", float64(-1), 0, false},
		{"zero", float64(0), 0, false},
		{"fraction", 2.5, 0, false},
		{"negative string", "-3", 0, false},
		{"words", "the PR", 0, false},
		{"boolean", true, 0, false},
		{"object", map[string]any{"id": 1}, 0, false},
		{"array", []any{1}, 0, false},
		{"too large", float64(1 << 40), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMergeFor(tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("ParseMergeFor(%#v) error = %v, want ok=%v", tt.in, err, tt.ok)
			}
			if tt.ok && got != tt.want {
				t.Fatalf("ParseMergeFor(%#v) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// Metadata.MergeFor is the read: a link or not, never an error. A value that
// can't be read is no link, so a ticket carrying junk behaves like one with
// no link at all (it is refused on write; this is the read after a hand edit).
func TestMetadataMergeFor(t *testing.T) {
	tests := []struct {
		meta Metadata
		id   int
		ok   bool
	}{
		{Metadata{}, 0, false},
		{Metadata{MetaMergeFor: nil}, 0, false},
		{Metadata{MetaMergeFor: float64(2302)}, 2302, true},
		{Metadata{MetaMergeFor: "2304"}, 2304, true},
		{Metadata{MetaMergeFor: "#2304"}, 2304, true},
		{Metadata{MetaMergeFor: "2304 "}, 2304, true},
		{Metadata{MetaMergeFor: "nope"}, 0, false},
		{Metadata{MetaMergeFor: false}, 0, false},
		{Metadata{MetaMergeFor: 0}, 0, false},
	}
	for _, tt := range tests {
		if id, ok := tt.meta.MergeFor(); id != tt.id || ok != tt.ok {
			t.Fatalf("%v.MergeFor() = %d,%v, want %d,%v", tt.meta, id, ok, tt.id, tt.ok)
		}
	}
	// It is its own key, not a merge-gate key: editing it doesn't owe the
	// project-wide integration/waits_for_merge sweep (no gates to reconcile).
	if (MetadataPatch{MetaMergeFor: 2302}).TouchesMergeGate() {
		t.Fatal("merge_for must not trigger the integration-branch sweep")
	}
}

// The strict parse and the read must agree on everything a link can be: a
// value the read calls a link is one the parse accepts, and vice versa.
func TestMergeForReadAndParseAgree(t *testing.T) {
	values := []any{nil, float64(1), "2", "#3", "", "x", true, float64(-1), 2.5, int64(9)}
	for _, v := range values {
		id, err := ParseMergeFor(v)
		byRead, readOK := (Metadata{MetaMergeFor: v}).MergeFor()
		byParse, parseOK := id, err == nil && id > 0
		if readOK != parseOK || byRead != byParse {
			t.Fatalf("%#v: read = %d,%v; parse = %d,%v", v, byRead, readOK, byParse, parseOK)
		}
	}
}
