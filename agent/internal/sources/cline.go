package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

func init() {
	// Roo Code is a fork of Cline and still writes the same task layout, so
	// one implementation serves both. They are separate sources because a team
	// running both wants to see which one the tokens went through.
	Register(clineFamily{
		name: schema.SourceCline,
		root: "Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/tasks",
		alt:  "Library/Application Support/Cursor/User/globalStorage/saoudrizwan.claude-dev/tasks",
	})
	Register(clineFamily{
		name: schema.SourceRooCode,
		root: "Library/Application Support/Code/User/globalStorage/rooveterinaryinc.roo-cline/tasks",
		alt:  "Library/Application Support/Cursor/User/globalStorage/rooveterinaryinc.roo-cline/tasks",
	})
}

// clineFamily reads Cline and its forks.
//
// UNVERIFIED: written against the documented on-disk layout and third-party
// parsers, not a real installation. Run `llm-tracker-agent probe cline` (or
// `probe roo_code`) on a machine that has it before trusting the figures.
//
// Each task is a directory under globalStorage holding ui_messages.json, a JSON
// array of UI events. One request is one entry with say == "api_req_started",
// whose `text` field is itself a JSON *string* that has to be decoded a second
// time before the token counts are reachable.
//
// Cline drops requests from a task on a checkpoint restore, and tasks can be
// deleted, so its history does not outlive our archive (no KeepsHistory).
type clineFamily struct {
	name schema.Source
	root string
	// alt is the same extension inside Cursor, which keeps its own
	// globalStorage the Code path never sees.
	alt string
}

func (a clineFamily) Name() schema.Source { return a.name }
func (a clineFamily) Roots() []string     { return []string{a.root, a.alt} }

// clineMessage is one entry of ui_messages.json.
type clineMessage struct {
	Say string `json:"say"`
	// TS is milliseconds since the epoch.
	TS int64 `json:"ts"`
	// Text carries the request's usage, JSON-encoded inside a JSON string.
	Text string `json:"text"`
	// ModelInfo is what current Cline writes. Older builds put the protocol in
	// the Text blob's apiProtocol instead, which is why both are read.
	ModelInfo struct {
		ProviderID string `json:"providerId"`
		ModelID    string `json:"modelId"`
	} `json:"modelInfo"`
}

// clineReq is the decoded `text` of an api_req_started entry.
//
// tokensIn excludes what was served from cache: Cline reports the two
// separately in its own UI, and adding cacheReads back into input would price
// cache hits at the full input rate.
type clineReq struct {
	TokensIn    int64  `json:"tokensIn"`
	TokensOut   int64  `json:"tokensOut"`
	CacheReads  int64  `json:"cacheReads"`
	CacheWrites int64  `json:"cacheWrites"`
	APIProtocol string `json:"apiProtocol"`
	Model       string `json:"model"`
	// Cost is a pointer so an absent key is distinguishable from a real zero.
	// Cline writes none for local providers and models it cannot price; read
	// as 0.0 it would be an authoritative "free" the server never re-prices.
	Cost *float64 `json:"cost"`
}

func (a clineFamily) Collect(ctx context.Context, c *Ctx) (Result, error) {
	var res Result

	// One mtime watermark per task, under a prefix rewind clears.
	taskKey := string(a.name) + ":task:"

	for _, root := range AbsRoots(a, c) {
		entries, err := os.ReadDir(root)
		if err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(root, e.Name(), "ui_messages.json")

			// A task file is rewritten whole as its conversation grows, so
			// there is no byte offset to resume from. Its mtime is the
			// watermark instead, compared per task: a single high-water mark
			// drops every task that arrives "behind" it -- restored, copied
			// with its mtime, synced, or written while another was read.
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			mtime := fi.ModTime().UnixMilli()
			seenKey := taskKey + e.Name()
			var seen int64
			if v, err := c.Store.Meta(ctx, seenKey); err == nil && v != "" {
				seen, _ = strconv.ParseInt(v, 10, 64)
			}
			if mtime <= seen {
				continue
			}

			if fi.Size() > maxWholeFileBytes {
				res.Errors = append(res.Errors, fmt.Errorf(
					"%s is %d bytes, above the %d-byte cap for a whole-file read",
					filepath.Join(e.Name(), "ui_messages.json"), fi.Size(), maxWholeFileBytes))
				continue
			}

			complete, err := a.readTask(c, path, e.Name())
			if err != nil {
				res.Errors = append(res.Errors, err)
				continue
			}
			res.Files++
			res.BytesRead += fi.Size()
			// Only a task that parsed is watermarked. A half-written one whose
			// last write shares the stat's millisecond would otherwise never
			// be read again.
			if !complete {
				continue
			}
			// Staged, so the mark commits with the rows it covers.
			c.StageMeta(seenKey, strconv.FormatInt(mtime, 10))
		}
	}

	stored, err := c.CommitPending(ctx)
	res.Stored = stored
	return res, err
}

// readTask emits one task file's requests. complete is false for a half-written
// file, which must not be watermarked.
func (a clineFamily) readTask(c *Ctx, path, taskID string) (complete bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	// A half-written task is skipped, not reported: the next pass reads it
	// whole. A file that parses into the wrong shape is reported.
	if !json.Valid(raw) {
		return false, nil
	}
	var msgs []clineMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return false, err
	}

	// The model is stamped on messages rather than on the request, so the
	// most recent one seen above a request is the one that served it.
	var provider, model string
	sameMS := map[int64]int{}

	for _, m := range msgs {
		if m.ModelInfo.ModelID != "" {
			provider, model = m.ModelInfo.ProviderID, m.ModelInfo.ModelID
		}
		if m.Say != "api_req_started" || m.Text == "" {
			continue
		}
		var req clineReq
		if json.Unmarshal([]byte(m.Text), &req) != nil {
			continue
		}

		usage := schema.Usage{
			InputTokens: req.TokensIn, OutputTokens: req.TokensOut,
			CacheReadTokens: req.CacheReads, CacheWrite5mTokens: req.CacheWrites,
		}
		if usage.TotalTokens() == 0 {
			continue
		}

		prov := firstNonEmpty(provider, req.APIProtocol)
		mdl := firstNonEmpty(model, req.Model)
		if mdl == "" {
			mdl = "unknown"
		}

		// Identity is the task, the request's own timestamp, and its position
		// among requests sharing that millisecond. Not the array index: a
		// checkpoint restore, a "delete messages after this point" or a
		// condense removes entries, and every request below the edit would
		// re-emit under a new id.
		sameMS[m.TS]++
		nid := taskID + "#" + strconv.FormatInt(m.TS, 10) + "#" + strconv.Itoa(sameMS[m.TS]-1)

		ev := schema.Event{
			V: schema.Version, ID: schema.MakeID(a.name, nid), NativeID: nid,
			Source: a.name, Surface: schema.SurfaceIDE,
			TS: unixMilliOrZero(m.TS), MachineID: c.MachineID,
			Provider: prov, Model: mdl, Endpoint: prov,
			Usage: usage, SessionID: taskID,
			// Cline drives whatever key the developer configured, so its spend
			// is metered against that key rather than a seat.
			CostBasis: schema.CostBilled,
		}
		ev.AccountRef = c.AccountRefAt(prov, ev.TS)
		if req.Cost != nil {
			cost := *req.Cost
			ev.NativeCostUSD = &cost
		}

		c.emit(ev)
	}
	return true, nil
}

// unixMilliOrZero converts Cline's millisecond timestamps, tolerating the
// zero that a malformed entry yields rather than dating it to 1970.
func unixMilliOrZero(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
