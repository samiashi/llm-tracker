// Package schema is the single source of truth for what the agent collects
// and what the server stores. Agent and server both import it, so the two
// cannot drift.
package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Version is the wire format version. The agent stamps every batch with it.
// Old agents stay installed on teammates' machines for months, so the server
// must keep accepting older versions rather than rejecting them.
const Version = 1

// Source identifies which harness produced an event.
type Source string

const (
	SourceClaudeCode Source = "claude_code"
	SourceCowork     Source = "cowork"
	SourceCodex      Source = "codex"
	SourceOpenCode   Source = "opencode"
	SourceGemini     Source = "gemini"
	SourceCopilot    Source = "copilot"

	// Official first-party harnesses for the open-weight models.
	SourceKimi     Source = "kimi"
	SourceDeepSeek Source = "dsh"
	SourceZCode    Source = "zcode"

	// Editor extensions, run inside VS Code rather than a terminal.
	SourceCline    Source = "cline"
	SourceRooCode  Source = "roo_code"
	SourceContinue Source = "continue"
)

// Surface is where the harness was driven from: a terminal, a desktop app or
// an IDE.
type Surface string

const (
	SurfaceCLI     Surface = "cli"
	SurfaceDesktop Surface = "desktop"
	SurfaceIDE     Surface = "ide"
	SurfaceUnknown Surface = "unknown"
)

// CostBasis records whether an event cost real money.
//
// Subscription seats (Claude Max, ChatGPT/Codex plans) have no marginal dollar
// cost: the tokens are already paid for by the seat. Summed with metered API
// spend they produce a number that means nothing, so the two are kept apart.
type CostBasis string

const (
	// CostBilled is metered API usage. Real money.
	CostBilled CostBasis = "billed"
	// CostRateCard is subscription usage priced at public API rates: what it
	// *would* have cost. Useful for comparing seats against API, never for
	// claiming spend.
	CostRateCard CostBasis = "rate_card_equivalent"
	// CostUnknown means the adapter cannot tell whether the usage was paid
	// per token or covered by a seat: Claude Code, Cowork, Codex and Kimi with
	// no detected account, and all of Copilot. The server still prices it, as
	// a third figure never summed with billed or rate-card spend.
	CostUnknown CostBasis = "unknown"
)

// Event is one model response. It carries counts and provenance only.
//
// There are deliberately no fields for prompts, completions, file contents,
// diffs, or tool arguments. This is the privacy boundary, and it is enforced
// by the type rather than by remembering to filter: the agent has nowhere to
// put conversation text even if a future parser accidentally extracted it.
type Event struct {
	V int `json:"v"`

	// ID is the idempotency key: sha256(source + ":" + native_id), truncated.
	// The server upserts on it under MAX rules (AGENTS.md invariant 1), so
	// re-reading a file can upgrade a reading but never count it twice.
	ID string `json:"id"`

	// NativeID is the provider's own identifier for the response:
	// requestId (Claude Code / Cowork), response_id (Codex), message id
	// (opencode). Kept for debugging and for re-deriving ID.
	NativeID string `json:"native_id"`

	Source  Source    `json:"source"`
	Surface Surface   `json:"surface"`
	TS      time.Time `json:"ts"`

	// MachineID is a stable per-machine UUID. One person may have several.
	MachineID string `json:"machine_id"`
	// AccountRef fingerprints the account that was active when this event was
	// captured. It must be stamped at capture time: ~/.codex/auth.json holds
	// exactly one account_id and switching overwrites it, so the association
	// cannot be reconstructed later.
	AccountRef string `json:"account_ref,omitempty"`

	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Endpoint is the base URL when it is not the provider default. Pricing
	// keys on it as well as the model (see PriceTable).
	Endpoint string `json:"endpoint,omitempty"`

	Usage Usage `json:"usage"`

	// InferenceGeo and Speed are Anthropic's pricing modifiers, read from the
	// transcript and applied by PriceTable.Cost.
	InferenceGeo string `json:"inference_geo,omitempty"`
	Speed        string `json:"speed,omitempty"`

	// Effort is the reasoning level the turn ran at (see effortOrder). It is
	// not a pricing input: reasoning tokens bill as ordinary output.
	Effort string `json:"effort,omitempty"`

	// CostBasis says whether this event cost real money. Only the adapter
	// knows: it is a property of how the account is paid for, not of the
	// tokens.
	CostBasis CostBasis `json:"cost_basis"`

	// NativeCostUSD is the harness's own cost figure, when it computes one.
	// opencode and Cline record a per-response cost from the rates the
	// provider actually charged; that beats anything our bundled table can
	// infer, so the server prefers it and uses the table only when it is absent.
	NativeCostUSD *float64 `json:"native_cost_usd,omitempty"`

	SessionID string `json:"session_id,omitempty"`
	// IsSubagent marks sidechain/subagent traffic. Worth isolating: Claude
	// Code's cleanup deletes parent transcripts at 30 days but leaves
	// subagents/ behind, so these outlive the sessions they belong to.
	IsSubagent bool `json:"is_subagent,omitempty"`

	AgentVersion string `json:"agent_version,omitempty"`

	// Collector is the agent's CollectorVersion when it read this event. A
	// newer collector's reading replaces an older one's on the server, which
	// is how a corrected adapter reaches rows already uploaded.
	Collector int `json:"collector,omitempty"`
}

// Usage holds the token counts.
type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`

	// Cache writes are split by TTL because they are priced differently: a 1h
	// write costs more than a 5m one, and Claude Code reports them separately.
	CacheWrite5mTokens int64 `json:"cache_write_5m_tokens"`
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens"`

	// ReasoningTokens is a subset of OutputTokens, not an addition to it.
	// Never add it to the total.
	ReasoningTokens int64 `json:"reasoning_tokens"`

	// Server-side tools are billed per call, not per token. Invisible to
	// anything that only sums tokens.
	WebSearchCalls int64 `json:"web_search_calls"`
	WebFetchCalls  int64 `json:"web_fetch_calls"`
}

// TotalTokens is the billable token count. ReasoningTokens is excluded
// because it is already counted inside OutputTokens.
func (u Usage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens +
		u.CacheWrite5mTokens + u.CacheWrite1hTokens
}

// UnknownSource reports an agent directory found on a machine that we have no
// adapter for, so a harness shows up as "detected, unsupported" instead of
// silently missing.
type UnknownSource struct {
	V         int       `json:"v"`
	MachineID string    `json:"machine_id"`
	Path      string    `json:"path"`
	Hint      string    `json:"hint,omitempty"`
	SizeBytes int64     `json:"size_bytes"`
	FirstSeen time.Time `json:"first_seen"`

	// Status separates "no adapter yet" from "investigated, cannot be done",
	// so a harness already ruled out does not prompt the same investigation
	// again.
	Status UnknownStatus `json:"status,omitempty"`
	// Note records the finding behind a blocked status.
	Note string `json:"note,omitempty"`
}

// UnknownStatus is why a detected harness has no adapter.
type UnknownStatus string

const (
	// StatusTodo means the format looks readable; nobody has written it yet.
	StatusTodo UnknownStatus = "todo"
	// StatusBlocked means it was investigated and cannot be supported.
	StatusBlocked UnknownStatus = "blocked"
)

// Batch is one agent->server upload.
type Batch struct {
	V             int             `json:"v"`
	MachineID     string          `json:"machine_id"`
	Hostname      string          `json:"hostname,omitempty"`
	AgentVersion  string          `json:"agent_version"`
	Events        []Event         `json:"events,omitempty"`
	UnknownSource []UnknownSource `json:"unknown_sources,omitempty"`

	// UnknownComplete marks UnknownSource as the machine's entire current set
	// rather than an increment, letting the server replace what it holds.
	// Without it the list only grows: a harness that gains an adapter would
	// keep its stale "unsupported" row forever.
	UnknownComplete bool `json:"unknown_complete,omitempty"`

	// Accounts are the provider logins active on the machine right now.
	//
	// Order carries meaning: the first is taken as the machine's own account
	// and credited with any event that arrives without an account_ref --
	// opencode authenticates per provider and has no notion of a person.
	Accounts []Account `json:"accounts,omitempty"`
}

// Account names one provider login on the uploading machine.
//
// Email is carried deliberately: an opaque ref cannot be mapped back to a
// colleague. It lives here, beside Event, so the privacy test that reads the
// wire types reads it too.
type Account struct {
	Ref      string `json:"ref"`
	Provider string `json:"provider"`
	Email    string `json:"email"`
	PlanType string `json:"plan_type"`
}

// IngestAck is the server's reply to a Batch.
//
// Defined once for both halves. Old agents stay installed for months, so a
// field the agent reads must keep its name here for as long as they do.
type IngestAck struct {
	// ServerVersion is the release the server is on. Both halves ship from one
	// tag, so an agent that differs knows it is stale without asking GitHub.
	ServerVersion string `json:"server_version"`

	EventsReceived int `json:"events_received"`
	EventsStored   int `json:"events_stored"`
	// EventsSkipped counts events for days before RetentionFloor.
	EventsSkipped int `json:"events_skipped"`
	// EventsRejected counts events dropped as implausible.
	EventsRejected int `json:"events_rejected"`
	Unpriced       int `json:"unpriced"`

	// RetentionFloor is the earliest day ingest accepts right now, so an agent
	// can retire what it cannot deliver and re-offer what it now can. Empty
	// when nothing is refused.
	RetentionFloor string `json:"retention_floor,omitempty"`
	// RetentionEnforced is true exactly when RetentionFloor is set. Always
	// present, so "refuses nothing" is distinguishable from an older server
	// that does not report it.
	RetentionEnforced bool `json:"retention_enforced"`
}

// EnrollRequest asks the server for an ingest token of this machine's own.
// The GitHub token that proves org membership travels as the bearer, never in
// the body.
type EnrollRequest struct {
	Hostname string `json:"hostname"`
}

// EnrollResponse carries the machine's ingest token. The server keeps only a
// hash of it, so this reply is the one place it can ever be read.
type EnrollResponse struct {
	Token         string `json:"token"`
	Login         string `json:"login"`
	ServerVersion string `json:"server_version"`
}

// EnrolledTokenPrefix starts every token enrolment issues: ingest refuses
// anything else without a lookup, and a re-run enroll knows a saved token for
// one of its own.
const EnrolledTokenPrefix = "ctk_"

// MakeID derives the stable idempotency key for an event.
func MakeID(source Source, nativeID string) string {
	sum := sha256.Sum256([]byte(string(source) + ":" + nativeID))
	return hex.EncodeToString(sum[:16])
}
