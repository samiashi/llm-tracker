package schema

import (
	"sort"
	"strconv"
	"strings"
)

// effortOrder is the canonical ordering of reasoning effort levels, least to
// most intensive.
//
// Two vendors, two scales, which is why this has to be stated rather than
// inferred. Claude runs low → medium → high → xhigh → max. Codex runs
// minimal → low → medium → high → xhigh, with ultra above that — the most
// intensive setting either vendor offers, and the top of this scale.
//
// Alphabetical sorting puts "max" before "medium" and "xhigh" last, which is
// wrong in both directions and makes a chart of an ordinal scale read as noise.
var effortOrder = []string{
	// opencode's word for "no level chosen, use the provider's own". Ranked
	// lowest because picking anything else is an escalation from it -- not
	// because it is literally the least reasoning, which varies by provider.
	"default",
	"minimal",
	"low",
	"medium",
	"high",
	"xhigh",
	"max",
	"ultra",
}

// effortAlias maps names that are not really levels onto the one they run at.
//
// "ultracode" is the notable case: Claude Code presents it above max in the
// effort menu, but it is xhigh plus workflow orchestration rather than a sixth
// tier, and a new session reverts to xhigh. Ranking it above max would put the
// busiest bar in the wrong place on a scale that is supposed to mean something.
var effortAlias = map[string]string{
	"ultracode": "xhigh",
}

// EffortDisplayOrder is the scale as a chart must draw it: effortOrder with
// every alias spliced in directly after the level it runs at.
//
// effortOrder stays the pure scale, since an alias listed there would take a
// rank of its own. But a chart's column list must contain every value it can
// be handed -- MatrixBars ranks a missing one last, drawing "ultracode" above
// ultra -- and must agree with EffortOrderSQL, which ranks it at xhigh.
// Returns a fresh slice: it goes out as an API field, and a caller that
// reorders it must not reorder the scale for everyone.
func EffortDisplayOrder() []string {
	byLevel := make(map[string][]string, len(effortAlias))
	for alias, level := range effortAlias {
		byLevel[level] = append(byLevel[level], alias)
	}
	// Sorted so the column order is identical run to run; map order is random.
	for _, aliases := range byLevel {
		sort.Strings(aliases)
	}

	out := make([]string, 0, len(effortOrder)+len(effortAlias))
	for _, e := range effortOrder {
		out = append(out, e)
		out = append(out, byLevel[e]...)
	}
	return out
}

var effortRank = buildEffortRank()

// buildEffortRank indexes effortOrder. Factored out so a test can extend the
// scale and rebuild, to check the generated SQL past nine levels.
func buildEffortRank() map[string]int {
	m := make(map[string]int, len(effortOrder))
	for i, e := range effortOrder {
		m[e] = i
	}
	return m
}

// EffortRank returns a sort key for an effort level. Anything unrecognised
// sorts last, so a new level from either vendor appears at the end rather than
// silently landing in the middle of the scale.
func EffortRank(effort string) int {
	name := strings.ToLower(strings.TrimSpace(effort))
	if alias, ok := effortAlias[name]; ok {
		name = alias
	}
	if r, ok := effortRank[name]; ok {
		return r
	}
	return len(effortOrder)
}

// EffortKeySQL normalises an effort column the way EffortRank reads it, or
// "XHigh" and " high" group apart from the levels they are.
func EffortKeySQL(col string) string { return "lower(trim(" + col + "))" }

// EffortOrderSQL renders the ranking as a SQL CASE expression over col, so the
// ordering has one definition rather than a hand-kept SQL copy that drifts.
// The column is lowered and trimmed as EffortRank does, or the two orderings
// disagree on a row written "XHigh".
func EffortOrderSQL(col string) string {
	expr := EffortKeySQL(col)

	var b strings.Builder
	b.WriteString("CASE " + expr)
	for i, e := range effortOrder {
		b.WriteString(" WHEN '" + e + "' THEN ")
		b.WriteString(strconv.Itoa(i))
	}
	// Sorted so the generated SQL is byte-identical run to run: map order is
	// random, which defeats statement caching and makes the output untestable
	// by equality.
	aliases := make([]string, 0, len(effortAlias))
	for alias := range effortAlias {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		b.WriteString(" WHEN '" + alias + "' THEN ")
		b.WriteString(strconv.Itoa(EffortRank(effortAlias[alias])))
	}
	b.WriteString(" ELSE ")
	b.WriteString(strconv.Itoa(len(effortOrder)))
	b.WriteString(" END")
	return b.String()
}
