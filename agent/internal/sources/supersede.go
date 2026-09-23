package sources

import (
	"strings"

	"github.com/samiashi/llm-tracker/schema"
)

// Superseded returns the ids of one source's stored events that a row keyed
// the current way replaces: the same record under an id an older collector
// derived. An old row is named only when its replacement is present, so a
// record since removed from disk keeps the only row it has.
//
// The collector calls it after the re-read that follows an upgrade listed in
// dedupeOnUpgrade. Server migration 00016 applies the same rules.
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

// supersededContinue pairs Continue's two older keys with the current one,
// <path under dev_data>#<offset>, for a record with the same model and usage.
// Before collector 5 a record was <absolute path>#<time>#<model>, which any
// later row at the same time replaces. Until collector 9 it was
// tokensGenerated.jsonl#<offset>, which the current row for the same offset
// replaces; time is not compared there, since an undated record takes the
// file's changing mtime. A file directly under dev_data keeps that form.
func supersededContinue(events []schema.Event) []string {
	type record struct {
		model string
		usage schema.Usage
	}
	key := func(e schema.Event) record { return record{e.Model, e.Usage} }

	later := map[record][]schema.Event{}
	for _, e := range events {
		if !strings.HasPrefix(e.NativeID, "/") {
			later[key(e)] = append(later[key(e)], e)
		}
	}
	var out []string
	for _, e := range events {
		pathKeyed := strings.HasPrefix(e.NativeID, "/")
		if !pathKeyed && strings.Contains(e.NativeID, "/") {
			continue // current
		}
		for _, n := range later[key(e)] {
			if (pathKeyed && n.TS.Equal(e.TS)) ||
				(!pathKeyed && strings.HasSuffix(n.NativeID, "/"+e.NativeID)) {
				out = append(out, e.ID)
				break
			}
		}
	}
	return out
}
