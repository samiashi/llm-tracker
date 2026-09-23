package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/schema"
	"github.com/samiashi/llm-tracker/server/internal/db"
)

const (
	// maxIngestBytes bounds a batch's JSON on the wire and again after
	// decompression. An agent's batch of 2,000 events is under 2 MiB, and
	// the server runs on a VM with 2 GB of memory.
	maxIngestBytes = 16 << 20
	// maxConcurrentIngests bounds the batches decoded at once, each of which
	// may hold maxIngestBytes of JSON and the events built from it. Ingest
	// writes one batch at a time anyway.
	maxConcurrentIngests = 4
	// maxFieldLen is far above any real string on the wire, so clipping
	// only ever touches corrupt input.
	maxFieldLen = 512
	// maxTokens is about a thousand times the largest response any model
	// produces, and keeps the sums built on counts far from int64 overflow.
	maxTokens = 1 << 40
	// maxNativeCostUSD bounds a self-reported cost, which goes straight into
	// billed spend.
	maxNativeCostUSD = 1_000_000.0
)

// authorised returns the GitHub login the caller's ingest token was enrolled
// by, and false for a token enrolment never issued or has revoked. Ingest is
// the one write path, and an open one lets anyone poison the numbers.
func (s *Server) authorised(r *http.Request) (string, bool, error) {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(got, schema.EnrolledTokenPrefix) {
		return "", false, nil
	}
	return s.DB.TokenLogin(r.Context(), got)
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	login, ok, err := s.authorised(r)
	if err != nil {
		// Not a 401, which an agent takes as its token refused -- something
		// retrying never fixes -- for what is a fault on this side.
		s.writeInternal(w, "ingest auth", err)
		return
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("invalid token"))
		return
	}
	if !requireJSON(w, r) {
		return
	}
	select {
	case ingestSlots <- struct{}{}:
		defer func() { <-ingestSlots }()
	default:
		// The agent keeps the batch and sends it again on its next pass.
		w.Header().Set("Retry-After", "30")
		writeErr(w, http.StatusServiceUnavailable,
			errors.New("the server is busy with other uploads; try again shortly"))
		return
	}

	// The byte limit applies after decompression too: a batch gzips about
	// 17x, and padding far more.
	r.Body = http.MaxBytesReader(w, r.Body, maxIngestBytes)
	src := io.Reader(r.Body)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("malformed gzip body"))
			return
		}
		defer zr.Close()
		src = http.MaxBytesReader(w, zr, maxIngestBytes)
	}

	var in ingestBody
	if err := json.NewDecoder(src).Decode(&in); err != nil {
		// 413 tells the agent to send less; a 400 would say its JSON is corrupt.
		var tooBig *http.MaxBytesError
		var tooLong *listTooLong
		switch {
		case errors.As(err, &tooBig):
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf(
				"a batch is at most %d MiB, before and after decompression", maxIngestBytes>>20))
		case errors.As(err, &tooLong):
			writeErr(w, http.StatusRequestEntityTooLarge, tooLong)
		default:
			writeErr(w, http.StatusBadRequest, errors.New("malformed request body"))
		}
		return
	}
	if in.V > schema.Version {
		// Accepted: refusing would break whoever upgraded first, and unknown
		// fields are ignored.
		s.Log.Warn("agent newer than server", "agent_v", in.V, "server_v", schema.Version)
	}

	batch, rejected := in.batch()
	res, err := s.DB.Ingest(r.Context(), login, &batch)
	var claimed *db.MachineClaimedError
	switch {
	case errors.As(err, &claimed):
		// The agent keeps the batch and retries, so nothing is lost while an
		// admin frees a laptop that changed hands. 409, not 403: the agent
		// reads 401 and 403 as its token refused and says to re-enrol, which
		// cannot help here.
		s.Log.Warn("ingest refused: the machine is registered to another login",
			"machine_id", claimed.Machine, "owner", claimed.Owner, "login", login)
		writeErr(w, http.StatusConflict, fmt.Errorf("%w; an admin frees it by running "+
			"the server with -revoke %s, which also stops that login's other machines "+
			"uploading until they enrol again", claimed, claimed.Owner))
		return
	case errors.Is(err, db.ErrNoMachine):
		writeErr(w, http.StatusBadRequest, err)
		return
	case err != nil:
		s.writeInternal(w, "ingest", err)
		return
	}
	if rejected > 0 {
		s.Log.Warn("rejected implausible events", "n", rejected, "machine_id", batch.MachineID)
	}
	// The agent takes received + rejected as the count it sent, and any other
	// reply as no acknowledgement of its batch.
	res.EventsReceived = len(batch.Events)
	res.EventsRejected = rejected
	res.ServerVersion = s.Version
	writeJSON(w, http.StatusOK, res)
}

// requireJSON refuses a body not declared as JSON. A page on another site can
// POST here without a preflight only with a CORS-simple content type, so this
// turns it away before it costs anything: a second lock beside the bearer
// header, which such a request cannot carry.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return true
	}
	writeErr(w, http.StatusUnsupportedMediaType,
		fmt.Errorf("%s requires Content-Type: application/json", r.URL.Path))
	return false
}

// ingestBody is a Batch as it arrives. Each list shadows the embedded one
// with a bounded type (encoding/json prefers the shallower field), and
// handleIngest stores only what batch() builds from them. A list added to
// schema.Batch must be shadowed here too, with a limit in listLimit;
// TestEveryListInABatchIsBounded fails until it is.
type ingestBody struct {
	schema.Batch
	Events        bounded[schema.Event]         `json:"events"`
	UnknownSource bounded[schema.UnknownSource] `json:"unknown_sources"`
	Accounts      bounded[schema.Account]       `json:"accounts"`
}

// batch returns what is worth storing -- the plausible events, with every
// string clipped -- and how many events were dropped.
func (in *ingestBody) batch() (schema.Batch, int) {
	b := in.Batch
	var rejected int
	b.Events, rejected = sanitise(in.Events)
	b.UnknownSource = in.UnknownSource
	b.Accounts = in.Accounts
	clipStrings(reflect.ValueOf(&b).Elem())
	return b, rejected
}

// sanitise drops events that cannot be true. The realistic source is a
// colleague's adapter misreading a format, and one bad row makes every total
// above it wrong, untraceably.
func sanitise(events []schema.Event) (kept []schema.Event, rejected int) {
	kept = make([]schema.Event, 0, len(events))
	// A far-future timestamp sits above every window, and "last seen"
	// arithmetic reads its negative age as healthy.
	floor := time.Now().AddDate(-10, 0, 0)
	ceil := time.Now().AddDate(0, 0, 2)
	for _, e := range events {
		switch {
		case !plausible(e.Usage),
			!e.TS.IsZero() && (e.TS.Before(floor) || e.TS.After(ceil)),
			e.NativeCostUSD != nil && (*e.NativeCostUSD < 0 || *e.NativeCostUSD > maxNativeCostUSD):
			rejected++
		default:
			kept = append(kept, e)
		}
	}
	return kept, rejected
}

// plausible bounds every counter before any is summed: out-of-range counts
// can wrap int64 into a total under the limit, and SQLite's SUM fails
// outright on overflow, taking every windowed endpoint with it.
func plausible(u schema.Usage) bool {
	for _, n := range [...]int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens,
		u.CacheWrite5mTokens, u.CacheWrite1hTokens, u.ReasoningTokens,
		u.WebSearchCalls, u.WebFetchCalls} {
		if n < 0 || n > maxTokens {
			return false
		}
	}
	return u.TotalTokens() <= maxTokens
}

// clipStrings clips every exported string reachable from v. It walks the
// types rather than naming fields, so a string added to the wire later is
// bounded too: an unclipped one is stored whole and served on every poll.
func clipStrings(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(clip(v.String(), maxFieldLen))
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				clipStrings(v.Field(i))
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			clipStrings(v.Index(i))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			clipStrings(v.Elem())
		}
	}
}

// clip bounds a string by runes, so a multi-byte character is never cut in
// half and stored as invalid UTF-8.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ingestSlots holds a slot for each batch being decoded. It is the process's
// memory it protects, so it is the process's bound, whichever Server asks.
var ingestSlots = make(chan struct{}, maxConcurrentIngests)

// listLimit is how many elements a batch's list of T may hold, and what the
// 413 calls them: near what an agent sends -- 2,000 events a batch, the
// logins it can see, the harnesses it found -- since the byte limit alone
// admits millions of minimal elements, each far larger decoded than on the
// wire.
func listLimit[T any]() (noun string, n int) {
	switch any(*new(T)).(type) {
	case schema.Event:
		return "events", 5_000
	case schema.UnknownSource:
		return "unknown sources", 64
	case schema.Account:
		return "accounts", 16
	}
	return "items", 0
}

// listTooLong is a list in a batch past its limit.
type listTooLong struct {
	noun  string
	limit int
}

func (e *listTooLong) Error() string {
	return fmt.Sprintf("a batch carries at most %d %s", e.limit, e.noun)
}

// bounded is a JSON array that stops decoding at its element type's limit,
// instead of materialising the whole slice to be measured afterwards.
type bounded[T any] []T

func (bounded[T]) limit() (string, int) { return listLimit[T]() }

func (b *bounded[T]) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil // null
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return errors.New("expected an array")
	}
	noun, limit := b.limit()
	for dec.More() {
		if len(*b) >= limit {
			return &listTooLong{noun, limit}
		}
		var v T
		if err := dec.Decode(&v); err != nil {
			return err
		}
		*b = append(*b, v)
	}
	return nil
}
