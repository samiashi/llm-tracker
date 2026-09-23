package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samiashi/llm-tracker/schema"
)

func init() { Register(Gemini{}) }

// Gemini reads Google's Gemini CLI.
//
// UNVERIFIED: neither the file names nor the fields below have been checked
// against real sessions. Current Gemini CLI builds are believed to record usage
// per message (a `tokens` object on each entry of
// tmp/<project>/chats/session-*.json), which this adapter does not read; it
// reads session-level totals, which such files may not carry at all. Probe it
// somewhere with real usage before trusting the figures.
//
// ~/.gemini is shared with Antigravity, a different product with a different
// format, so the root is scoped to tmp/.
type Gemini struct{}

func (Gemini) Name() schema.Source { return schema.SourceGemini }
func (Gemini) Roots() []string     { return []string{".gemini/tmp"} }

// geminiSession is one session_*.json aggregate. Its counters are running
// totals for the session, keyed on the session id, so the max-wins conflict
// rule upgrades the one row as the session grows.
type geminiSession struct {
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
	Model     string `json:"model"`
	StartTime string `json:"startTime"`
	LastTime  string `json:"lastUpdated"`

	InputTokens     int64 `json:"session_input_tokens"`
	OutputTokens    int64 `json:"session_output_tokens"`
	CachedTokens    int64 `json:"session_cached_tokens"`
	ReasoningTokens int64 `json:"session_reasoning_tokens"`
	ToolTokens      int64 `json:"session_tool_tokens"`

	// Some builds nest the same counters under "metrics".
	Metrics *struct {
		InputTokens     int64 `json:"session_input_tokens"`
		OutputTokens    int64 `json:"session_output_tokens"`
		CachedTokens    int64 `json:"session_cached_tokens"`
		ReasoningTokens int64 `json:"session_reasoning_tokens"`
	} `json:"metrics"`
}

func (a Gemini) Collect(ctx context.Context, c *Ctx) (Result, error) {
	var res Result

	for _, root := range AbsRoots(a, c) {
		err := walkRoot(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr
			}
			base := filepath.Base(path)
			if !strings.HasPrefix(base, "session") || !strings.HasSuffix(base, ".json") {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			res.Files++

			// Rewritten in place, so read whole rather than tailed, and capped:
			// these can reach hundreds of megabytes.
			info, ierr := d.Info()
			if ierr != nil {
				res.Errors = append(res.Errors, ierr)
				return nil //nolint:nilerr
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			if info.Size() > maxWholeFileBytes {
				res.Errors = append(res.Errors, fmt.Errorf(
					"%s is %d bytes, above the %d-byte cap for a whole-file read",
					filepath.Base(path), info.Size(), maxWholeFileBytes))
				return nil
			}

			b, rerr := os.ReadFile(path) //nolint:gosec // bounded above; root is a constant
			if rerr != nil {
				res.Errors = append(res.Errors, rerr)
				return nil //nolint:nilerr
			}
			res.BytesRead += int64(len(b))

			var s geminiSession
			if json.Unmarshal(b, &s) != nil {
				c.Unparsed()
				return nil //nolint:nilerr // counted: one odd file must not stop the rest
			}
			in, out, cached, reasoning := s.InputTokens, s.OutputTokens, s.CachedTokens, s.ReasoningTokens
			if m := s.Metrics; m != nil && in+out == 0 {
				in, out, cached, reasoning = m.InputTokens, m.OutputTokens, m.CachedTokens, m.ReasoningTokens
			}

			usage := schema.Usage{
				// Cached tokens are reported apart from input here, so there
				// is nothing to subtract.
				InputTokens: in + s.ToolTokens, OutputTokens: out,
				CacheReadTokens: cached, ReasoningTokens: reasoning,
			}
			if usage.TotalTokens() == 0 {
				return nil
			}
			// Every file is re-read on every pass, so this id must not change
			// between releases or each session is stored twice. Two id-less
			// files with one basename in different projects share it, and the
			// MAX rule keeps the larger.
			nid := firstNonEmpty(s.SessionID, s.ID, strings.TrimSuffix(base, ".json"))
			model := s.Model
			if model == "" {
				model = "gemini"
			}

			c.emit(schema.Event{
				V: schema.Version, ID: schema.MakeID(schema.SourceGemini, nid), NativeID: nid,
				Source: schema.SourceGemini, Surface: schema.SurfaceCLI,
				TS:        pickTime(firstNonEmpty(s.LastTime, s.StartTime), 0),
				MachineID: c.MachineID,
				Provider:  "gemini", Model: model, Endpoint: "gemini",
				Usage: usage, SessionID: nid, CostBasis: schema.CostRateCard,
			})
			return nil
		})
		if err != nil {
			res.Unparsed = c.takeUnparsed()
			return res, err
		}
	}
	res.Unparsed = c.takeUnparsed()
	return res, nil
}
