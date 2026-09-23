package sources

import (
	"cmp"
	"context"
	"encoding/json"

	"github.com/samiashi/llm-tracker/schema"
)

func init() { Register(Copilot{}) }

// Copilot reads GitHub Copilot CLI.
//
// UNVERIFIED: not checked against a real session-state directory. Probe it
// before trusting the figures.
//
// Per GitHub's documentation, session-aggregate totals are always recorded but
// per-turn input and cache attribution only at debug log level, so a low
// Copilot figure may be coarser than other harnesses' rather than wrong.
type Copilot struct{}

func (Copilot) Name() schema.Source { return schema.SourceCopilot }

// Roots scopes to the session store. ~/.copilot/logs holds process logs with
// no usage in them, and walking those on every pass would be pure cost.
func (Copilot) Roots() []string {
	return []string{".copilot/session-state", ".copilot/history-session-state"}
}

// copilotEvent is one line of a Copilot CLI JSONL session log.
type copilotEvent struct {
	Timestamp string `json:"timestamp"`
	CreatedAt int64  `json:"created_at"`

	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	ID        string `json:"id"`
	Model     string `json:"model"`

	Usage *openAIUsage `json:"usage"`
	// Some builds nest usage under a response or turn object.
	Response *struct {
		ID    string       `json:"id"`
		Model string       `json:"model"`
		Usage *openAIUsage `json:"usage"`
	} `json:"response"`
	Turn *struct {
		Usage *openAIUsage `json:"usage"`
	} `json:"turn"`
}

func (a Copilot) Collect(ctx context.Context, c *Ctx) (Result, error) {
	return walkJSONL(ctx, c, AbsRoots(a, c), anyOf(hasExt(".jsonl"), hasExt(".log")),
		func(_ string, _ int64, line []byte) {
			var e copilotEvent
			if json.Unmarshal(line, &e) != nil {
				c.Unparsed()
				return
			}

			usage, model, nid := e.Usage, e.Model, cmp.Or(e.RequestID, e.ID)
			if usage == nil && e.Response != nil {
				usage = e.Response.Usage
				model = cmp.Or(model, e.Response.Model)
				nid = cmp.Or(nid, e.Response.ID)
			}
			if usage == nil && e.Turn != nil {
				usage = e.Turn.Usage
			}
			if usage == nil || usage.empty() || nid == "" {
				return
			}

			if model == "" {
				model = "copilot"
			}

			// Copilot bills by seat and premium request, not by token, so the
			// basis is unknown: the volume stays visible without an invented
			// cost. No detected account covers GitHub, so none is stamped.
			c.emit(schema.Event{
				V: schema.Version, ID: schema.MakeID(schema.SourceCopilot, nid), NativeID: nid,
				Source: schema.SourceCopilot, Surface: schema.SurfaceCLI,
				TS: pickTime(e.Timestamp, e.CreatedAt), MachineID: c.MachineID,
				Provider: "github", Model: model,
				Usage:     usage.normalise(),
				SessionID: e.SessionID,
				CostBasis: schema.CostUnknown,
			})
		})
}
