package sources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/identity"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// A Claude Code session routed through CLIProxyAPI is served by whichever of
// the proxy's accounts it picks, request by request, and the transcript names
// none of them: attributed by who is signed in, every proxied token lands on
// one account. The proxy's usage monitor records the account behind each
// request, so a transcript event is matched to its record instead.
//
// The monitor's ids are the proxy's own, never the transcript's requestId, so
// a match is on content: the same model and the same input, cache-read and
// cache-write counts, which differ between requests by the size of their
// context. Output is left out, because it grows across the streamed lines of
// one response. The record is stamped when the request starts and the
// transcript line when it ends, so the record must precede the event, and of
// several that do, the latest is taken: a session that sends one request
// twice gets each reply's own.

// ProxyLedgerPath is the usage monitor's database, relative to home.
const ProxyLedgerPath = ".cli-proxy-api/usage-monitor/usage.sqlite3"

const (
	// proxyMaxLatency bounds how long before its transcript line a request's
	// record may start. Observed replies took up to about two minutes.
	proxyMaxLatency = 15 * time.Minute
	// proxyClockSlack lets a record start just after the event's own stamp:
	// the two are written by different processes.
	proxyClockSlack = 5 * time.Second
	// ProxyRecheck is how far back each pass looks again for events stored
	// before their record. The proxy holds records an hour before the monitor
	// must have taken them.
	ProxyRecheck = 2 * time.Hour
)

type proxyRecord struct {
	at  time.Time
	ref string // "" when the proxy no longer holds an account by that email
	key proxyKey
}

type proxyKey struct {
	model                        string
	input, cacheRead, cacheWrite int64
}

func proxyKeyOf(e schema.Event) proxyKey {
	u := e.Usage
	return proxyKey{e.Model, u.InputTokens, u.CacheReadTokens, u.CacheWrite5mTokens + u.CacheWrite1hTokens}
}

// ProxyLedger answers which account served a request, from the monitor's
// records, loaded an hour at a time as events ask for them. A nil ledger
// matches nothing, which is what a machine without the proxy has.
type ProxyLedger struct {
	db    *sql.DB
	refs  map[string]string // lowercased email -> account ref
	hours map[int64][]proxyRecord
	err   error
}

// OpenProxyLedger opens the monitor's database, or returns nil when there is
// none.
//
// Not mode=ro: the monitor closes every connection, which removes its -wal and
// -shm, and a read-only connection cannot recreate them, so it fails to open
// more often than not. query_only makes an accidental write fail instead.
func OpenProxyLedger(home string) (*ProxyLedger, error) {
	path := filepath.Join(home, ProxyLedgerPath)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=query_only(true)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, a := range identity.ProxyAccounts(home) {
		refs[strings.ToLower(a.Email)] = a.Ref
	}
	return &ProxyLedger{db: db, refs: refs, hours: map[int64][]proxyRecord{}}, nil
}

// Close releases the database.
func (l *ProxyLedger) Close() error {
	if l == nil {
		return nil
	}
	return l.db.Close()
}

// Err is the first failure to read the monitor. Events it could not answer
// for keep their inferred account, and a later pass tries again.
func (l *ProxyLedger) Err() error {
	if l == nil {
		return nil
	}
	return l.err
}

// Account returns the account that served the request behind e, if the
// monitor recorded it.
func (l *ProxyLedger) Account(ctx context.Context, e schema.Event) (string, bool) {
	if l == nil || e.TS.IsZero() {
		return "", false
	}
	from, to := e.TS.Add(-proxyMaxLatency), e.TS.Add(proxyClockSlack)
	key := proxyKeyOf(e)
	var best *proxyRecord
	for h := from.Unix() / 3600; h <= to.Unix()/3600; h++ {
		for i, r := range l.hour(ctx, h) {
			if r.key != key || r.at.Before(from) || r.at.After(to) {
				continue
			}
			if best == nil || r.at.After(best.at) {
				best = &l.hours[h][i]
			}
		}
	}
	if best == nil || best.ref == "" {
		return "", false
	}
	return best.ref, true
}

// hour loads the records that started in one UTC hour. The monitor stores
// RFC 3339 UTC timestamps, so a string range over them is a time range.
func (l *ProxyLedger) hour(ctx context.Context, h int64) []proxyRecord {
	if rs, ok := l.hours[h]; ok {
		return rs
	}
	const layout = "2006-01-02T15:04:05"
	start := time.Unix(h*3600, 0).UTC()
	rows, err := l.db.QueryContext(ctx, `
		SELECT timestamp, email, COALESCE(model, ''), COALESCE(input_tokens, 0),
		       COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0)
		FROM events
		WHERE COALESCE(failed, 0) = 0 AND timestamp >= ? AND timestamp < ?`,
		start.Format(layout), start.Add(time.Hour).Format(layout))
	if err != nil {
		l.fail(err)
		return nil // not cached: the next event asks again
	}
	defer rows.Close()
	var out []proxyRecord
	for rows.Next() {
		var ts, email string
		var r proxyRecord
		if err := rows.Scan(&ts, &email, &r.key.model, &r.key.input,
			&r.key.cacheRead, &r.key.cacheWrite); err != nil {
			l.fail(err)
			return nil
		}
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			continue
		}
		r.at, r.ref = at, l.refs[strings.ToLower(email)]
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		l.fail(err)
		return nil
	}
	l.hours[h] = out
	return out
}

func (l *ProxyLedger) fail(err error) {
	if l.err == nil {
		l.err = err
	}
}

// attributeProxied stamps e with the account the proxy served it from, when
// the monitor recorded the request.
func (c *Ctx) attributeProxied(ctx context.Context, e *schema.Event) {
	if ref, ok := c.Proxy.Account(ctx, *e); ok {
		e.AccountRef, e.AccountExact = ref, true
		e.CostBasis = schema.CostRateCard
	}
}

// ReattributeProxied gives the proxy's account to recent Claude Code events
// stored before the monitor recorded their request, and requeues them for
// upload. Returns how many rows it moved.
//
// An abandoned attempt (abandonedAttempts) has its own model and counts, so it
// matches no record; it takes its response's account, as it does when read.
func ReattributeProxied(ctx context.Context, c *Ctx, since time.Time) (int, error) {
	if c.Proxy == nil {
		return 0, nil
	}
	rows, err := c.Store.SourceRowsSince(ctx, string(schema.SourceClaudeCode), since)
	if err != nil {
		return 0, err
	}
	events := make([]schema.Event, len(rows))
	byNative := map[string]int{}
	for i, r := range rows {
		raw, _ := r.Payload.(json.RawMessage)
		if json.Unmarshal(raw, &events[i]) == nil {
			byNative[events[i].NativeID] = i
		}
	}
	var moved []store.Record
	stamp := func(i int, ref string) {
		e := &events[i]
		if e.AccountExact {
			return
		}
		e.AccountRef, e.AccountExact, e.CostBasis = ref, true, schema.CostRateCard
		r := rows[i]
		r.Payload = *e
		moved = append(moved, r)
	}
	for i, e := range events {
		if e.AccountExact || e.ID == "" || strings.Contains(e.NativeID, "#") {
			continue
		}
		if ref, ok := c.Proxy.Account(ctx, e); ok {
			stamp(i, ref)
		}
	}
	for i, e := range events {
		parent, _, ok := strings.Cut(e.NativeID, "#")
		if !ok {
			continue
		}
		if p, ok := byNative[parent]; ok && events[p].AccountExact {
			stamp(i, events[p].AccountRef)
		}
	}
	return c.Store.RewritePayloads(ctx, moved)
}
