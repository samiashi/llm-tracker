package sources

import (
	"context"
	"os"
	"testing"
	"time"
)

// Re-reading an unchanged request writes nothing, so it is not counted.
func TestClineReportsOnlyRowsItActuallyWrote(t *testing.T) {
	first := `{"type":"say","say":"api_req_started","ts":1790000001000,` +
		`"modelInfo":{"providerId":"anthropic","modelId":"claude-opus-5"},` +
		`"text":"{\"tokensIn\":10,\"tokensOut\":5,\"cost\":0.1}"}`
	second := `{"type":"say","say":"api_req_started","ts":1790000002000,` +
		`"text":"{\"tokensIn\":20,\"tokensOut\":7,\"cost\":0.2}"}`

	c, a := clineCtx(t, "["+first+"]")
	res, err := a.Collect(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 1 {
		t.Fatalf("first pass stored %d, want 1", res.Stored)
	}

	// The task grows; bump its mtime past the watermark.
	path := taskFile(c, a)
	if err := os.WriteFile(path, []byte("["+first+","+second+"]"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}

	res, err = a.Collect(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 1 {
		t.Errorf("second pass reported %d stored, want 1 -- only the new request was written", res.Stored)
	}
}
