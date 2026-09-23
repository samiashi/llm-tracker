// Package tracker uploads the local archive to the team server.
package tracker

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/identity"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// batchSize bounds one request. A first run has tens of thousands of events
// queued, and sending them in chunks means a failure costs one chunk rather
// than the whole backfill.
const batchSize = 2000

type Client struct {
	BaseURL string
	Token   string
	Version string
	HTTP    *http.Client
	Log     *slog.Logger

	// versionWarned keeps the stale-agent notice to once per process: repeated
	// every pass, it would bury the log it is meant to stand out in.
	versionWarned atomic.Bool
}

func New(baseURL, token, version string, log *slog.Logger) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		Version: version,
		HTTP: &http.Client{
			Timeout: 120 * time.Second,
			// A redirect is never an acknowledgement: followed, a POST behind
			// a login redirect becomes a GET for the login page, whose 200
			// would mark the batch sent.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		Log: log,
	}
}

// Stats reports one sync run.
type Stats struct {
	Batches int
	Events  int
	Quota   int
	Unknown int
	// Skipped counts events the server refused because they predate its
	// retention floor.
	Skipped int
	// Retired counts rows taken out of the upload queue as a result (see
	// store.Refused).
	Retired int
	// Rejected counts events the server dropped as implausible.
	Rejected int
}

// Push uploads everything pending.
//
// Rows are marked sent only after the server acknowledges them, and its insert
// is idempotent, so a failure re-sends rows rather than losing them.
func (c *Client) Push(ctx context.Context, st *store.Store, machineID string) (*Stats, error) {
	stats := &Stats{}
	hostname, _ := os.Hostname()

	accounts := make([]schema.Account, 0, 2)
	for _, a := range identity.All() {
		accounts = append(accounts, *a)
	}

	unknown, err := st.AllUnknown(ctx)
	if err != nil {
		return stats, err
	}
	unknownDTO := make([]schema.UnknownSource, 0, len(unknown))
	for _, u := range unknown {
		unknownDTO = append(unknownDTO, schema.UnknownSource{
			V: schema.Version, MachineID: machineID, Path: u.Path,
			Hint: u.Hint, SizeBytes: u.SizeBytes, FirstSeen: u.FirstSeen,
			Status: schema.UnknownStatus(u.Status), Note: u.Note,
		})
	}
	// The whole set, once per push, flagged complete so the server replaces its
	// copy rather than accumulating -- including when the set is empty.
	unknownSent := false

	for {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}

		eventIDs, eventPayloads, err := st.Unsent(ctx, store.Events, batchSize)
		if err != nil {
			return stats, err
		}
		quotaIDs, quotaPayloads, err := st.Unsent(ctx, store.Quota, batchSize)
		if err != nil {
			return stats, err
		}
		if len(eventIDs) == 0 && len(quotaIDs) == 0 && unknownSent {
			return stats, nil
		}

		body := wireBatch{
			Batch: schema.Batch{
				V:            schema.Version,
				MachineID:    machineID,
				Hostname:     hostname,
				AgentVersion: c.Version,
				Accounts:     accounts,
			},
			Events: eventPayloads,
			Quota:  quotaPayloads,
		}
		if !unknownSent {
			body.UnknownSource = unknownDTO
			body.UnknownComplete = true
		}
		ack, err := c.ingest(ctx, body)
		if err != nil {
			return stats, err
		}

		// Re-offer whatever the server would now take: everything once it keeps
		// everything, else the refused rows at or above its current floor,
		// which stay at sent = 2 otherwise. Quota is never refused by age, so
		// any refused quota row goes back.
		requeued := int64(0)
		if from, ok := ack.acceptsFrom(); ok {
			for table, since := range map[store.Table]time.Time{store.Events: from, store.Quota: {}} {
				n, err := st.Requeue(ctx, table, since)
				if err != nil {
					return stats, err
				}
				if n > 0 {
					c.Log.Info("re-queueing rows the server refused before and would now take",
						"table", table, "requeued", n)
				}
				requeued += n
			}
		}

		// The server drops events older than its retention floor and says so.
		// They are retired, not marked sent (see store.Refused), so the rest of
		// the queue drains.
		if ack.EventsSkipped > 0 {
			retired := 0
			if before, ok := floorStart(ack.RetentionFloor); ok {
				// Events only: the server takes quota samples of any age, so
				// retiring them withholds samples it would accept, invisibly
				// to `status`.
				n, err := st.Refused(ctx, store.Events, before)
				if err != nil {
					return stats, err
				}
				retired = int(n)
			}
			c.Log.Warn("the server will not accept events older than its retention floor",
				"skipped", ack.EventsSkipped, "floor", ack.RetentionFloor, "retired", retired,
				"fix", "they predate the server's retention window and are kept "+
					"locally, not uploaded. The floor only ever moves forward, so "+
					"widening -retain will not re-admit them; turn pruning off to "+
					"let this backlog deliver")
			stats.Skipped += ack.EventsSkipped
			stats.Retired += retired
			// Continue only if the queue moved. Retiring nothing -- an older
			// server that reports no floor, or one that will not parse --
			// would fetch the same batch and loop forever inside one push.
			if retired == 0 {
				c.Log.Error("the server refused a batch but reported no retention floor",
					"skipped", ack.EventsSkipped,
					"fix", "upgrade the server; until then this backlog cannot be cleared")
				// An error, not nil: nil would record a successful sync, and
				// `status` would report a healthy agent whose backlog is stuck.
				return stats, fmt.Errorf(
					"server refused %d events and reported no usable retention floor",
					ack.EventsSkipped)
			}
			continue
		}
		if ack.EventsRejected > 0 {
			c.Log.Error("the server rejected events as implausible",
				"rejected", ack.EventsRejected, "batch", len(eventIDs))
			stats.Rejected += ack.EventsRejected
		}

		if err := st.MarkSent(ctx, store.Events, eventIDs); err != nil {
			return stats, err
		}
		if err := st.MarkSent(ctx, store.Quota, quotaIDs); err != nil {
			return stats, err
		}
		if !unknownSent {
			unknownSent = true
			stats.Unknown = len(unknownDTO)
		}

		stats.Batches++
		stats.Events += len(eventIDs)
		stats.Quota += len(quotaIDs)

		// A short batch means the queue is drained, unless this ack put rows
		// back into it.
		if len(eventIDs) < batchSize && len(quotaIDs) < batchSize && requeued == 0 {
			return stats, nil
		}
	}
}

// wireBatch is schema.Batch with its events and quota already encoded, so
// rows are not decoded and re-encoded on their way out of the archive.
//
// It embeds the schema type rather than mirroring it: the two fields below
// shadow Batch's own (encoding/json takes the shallowest field of a name), and
// everything else on the wire is schema.Batch's -- the type the privacy test
// reads. TestWireBatchIsSchemaBatch enforces it.
type wireBatch struct {
	schema.Batch
	Events []json.RawMessage `json:"events,omitempty"`
	Quota  []json.RawMessage `json:"quota,omitempty"`
}

// ack is the server's reply to one batch, as the agent reads it.
type ack struct {
	schema.IngestAck
	// A pointer, so a server that never sends the field is not mistaken for
	// one that keeps everything, which would re-queue every refused row on
	// every push.
	RetentionEnforced *bool `json:"retention_enforced"`
}

// acceptsFrom is the earliest instant the server takes events from, and false
// when it has not said.
func (a ack) acceptsFrom() (time.Time, bool) {
	switch {
	case a.RetentionEnforced == nil:
		return time.Time{}, false
	case !*a.RetentionEnforced:
		return time.Time{}, true
	}
	return floorStart(a.RetentionFloor)
}

// ingest uploads one batch and returns the server's acknowledgement of it.
//
// The batch is marked sent on the strength of this reply, so anything that is
// not unmistakably an acknowledgement of this batch is an error: the
// dashboard's HTML and a login page both answer 200.
func (c *Client) ingest(ctx context.Context, body wireBatch) (ack, error) {
	var a ack
	b, err := json.Marshal(body)
	if err != nil {
		return a, err
	}

	// Compressed on the way up: an event batch is mostly repeated key names.
	// Small bodies go as-is.
	encoding := ""
	if len(b) >= minGzipBytes {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, werr := zw.Write(b); werr == nil && zw.Close() == nil {
			b, encoding = buf.Bytes(), "gzip"
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/ingest", bytes.NewReader(b))
	if err != nil {
		return a, err
	}
	req.Header.Set("Content-Type", "application/json")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	// No Accept-Encoding: set by hand, it stops the transport decompressing
	// the reply, and a proxy's gzipped acknowledgement then fails to decode.
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return a, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return a, fmt.Errorf("server returned %d, redirecting to %q: the server URL "+
			"must be the tracker's own address", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp.StatusCode >= 300 {
		var msg struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		return a, fmt.Errorf("server returned %d: %s", resp.StatusCode, msg.Error)
	}

	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return a, fmt.Errorf("%s did not answer with an ingest acknowledgement "+
			"(is the server URL right?): %w", c.BaseURL, err)
	}
	if a.ServerVersion == "" || a.EventsReceived+a.EventsRejected != len(body.Events) {
		return a, fmt.Errorf("%s answered, but not with an acknowledgement of this batch "+
			"(is the server URL right?)", c.BaseURL)
	}
	c.noteServerVersion(a.ServerVersion)
	return a, nil
}

// Check sends an empty batch -- no machine, no events, so the server stores
// nothing -- and fails unless the reply is an ingest acknowledgement from a
// server that accepts this token. It goes through ingest, so it passes exactly
// when a real upload's acknowledgement would.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.ingest(ctx, wireBatch{Batch: schema.Batch{V: schema.Version, AgentVersion: c.Version}})
	return err
}

// AuthRejected reports whether a sync failure -- an error's text, or the copy
// the health record keeps -- means the server refused this agent's credential
// rather than being unreachable. One retries itself; the other never will.
//
// Matched on ingest's own "server returned <code>" wording, which is stable,
// never on a bare "401": a refusal of 401 events contains that too.
func AuthRejected(msg string) bool {
	return strings.Contains(msg, "server returned 401") ||
		strings.Contains(msg, "server returned 403")
}

// floorStart converts the server's retention floor -- a UTC day -- into the
// instant before which it accepts nothing.
//
// UTC because that is the clock the server derives an event's day in. Parsing
// locally would put the boundary up to a day out for anyone east or west of
// it, retiring rows the server would have taken or leaving rows it will not.
func floorStart(day string) (time.Time, bool) {
	if day == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// minGzipBytes is roughly where the gzip framing stops being most of the
// payload.
const minGzipBytes = 1024

// noteServerVersion warns once when the server is on a newer release than
// this agent, which then collects with an older adapter set: smaller numbers,
// not an error. A server behind the agent is the operator's upgrade, not this
// machine's. It never refuses to run: an agent that stops uploading because it
// is out of date turns a cosmetic problem into missing data.
func (c *Client) noteServerVersion(server string) {
	srv, ok := schema.ReleaseVersion(server)
	if !ok || server == c.Version {
		return
	}
	if own, isRelease := schema.ReleaseVersion(c.Version); isRelease && slices.Compare(srv[:], own[:]) <= 0 {
		return
	}
	if c.versionWarned.Swap(true) {
		return
	}
	c.Log.Warn("the server is on a newer release than this agent; usage may be "+
		"collected with an older adapter set",
		"agent", c.Version, "server", server,
		"upgrade", "llm-tracker-agent upgrade")
}
