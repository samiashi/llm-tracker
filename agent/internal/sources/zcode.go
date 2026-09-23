package sources

import (
	"cmp"
	"context"
	"encoding/json"

	"github.com/samiashi/llm-tracker/schema"
)

func init() { Register(ZCode{}) }

// ZCode reads Z.ai's official GLM coding CLI.
//
// UNVERIFIED, and the least certain adapter here: Z.ai documents configuration
// and command history (~/.zai/history.json) but no usage record format. This
// looks for OpenAI-compatible usage objects in JSONL session logs under the
// plausible roots, and if the layout differs it finds nothing and the source
// never reports. Probe it on a machine with ZCode installed and correct the
// roots or field names from what the probe prints.
type ZCode struct{}

func (ZCode) Name() schema.Source { return schema.SourceZCode }

func (ZCode) Roots() []string {
	return []string{".zcode/sessions", ".zcode", ".zai/sessions"}
}

type zcodeLine struct {
	Timestamp string       `json:"timestamp"`
	CreatedAt int64        `json:"created_at"`
	ID        string       `json:"id"`
	RequestID string       `json:"request_id"`
	SessionID string       `json:"session_id"`
	Model     string       `json:"model"`
	Usage     *openAIUsage `json:"usage"`
}

func (a ZCode) Collect(ctx context.Context, c *Ctx) (Result, error) {
	return walkJSONL(ctx, c, AbsRoots(a, c), hasExt(".jsonl"), func(_ string, _ int64, line []byte) {
		var l zcodeLine
		if json.Unmarshal(line, &l) != nil {
			c.Unparsed()
			return
		}
		if l.Usage == nil || l.Usage.empty() {
			return
		}
		nid := cmp.Or(l.RequestID, l.ID)
		if nid == "" {
			return
		}
		model := l.Model
		if model == "" {
			model = "glm"
		}

		// No detected account covers Z.ai, so none is stamped.
		c.emit(schema.Event{
			V: schema.Version, ID: schema.MakeID(schema.SourceZCode, nid), NativeID: nid,
			Source: schema.SourceZCode, Surface: schema.SurfaceCLI,
			TS: pickTime(l.Timestamp, l.CreatedAt), MachineID: c.MachineID,
			Provider: "zai", Model: model, Endpoint: "zai",
			Usage:     l.Usage.normalise(),
			SessionID: l.SessionID,
			CostBasis: schema.CostBilled,
		})
	})
}
