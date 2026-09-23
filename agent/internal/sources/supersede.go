package sources

import (
	"slices"
	"strings"

	"github.com/samiashi/llm-tracker/schema"
)

// Superseded returns the ids of one source's stored events that a row keyed
// the current way replaces: the same record under an id an older collector
// derived. An old row is named only when its replacement is present, so a
// record since removed from disk keeps the only row it has.
//
// The collector calls it after the re-read that follows an upgrade listed in
// dedupeOnUpgrade. The server's retire and refuse triggers apply the same
// rules to its own copy.
func Superseded(src schema.Source, events []schema.Event) []string {
	switch src {
	case schema.SourceCline, schema.SourceRooCode:
		return supersededCline(events)
	case schema.SourceContinue:
		return supersededContinue(events)
	}
	return nil
}

// supersededCline pairs a request keyed <task>#<array index>, before
// collector 5, with the <task>#<ms>#<n> row for the same task, time and usage.
func supersededCline(events []schema.Event) []string {
	type record struct {
		task  string
		ms    int64
		usage schema.Usage
	}
	key := func(e schema.Event) record { return record{e.SessionID, e.TS.UnixMilli(), e.Usage} }

	current := map[record]bool{}
	for _, e := range events {
		if strings.Count(e.NativeID, "#") == 2 {
			current[key(e)] = true
		}
	}
	var out []string
	for _, e := range events {
		if strings.Count(e.NativeID, "#") == 1 && current[key(e)] {
			out = append(out, e.ID)
		}
	}
	return out
}

// supersededContinue pairs each of Continue's older keys with the row that
// replaces it, telling keys apart by shape and by the collector that wrote
// them:
//
//   - before collector 5, <absolute path>#<time>#<model>, replaced by any
//     later row from the same machine with the same time, model and counts;
//   - collectors 5 to 8, tokensGenerated.jsonl#<offset>, replaced by a later
//     row from the same machine with the same model and counts, keyed on the
//     same offset under a directory (".../tokensGenerated.jsonl#<offset>") or
//     a machine ("<machine>:tokensGenerated.jsonl#<offset>"). Time is not
//     compared: an undated record takes the file's changing mtime;
//   - collector 9, <path under dev_data>#<offset>, replaced by the row
//     <machine>:<the same> from the same machine. A file directly under
//     dev_data has no directory in its key, which is why the collector, not
//     the shape, separates it from a 5-to-8 key: taken for one, the current
//     row of a root-level file would be deleted.
//
// Collector 10 keys <machine>:<path under dev_data>#<offset>.
func supersededContinue(events []schema.Event) []string {
	type record struct {
		machine, model string
		in, out        int64
	}
	key := func(e schema.Event) record {
		return record{e.MachineID, e.Model, e.Usage.InputTokens, e.Usage.OutputTokens}
	}
	type row struct{ machine, native string }

	later := map[record][]schema.Event{}
	keyed := map[row]bool{}
	for _, e := range events {
		keyed[row{e.MachineID, e.NativeID}] = true
		if !strings.HasPrefix(e.NativeID, "/") {
			later[key(e)] = append(later[key(e)], e)
		}
	}
	replaced := func(e schema.Event, by func(n schema.Event) bool) bool {
		return slices.ContainsFunc(later[key(e)], by)
	}

	var out []string
	for _, e := range events {
		var gone bool
		switch {
		case strings.HasPrefix(e.NativeID, "/"):
			gone = replaced(e, func(n schema.Event) bool { return n.TS.Equal(e.TS) })
		case e.Collector < 9:
			gone = replaced(e, func(n schema.Event) bool {
				return strings.HasSuffix(n.NativeID, "/"+e.NativeID) ||
					strings.HasSuffix(n.NativeID, ":"+e.NativeID)
			})
		case e.Collector == 9:
			gone = keyed[row{e.MachineID, e.MachineID + ":" + e.NativeID}]
		}
		if gone {
			out = append(out, e.ID)
		}
	}
	return out
}
