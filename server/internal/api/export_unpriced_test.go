package api

import (
	"slices"
	"testing"
)

// Appended, so a sheet that reads columns by position still lines up.
func TestTheCSVMarksUnpricedTokens(t *testing.T) {
	s := newServer(t)
	seed(t, s,
		event("a", "not-a-real-model", "anthropic:a", 5_000),
		event("b", "claude-opus-5", "anthropic:a", 100),
	)

	_, rows := getCSV(t, s, "/v1/export.csv")
	header := rows[0]
	existing := []string{"day", "person", "source", "model", "effort", "origin",
		"tokens", "input_tokens", "output_tokens", "cache_read_tokens",
		"cost_usd", "cost_basis", "responses"}
	if !slices.Equal(header[:min(len(header), len(existing))], existing) {
		t.Fatalf("header %v moved an existing column; want it to start %v", header, existing)
	}
	col := slices.Index(header, "unpriced_tokens")
	if col < 0 {
		t.Fatalf("header %v has no unpriced_tokens", header)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows (incl. header), want 3", len(rows))
	}
	for _, r := range rows[1:] {
		want := "0"
		if r[3] == "not-a-real-model" {
			want = r[6]
		}
		if r[col] != want {
			t.Fatalf("%s: unpriced_tokens = %q, want %q", r[3], r[col], want)
		}
	}
}
