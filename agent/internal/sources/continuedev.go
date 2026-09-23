package sources

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/samiashi/llm-tracker/schema"
)

func init() { Register(ContinueDev{}) }

// ContinueDev reads Continue's local development data.
//
// UNVERIFIED: written against Continue's published development-data document,
// not a real installation. Run `llm-tracker-agent probe continue` on a
// machine that has it before trusting the figures.
//
// Continue writes one JSONL file per event kind under ~/.continue/dev_data,
// and tokensGenerated.jsonl is the one that carries counts.
type ContinueDev struct{}

func (ContinueDev) Name() schema.Source { return schema.SourceContinue }

// Roots is the whole dev_data tree because Continue versions its schema by
// directory (dev_data/0.2.0/...), and a new version would otherwise go unread
// until someone noticed the source had gone quiet.
func (ContinueDev) Roots() []string { return []string{".continue/dev_data"} }

// continueTokens is one line of tokensGenerated.jsonl.
//
// Continue counts only prompt and generated tokens; it does not report cache
// reads or writes separately, so those stay zero rather than being guessed at.
type continueTokens struct {
	PromptTokens    int64  `json:"promptTokens"`
	GeneratedTokens int64  `json:"generatedTokens"`
	Model           string `json:"model"`
	Provider        string `json:"provider"`
	Timestamp       string `json:"timestamp"`
	// Continue's newer builds nest the payload rather than flattening it.
	Data struct {
		PromptTokens    int64  `json:"promptTokens"`
		GeneratedTokens int64  `json:"generatedTokens"`
		Model           string `json:"model"`
		Provider        string `json:"provider"`
	} `json:"data"`
}

func (a ContinueDev) Collect(ctx context.Context, c *Ctx) (Result, error) {
	root := filepath.Join(c.Home, a.Roots()[0])
	return walkJSONL(ctx, c, AbsRoots(a, c), baseName("tokensGenerated.jsonl"),
		func(path string, at int64, line []byte) {
			var t continueTokens
			if json.Unmarshal(line, &t) != nil {
				c.Unparsed()
				return
			}

			in, out := t.PromptTokens, t.GeneratedTokens
			model, provider := t.Model, t.Provider
			if in == 0 && out == 0 {
				in, out = t.Data.PromptTokens, t.Data.GeneratedTokens
				model, provider = t.Data.Model, t.Data.Provider
			}
			usage := schema.Usage{InputTokens: in, OutputTokens: out}
			if usage.TotalTokens() == 0 {
				return
			}
			if model == "" {
				model = "unknown"
			}
			if provider == "" {
				provider = modelProvider(model)
			}

			ts := parseTS(t.Timestamp)
			if ts.IsZero() {
				ts = fileMTime(path)
			}

			// No record id, and second-precision timestamps, so identity is
			// the file's path under dev_data and the byte the line starts at.
			// Not the basename: every version directory holds a
			// tokensGenerated.jsonl, and their offsets would collide.
			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = filepath.Base(path)
			}
			nid := filepath.ToSlash(rel) + "#" + strconv.FormatInt(at, 10)

			ev := schema.Event{
				V: schema.Version, ID: schema.MakeID(schema.SourceContinue, nid), NativeID: nid,
				Source: schema.SourceContinue, Surface: schema.SurfaceIDE,
				TS: ts, MachineID: c.MachineID,
				Provider: provider, Model: model, Endpoint: provider,
				Usage: usage,
				// Continue calls whatever key the developer configured.
				CostBasis: schema.CostBilled,
			}
			ev.AccountRef = c.AccountRefAt(provider, ts)
			c.emit(ev)
		})
}

// modelProvider guesses a provider from a model name for the records where
// Continue omits it. Kept deliberately small: a wrong guess prices an event
// against the wrong table, so anything unrecognised stays empty.
func modelProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"):
		return "openai"
	case strings.HasPrefix(m, "gemini"):
		return "google"
	}
	return ""
}
