package sources

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/samiashi/llm-tracker/schema"
)

// anthropicLine is one record from a Claude Code or Cowork transcript.
//
// The two products emit the same usage object but disagree on case for the
// surrounding keys -- Claude Code writes requestId and sessionId, Cowork writes
// request_id and session_id -- so both spellings are declared and whichever is
// populated wins. Nothing here reaches into message content.
type anthropicLine struct {
	Type string `json:"type"`

	RequestIDCamel string `json:"requestId"`
	RequestIDSnake string `json:"request_id"`
	SessionIDCamel string `json:"sessionId"`
	SessionIDSnake string `json:"session_id"`

	UUID        string `json:"uuid"`
	Timestamp   string `json:"timestamp"`
	Version     string `json:"version"`
	Entrypoint  string `json:"entrypoint"`
	IsSidechain bool   `json:"isSidechain"`

	// Effort is the session default; PerTurnEffort overrides it for a single
	// turn and is frequently null, so the per-turn value wins when present.
	Effort        string `json:"effort"`
	PerTurnEffort string `json:"perTurnEffort"`

	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			anthropicTokens

			OutputTokensDetails struct {
				ThinkingTokens int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`

			ServerToolUse struct {
				WebSearchRequests int64 `json:"web_search_requests"`
				WebFetchRequests  int64 `json:"web_fetch_requests"`
			} `json:"server_tool_use"`

			// Pricing modifiers that ride along with the usage object.
			InferenceGeo string `json:"inference_geo"`
			Speed        string `json:"speed"`

			// Iterations lists every attempt behind the response. The
			// top-level figures are the last one's; when a model falls back,
			// the attempt it abandoned is an earlier entry, on its own model.
			Iterations []anthropicIteration `json:"iterations"`
		} `json:"usage"`
	} `json:"message"`
}

// anthropicTokens is the token part of a usage object, shared by the response
// and each of its iterations.
type anthropicTokens struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`

	// Cache writes split by TTL, which are billed at different rates and so
	// never collapsed into cache_creation_input_tokens.
	CacheCreation struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// usage maps the tokens onto schema.Usage. A record that carries only the
// combined cache-write figure has it attributed to the cheaper 5m bucket, so
// a missing split under-states the bill rather than over-stating it.
func (t anthropicTokens) usage() schema.Usage {
	w5, w1h := t.CacheCreation.Ephemeral5m, t.CacheCreation.Ephemeral1h
	if w5 == 0 && w1h == 0 {
		w5 = t.CacheCreationInputTokens
	}
	return schema.Usage{
		InputTokens:        t.InputTokens,
		OutputTokens:       t.OutputTokens,
		CacheReadTokens:    t.CacheReadInputTokens,
		CacheWrite5mTokens: w5,
		CacheWrite1hTokens: w1h,
	}
}

type anthropicIteration struct {
	anthropicTokens
	Model string `json:"model"`
}

func (l *anthropicLine) requestID() string {
	if l.RequestIDCamel != "" {
		return l.RequestIDCamel
	}
	return l.RequestIDSnake
}

func (l *anthropicLine) sessionID() string {
	if l.SessionIDCamel != "" {
		return l.SessionIDCamel
	}
	return l.SessionIDSnake
}

// nativeID is the dedup key. requestId is preferred because it is what the two
// products share across the several streamed records they write per response.
func (l *anthropicLine) nativeID() string {
	if r := l.requestID(); r != "" {
		return r + "|" + l.Message.ID
	}
	if l.Message.ID != "" {
		return l.Message.ID
	}
	return l.UUID
}

func surfaceFor(entrypoint string) schema.Surface {
	switch strings.ToLower(entrypoint) {
	case "cli":
		return schema.SurfaceCLI
	case "claude-desktop", "desktop":
		return schema.SurfaceDesktop
	case "vscode", "jetbrains", "intellij":
		return schema.SurfaceIDE
	case "":
		return schema.SurfaceUnknown
	default:
		return schema.Surface(entrypoint)
	}
}

// toEvent maps a parsed line onto an Event, or returns false if the line
// carries no usage (most lines do not: user turns, tool results, system notes).
func (l *anthropicLine) toEvent(src schema.Source, c *Ctx, accountRef string) (schema.Event, bool) {
	if l.Type != "assistant" {
		return schema.Event{}, false
	}
	u := l.Message.Usage

	usage := u.usage()
	usage.ReasoningTokens = u.OutputTokensDetails.ThinkingTokens
	usage.WebSearchCalls = u.ServerToolUse.WebSearchRequests
	usage.WebFetchCalls = u.ServerToolUse.WebFetchRequests
	if usage.TotalTokens() == 0 {
		return schema.Event{}, false
	}

	nid := l.nativeID()
	if nid == "" {
		return schema.Event{}, false
	}

	// OAuth seats have no marginal cost; the tokens are already bought. Price
	// them as rate-card equivalent so they can be compared with API spend but
	// never added to it.
	basis := schema.CostRateCard
	if accountRef == "" {
		basis = schema.CostUnknown
	}

	return schema.Event{
		V:          schema.Version,
		ID:         schema.MakeID(src, nid),
		NativeID:   nid,
		Source:     src,
		Surface:    surfaceFor(l.Entrypoint),
		TS:         parseTS(l.Timestamp),
		MachineID:  c.MachineID,
		AccountRef: accountRef,
		Provider:   inferProvider(l.Message.Model),
		Model:      l.Message.Model,
		Usage:      usage,
		SessionID:  l.sessionID(),
		IsSubagent: l.IsSidechain,
		CostBasis:  basis,
		// "not_available" is the absent marker, not a geography.
		InferenceGeo: strings.TrimSuffix(u.InferenceGeo, "not_available"),
		Speed:        u.Speed,
		Effort:       cmp.Or(l.PerTurnEffort, l.Effort),
	}, true
}

// abandonedAttempts returns an event for each attempt a model fallback gave
// up on: every iteration but the last, whose usage is the response's own.
// Keyed on the response and the attempt's position, so the several streamed
// lines of one response store each attempt once.
func (l *anthropicLine) abandonedAttempts(response schema.Event) []schema.Event {
	its := l.Message.Usage.Iterations
	if len(its) < 2 {
		return nil
	}
	var out []schema.Event
	for i, it := range its[:len(its)-1] {
		usage := it.usage()
		if usage.TotalTokens() == 0 {
			continue
		}
		ev := response
		ev.NativeID = response.NativeID + "#" + strconv.Itoa(i)
		ev.ID = schema.MakeID(response.Source, ev.NativeID)
		ev.Model = cmp.Or(it.Model, "unknown")
		ev.Provider = inferProvider(ev.Model)
		ev.Usage = usage
		out = append(out, ev)
	}
	return out
}

// --- Claude Code -----------------------------------------------------------

func init() { Register(ClaudeCode{}) }

type ClaudeCode struct{}

func (ClaudeCode) Name() schema.Source { return schema.SourceClaudeCode }
func (ClaudeCode) Roots() []string     { return []string{".claude/projects"} }

func (a ClaudeCode) Collect(ctx context.Context, c *Ctx) (Result, error) {
	return walkJSONL(ctx, c, AbsRoots(a, c), hasExt(".jsonl"), func(path string, at int64, line []byte) {
		var l anthropicLine
		if json.Unmarshal(line, &l) != nil {
			c.Unparsed()
			return
		}
		// A session launched from Cowork keeps the account UUID in its project
		// path, which is exact. Everything else falls back to whichever
		// account was signed in when the event happened.
		acct := accountInPath(path)
		if acct == "" {
			acct = c.AccountRefAt("anthropic", parseTS(l.Timestamp))
		}
		if ev, ok := l.toEvent(schema.SourceClaudeCode, c, acct); ok {
			ev.AgentVersion = l.Version
			c.emit(ev)
			for _, a := range l.abandonedAttempts(ev) {
				c.emit(a)
			}
		}
	})
}

// --- Cowork ----------------------------------------------------------------

func init() { Register(Cowork{}) }

type Cowork struct{}

func (Cowork) Name() schema.Source { return schema.SourceCowork }

// Roots returns both session directories: Anthropic is renaming
// local-agent-mode-sessions to claude-code-sessions, and a machine that spanned
// the change keeps part of its history in each.
func (Cowork) Roots() []string {
	const base = "Library/Application Support/Claude/"
	return []string{base + "local-agent-mode-sessions", base + "claude-code-sessions"}
}

// accountInPath finds the account UUID that follows "workspaces-" in a path.
// Cowork runs Claude Code sessions in a scratch workspace named after the
// account, so their project directory carries the attribution the transcript
// omits.
func accountInPath(path string) string {
	for _, seg := range strings.Split(path, string(os.PathSeparator)) {
		for _, part := range strings.Split(seg, "-scratch-") {
			if i := strings.LastIndex(part, "workspaces-"); i >= 0 {
				cand := part[i+len("workspaces-"):]
				if len(cand) >= 36 && isUUID(cand[:36]) {
					return "anthropic:" + cand[:36]
				}
			}
		}
	}
	return ""
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// accountFromPath recovers the owning account from the directory layout.
// Cowork nests sessions as <root>/<accountUuid>/<orgUuid>/<sessionUuid>/, so
// attribution is exact, even for history captured before the agent ran.
func accountFromPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) < 2 || len(parts[0]) != 36 {
		return ""
	}
	return "anthropic:" + parts[0]
}

func (a Cowork) Collect(ctx context.Context, c *Ctx) (Result, error) {
	roots := AbsRoots(a, c)

	return walkJSONL(ctx, c, roots, hasExt(".jsonl"), func(path string, at int64, line []byte) {
		var l anthropicLine
		if json.Unmarshal(line, &l) != nil {
			c.Unparsed()
			return
		}
		acct := c.AccountRefAt("anthropic", parseTS(l.Timestamp))
		for _, r := range roots {
			if strings.HasPrefix(path, r) {
				if got := accountFromPath(r, path); got != "" {
					acct = got
				}
				break
			}
		}

		if ev, ok := l.toEvent(schema.SourceCowork, c, acct); ok {
			ev.Surface = schema.SurfaceDesktop
			c.emit(ev)
			for _, a := range l.abandonedAttempts(ev) {
				c.emit(a)
			}
		}
	})
}
