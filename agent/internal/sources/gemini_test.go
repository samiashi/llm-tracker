package sources

import (
	"testing"
)

func TestGeminiKeepsTheReleasedIDForASessionWithoutOne(t *testing.T) {
	events := collectAndCommit(t, Gemini{}, ".gemini/tmp/project-a/session.json",
		`{"model":"gemini-3-pro","session_input_tokens":100,"session_output_tokens":20}`)
	if len(events) != 1 || events[0].NativeID != "session" {
		t.Fatalf("got %+v, want one event with the released id %q", events, "session")
	}
}
