package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// put stores records the way a pass does, through CommitFile, with no cursor.
func put(t *testing.T, s *Store, recs ...Record) {
	t.Helper()
	if _, err := s.CommitFile(context.Background(), "", 0, 0, recs, nil); err != nil {
		t.Fatal(err)
	}
}

// row is one stored row as the upload path sees it.
type row struct {
	Payload   string
	Total     int64
	Collector int
	Sent      int
}

func readRow(t *testing.T, s *Store, table, id string) row {
	t.Helper()
	var r row
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT payload, total_tokens, collector, sent FROM `+table+` WHERE id = ?`, id).
		Scan(&r.Payload, &r.Total, &r.Collector, &r.Sent); err != nil {
		t.Fatal(err)
	}
	return r
}

// Insert-or-ignore would keep a streamed response's first, smallest line.
func TestStreamedResponseKeepsLargestReading(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()

	for _, tok := range []int64{100, 400, 900} {
		put(t, s, Record{ID: "req-1", TS: now, TotalTokens: tok, Payload: map[string]any{"total": tok}})
	}

	events, _, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("got %d rows, want 1 -- streamed records must collapse", events)
	}

	_, payloads, err := s.Unsent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(payloads[0]); got != `{"total":900}` {
		t.Fatalf("kept %s, want the largest reading", got)
	}
}

// A smaller re-read must not overwrite a complete reading, and must not
// re-queue the row for upload.
func TestSmallerRereadIsIgnored(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()

	put(t, s, Record{ID: "r", TS: now, TotalTokens: 900, Payload: 900})
	if err := s.MarkSent(ctx, []string{"r"}); err != nil {
		t.Fatal(err)
	}
	put(t, s, Record{ID: "r", TS: now, TotalTokens: 100, Payload: 100})

	_, unsent, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unsent != 0 {
		t.Fatal("a smaller re-read must not re-queue an already-uploaded row")
	}
}

// A larger reading arriving after upload must be re-queued, or the server
// keeps the partial figure forever.
func TestLargerRereadRequeues(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()

	put(t, s, Record{ID: "r", TS: now, TotalTokens: 100, Payload: 100})
	_ = s.MarkSent(ctx, []string{"r"})
	put(t, s, Record{ID: "r", TS: now, TotalTokens: 900, Payload: 900})

	_, unsent, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unsent != 1 {
		t.Fatalf("got %d unsent, want 1 -- an upgraded reading must re-upload", unsent)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	s, ctx := open(t), context.Background()
	if off, size, _ := s.Cursor(ctx, "/nope"); off != 0 || size != 0 {
		t.Fatal("unknown path should start at zero")
	}
	if _, err := s.CommitFile(ctx, "/a", 120, 500, nil, nil); err != nil {
		t.Fatal(err)
	}
	off, size, err := s.Cursor(ctx, "/a")
	if err != nil || off != 120 || size != 500 {
		t.Fatalf("got (%d,%d,%v), want (120,500,nil)", off, size, err)
	}
}

// Stamped with the newer version, the old payload would tie with that
// collector's own final reading, which then never lands.
func TestANewerCollectorsSmallerReadingChangesNothing(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()

	put(t, s, Record{ID: "r", TS: now, TotalTokens: 900, Collector: 1,
		Payload: map[string]any{"v": 1, "total": 900}})
	if err := s.MarkSent(ctx, []string{"r"}); err != nil {
		t.Fatal(err)
	}
	put(t, s, Record{ID: "r", TS: now, TotalTokens: 100, Collector: 2,
		Payload: map[string]any{"v": 2, "total": 100}})

	got := readRow(t, s, "event", "r")
	want := row{Payload: `{"total":900,"v":1}`, Total: 900, Collector: 1, Sent: 1}
	if got != want {
		t.Fatalf("row = %+v, want it untouched: %+v", got, want)
	}
}

// The re-read reaches the final reading through smaller partial ones, none of
// which may stop it landing.
func TestABackfillUpgradesAStreamedRowToItsFinalReading(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()

	put(t, s, Record{ID: "req", TS: now, TotalTokens: 900, Collector: 7,
		Payload: map[string]any{"collector": 7, "total": 900}})
	if err := s.MarkSent(ctx, []string{"req"}); err != nil {
		t.Fatal(err)
	}

	// In file order, in one commit, as the walk does it.
	put(t, s,
		Record{ID: "req", TS: now, TotalTokens: 100, Collector: 8,
			Payload: map[string]any{"collector": 8, "total": 100}},
		Record{ID: "req", TS: now, TotalTokens: 900, Collector: 8,
			Payload: map[string]any{"collector": 8, "total": 900}},
	)

	got := readRow(t, s, "event", "req")
	want := row{Payload: `{"collector":8,"total":900}`, Total: 900, Collector: 8, Sent: 0}
	if got != want {
		t.Fatalf("row = %+v, want the new collector's final reading, re-queued: %+v", got, want)
	}
}

// Stamped with the highest collector that ever touched it, the row would
// refuse the newer build's re-read of the same total.
func TestARowGrownDuringARollbackIsUpgradedWhenRolledForward(t *testing.T) {
	s := open(t)
	now := time.Now()
	reading := func(tok int64, collector int) Record {
		return Record{ID: "sess", TS: now, TotalTokens: tok, Collector: collector,
			Payload: map[string]any{"by": collector, "total": tok}}
	}

	put(t, s, reading(900, 9))  // the newer build
	put(t, s, reading(1000, 8)) // rolled back; the response grew meanwhile
	if got := readRow(t, s, "event", "sess"); got.Collector != 8 {
		t.Fatalf("collector = %d after the older build's reading replaced the payload, want 8", got.Collector)
	}
	put(t, s, reading(1000, 9)) // rolled forward; the backfill re-reads it

	got := readRow(t, s, "event", "sess")
	if got.Payload != `{"by":9,"total":1000}` || got.Collector != 9 {
		t.Fatalf("row = %+v, want the newer build's reading of the same total", got)
	}
}

// The guards above must not freeze a row at its first value.
func TestLongerReadingReplacesThePayload(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()

	put(t, s, Record{ID: "r", TS: now, TotalTokens: 100, Collector: 1,
		Payload: map[string]any{"total": 100}})
	put(t, s, Record{ID: "r", TS: now, TotalTokens: 900, Collector: 1,
		Payload: map[string]any{"total": 900}})

	_, payloads, err := s.Unsent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(payloads[0]); got != `{"total":900}` {
		t.Fatalf("payload = %s, want the longer reading's", got)
	}
}

// What `rewind` does after a CollectorVersion bump, to every streamed row.
func TestReplayingAStreamedResponseKeepsTheLargestReading(t *testing.T) {
	for _, name := range []string{"ascending", "descending"} {
		t.Run(name, func(t *testing.T) {
			s, ctx := open(t), context.Background()
			now := time.Now()

			readings := []int64{54983, 54983, 54983, 57448}
			if name == "descending" {
				readings = []int64{57448, 54983, 54983, 54983}
			}
			for i, tok := range readings {
				// Collector 2 on the first pass, 3 on the replay.
				collector := 2
				if i > 0 {
					collector = 3
				}
				put(t, s, Record{ID: "req", TS: now, TotalTokens: tok, Collector: collector,
					Payload: map[string]any{"total": tok}})
			}

			_, payloads, err := s.Unsent(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(payloads[0]); got != `{"total":57448}` {
				t.Fatalf("payload = %s, want the largest reading", got)
			}
			var tok int64
			if err := s.db.QueryRowContext(ctx,
				`SELECT total_tokens FROM event WHERE id='req'`).Scan(&tok); err != nil {
				t.Fatal(err)
			}
			if tok != 57448 {
				t.Fatalf("total_tokens = %d, want 57448", tok)
			}
		})
	}
}

func TestOlderCollectorIsIgnored(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()

	put(t, s, Record{ID: "r", TS: now, TotalTokens: 100, Collector: 2, Payload: "new"})
	_ = s.MarkSent(ctx, []string{"r"})
	put(t, s, Record{ID: "r", TS: now, TotalTokens: 100, Collector: 1, Payload: "old"})

	_, unsent, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unsent != 0 {
		t.Fatal("an older collector must not re-queue the row")
	}
}

// A pruned server refuses everything below its retention floor; see Refused
// for why those rows can be neither sent nor left queued.
func TestRefusedRowsLeaveTheQueueWithoutBeingCalledSent(t *testing.T) {
	s, ctx := open(t), context.Background()
	floor := time.Now().AddDate(0, 0, -30)

	rec := func(id string, at time.Time) Record {
		return Record{ID: id, TS: at, TotalTokens: 10, Payload: []byte(`{"id":"` + id + `"}`)}
	}
	put(t, s,
		rec("ancient", floor.AddDate(0, 0, -60)),
		rec("old", floor.AddDate(0, 0, -1)),
		rec("fresh", time.Now()),
	)

	n, err := s.Refused(ctx, floor)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("retired %d rows, want the 2 below the floor", n)
	}

	// The fresh row is reachable, not stuck behind the refused ones.
	ids, _, err := s.Unsent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "fresh" {
		t.Fatalf("queue = %v, want only the row the server will accept", ids)
	}

	// Kept and counted, not deleted and not claimed as delivered.
	refused, err := s.CountRefused(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if refused != 2 {
		t.Fatalf("CountRefused = %d, want 2 still in the archive", refused)
	}
	var sent int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event WHERE sent = 1`).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("%d rows marked sent; nothing was delivered", sent)
	}
}

// A scope is a literal prefix: not a substring of the path, and `_` is not a
// wildcard.
func TestResetClearsExactlyItsScope(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, p := range []string{
		"/Users/dev/.kimi/sessions/a/wire.jsonl",
		"/Users/dev/.claude/projects/-Users-dev-src-kimi-bench/s.jsonl",
	} {
		if _, err := s.CommitFile(ctx, p, 1, 1, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"roo_code:task:1", "rooXcode:task:1"} {
		if err := s.SetMeta(ctx, k, "1"); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.ResetCursors(ctx, Scope{
		CursorPrefixes: []string{"/Users/dev/.kimi/sessions/"},
		MetaPrefixes:   []string{"roo_code:"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cleared %d cursors, want only the one under the prefix", n)
	}
	if off, _, _ := s.Cursor(ctx, "/Users/dev/.claude/projects/-Users-dev-src-kimi-bench/s.jsonl"); off != 1 {
		t.Error("a path merely containing the source's name was rewound")
	}
	if v, _ := s.Meta(ctx, "rooXcode:task:1"); v != "1" {
		t.Error("`_` in the meta prefix matched as a wildcard")
	}
	if v, _ := s.Meta(ctx, "roo_code:task:1"); v != "" {
		t.Error("the scope's own meta key survived")
	}
}

// The primary key already indexes those columns; the copy only doubles every
// insert.
func TestAnOlderStoreLosesTheDuplicateAccountWindowIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(context.Background(),
		`CREATE INDEX IF NOT EXISTS idx_account_window ON account_window(provider, observed_at)`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_account_window'`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("idx_account_window survived reopening")
	}
}

// Nothing reads or sends plan-quota readings, so an older store's table of
// them must not linger in the archive.
func TestOpeningAnOlderStoreDropsItsQuotaTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(context.Background(), `CREATE TABLE quota (
		id TEXT PRIMARY KEY, ts INTEGER NOT NULL, total_tokens INTEGER NOT NULL DEFAULT 0,
		collector INTEGER NOT NULL DEFAULT 0, payload TEXT NOT NULL, sent INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'quota'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the quota table survived reopening")
	}
}
