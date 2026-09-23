package sources

import (
	"cmp"
	"context"
	"encoding/json"

	"github.com/samiashi/llm-tracker/schema"
)

func init() { Register(DeepSeekHarness{}) }

// DeepSeekHarness reads DeepSeek's official harness (dsh).
//
// UNVERIFIED: written from DSH plugin documentation rather than a real
// installation. Probe it before trusting the numbers.
//
// DeepSeek publishes no account-level usage endpoint, so the local session log
// is the only record of what was spent.
type DeepSeekHarness struct{}

func (DeepSeekHarness) Name() schema.Source { return schema.SourceDeepSeek }

// Roots covers the CLI home and the desktop build, which versions its own
// directory (~/.dsh_desktop/<version>) and therefore accumulates several.
func (DeepSeekHarness) Roots() []string {
	return []string{".dsh", ".dsh_desktop"}
}

// dshLine is one record of a DSH session log.
type dshLine struct {
	Timestamp string `json:"timestamp"`
	CreatedAt int64  `json:"created_at"`

	ID        string `json:"id"`
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`

	Usage *openAIUsage `json:"usage"`
	// Some builds nest the API response rather than lifting usage out of it.
	Response *struct {
		ID    string       `json:"id"`
		Model string       `json:"model"`
		Usage *openAIUsage `json:"usage"`
	} `json:"response"`
}

func (a DeepSeekHarness) Collect(ctx context.Context, c *Ctx) (Result, error) {
	return walkJSONL(ctx, c, AbsRoots(a, c), anyOf(hasExt(".jsonl"), baseName("session.log")),
		func(_ string, _ int64, line []byte) {
			var l dshLine
			if json.Unmarshal(line, &l) != nil {
				c.Unparsed()
				return
			}

			usage, model, nid := l.Usage, l.Model, cmp.Or(l.RequestID, l.ID)
			if usage == nil && l.Response != nil {
				usage = l.Response.Usage
				if model == "" {
					model = l.Response.Model
				}
				nid = cmp.Or(nid, l.Response.ID)
			}
			if usage == nil || usage.empty() || nid == "" {
				return
			}

			if model == "" {
				model = "deepseek"
			}

			// DSH authenticates with a DeepSeek API key, so this is metered
			// spend rather than seat usage. No detected account covers
			// DeepSeek, so none is stamped.
			c.emit(schema.Event{
				V: schema.Version, ID: schema.MakeID(schema.SourceDeepSeek, nid), NativeID: nid,
				Source: schema.SourceDeepSeek, Surface: schema.SurfaceCLI,
				TS: pickTime(l.Timestamp, l.CreatedAt), MachineID: c.MachineID,
				Provider: "deepseek", Model: model, Endpoint: "deepseek",
				Usage:     usage.normalise(),
				SessionID: l.SessionID,
				CostBasis: schema.CostBilled,
			})
		})
}
