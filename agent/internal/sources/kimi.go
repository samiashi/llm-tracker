package sources

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/samiashi/llm-tracker/schema"
)

func init() { Register(Kimi{}) }

// Kimi reads Moonshot's official Kimi Code CLI.
//
// UNVERIFIED: written against Moonshot's published data-locations document and
// community tooling, not a real installation. Run
// `llm-tracker-agent probe kimi` on a machine that has it before trusting the
// figures. Parsing is tolerant, so a field rename yields a zero rather than a
// crash, and the collector's silent-source alarm fires if the format moves.
type Kimi struct{}

func (Kimi) Name() schema.Source { return schema.SourceKimi }

// Roots covers both documented layouts. Moonshot's own docs describe
// ~/.kimi/sessions (overridable with KIMI_SHARE_DIR) while community tools
// report ~/.kimi-code/sessions (KIMI_CODE_HOME); the two ship under different
// product names and a machine may have either.
func (Kimi) Roots() []string {
	return []string{".kimi/sessions", ".kimi-code/sessions", ".kimi/imported_sessions"}
}

// kimiWire is one line of a session's wire.jsonl.
type kimiWire struct {
	Type  string `json:"type"`
	Scope string `json:"scope"`
	ID    string `json:"id"`
	Time  string `json:"timestamp"`
	Model string `json:"model"`

	Usage struct {
		// Kimi's own spellings.
		InputOther         int64 `json:"inputOther"`
		Output             int64 `json:"output"`
		InputCacheRead     int64 `json:"inputCacheRead"`
		InputCacheCreation int64 `json:"inputCacheCreation"`
		Reasoning          int64 `json:"reasoning"`

		openAIUsage
	} `json:"usage"`

	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
}

func (a Kimi) Collect(ctx context.Context, c *Ctx) (Result, error) {
	return walkJSONL(ctx, c, AbsRoots(a, c), baseName("wire.jsonl"), func(path string, at int64, line []byte) {
		var w kimiWire
		if json.Unmarshal(line, &w) != nil {
			c.Unparsed()
			return
		}
		if !strings.HasPrefix(w.Type, "usage.record") {
			return
		}
		// Kimi emits both turn-scoped and session-scoped usage records, and the
		// session-scoped ones are running totals for the whole conversation.
		// Counting both would roughly double every session.
		if w.Scope != "" && w.Scope != "turn" {
			return
		}

		usage := w.Usage.normalise()
		if w.Usage.InputOther > 0 || w.Usage.Output > 0 {
			usage = schema.Usage{
				InputTokens: w.Usage.InputOther, OutputTokens: w.Usage.Output,
				CacheReadTokens: w.Usage.InputCacheRead, CacheWrite5mTokens: w.Usage.InputCacheCreation,
				ReasoningTokens: w.Usage.Reasoning,
			}
		}
		if usage.TotalTokens() == 0 {
			return
		}

		nid := cmp.Or(w.RequestID, w.ID)
		if nid == "" {
			return
		}
		model := w.Model
		if model == "" {
			model = "kimi"
		}

		c.emit(schema.Event{
			V: schema.Version, ID: schema.MakeID(schema.SourceKimi, nid), NativeID: nid,
			Source: schema.SourceKimi, Surface: schema.SurfaceCLI,
			TS: parseTS(w.Time), MachineID: c.MachineID,
			// Endpoint is "moonshot", LiteLLM's provider name, which is what
			// the price table keys endpoints on; it has no "moonshotai".
			Provider: "moonshotai", Model: model, Endpoint: "moonshot",
			Usage: usage, SessionID: w.SessionID,
			// No detected account covers Moonshot, so whether this ran on a
			// subscription or a metered key is unknown.
			ProjectPath: sessionDirOf(path), CostBasis: schema.CostUnknown,
		})
	})
}

// sessionDirOf returns the session directory holding a wire log, which is the
// closest thing to a project identifier Kimi exposes without reading content.
func sessionDirOf(path string) string {
	dir := path
	for i := 0; i < 2; i++ {
		dir = parentDir(dir)
	}
	return dir
}

func parentDir(p string) string {
	if i := strings.LastIndex(p, string(os.PathSeparator)); i > 0 {
		return p[:i]
	}
	return p
}
