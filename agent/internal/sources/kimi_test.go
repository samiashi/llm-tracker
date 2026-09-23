package sources

import (
	"testing"
)

func TestKimiIgnoresSessionScopedRecords(t *testing.T) {
	events := collectFile(t, Kimi{}, ".kimi/sessions/s1/run/wire.jsonl",
		`{"type":"usage.record","scope":"session","request_id":"s1","usage":{"inputOther":999,"output":999}}
{"type":"usage.record","scope":"turn","request_id":"t1","usage":{"inputOther":10,"output":5}}
`)
	if len(events) != 1 || events[0].NativeID != "t1" {
		t.Fatalf("got %+v, want only the turn-scoped record", events)
	}
}
