package db

import (
	"time"
)

// Window is an inclusive day range plus an optional person filter.
//
// Person is an email rather than an account reference, because one colleague
// holds several logins and drilling into "them" means all of them.
type Window struct {
	From, To, Person string
	// Start and End select a half-open timestamp range [Start, End). Only the
	// rolling 24h view uses this exception to the day-rollup rule.
	Start, End int64
	// Zone controls hourly display buckets only; day windows and storage stay UTC.
	Zone *time.Location
}

// Normalise fills in the default window, the last 30 UTC days. A caller that
// reports the range it served, like the CSV filename, settles it first.
func (w Window) Normalise() Window {
	if w.End > 0 {
		w.From = time.Unix(w.Start, 0).UTC().Format(time.DateOnly)
		w.To = time.Unix(w.End-1, 0).UTC().Format(time.DateOnly)
		return w
	}
	if w.To == "" {
		w.To = time.Now().UTC().Format("2006-01-02")
	}
	if w.From == "" {
		w.From = time.Now().UTC().AddDate(0, 0, -29).Format("2006-01-02")
	}
	return w
}

// unknownKey is the key a breakdown gives rows with no value for its
// dimension. By person, those are the events of no account, and the dashboard
// feeds the key back as the filter that selects them.
const unknownKey = "unknown"

// where builds the shared filter clause and its arguments.
func (w Window) where(col string) (string, []any) {
	clause := col + "day BETWEEN ? AND ?"
	args := []any{w.From, w.To}
	if w.End > 0 {
		clause += " AND " + col + "ts >= ? AND " + col + "ts < ?"
		args = append(args, w.Start, w.End)
	}
	if w.Person != "" {
		clause += " AND " + col + "account_ref IN " + personRefs
		args = append(args, personArgs(w.Person)...)
	}
	return clause, args
}

// personRefs resolves a person to account refs through a subquery rather than
// a join, so every query applies it the same way. One IN list, so the planner
// ranges over (account_ref, day) per ref instead of reading the person's whole
// history. The key is a ref too: ByPerson keys an account with no email by it.
const personRefs = "(SELECT ref FROM account WHERE email = ? UNION ALL SELECT ?)"

// personArgs binds personRefs for a person key.
func personArgs(person string) []any {
	ref := person
	if person == unknownKey {
		ref = ""
	}
	return []any{person, ref}
}

// listLen is a list's length when a request names none, and the most one may
// name. The handlers pass 0 for an absent parameter, so each default and cap
// is written once, here.
type listLen struct{ def, max int }

func (l listLen) of(n int) int {
	if n <= 0 {
		return l.def
	}
	return min(n, l.max)
}

// relation preserves the aggregate column contract for timestamp windows.
// Whole-day queries must keep using event_daily so retention is invisible.
// A raw event counts as one response; cost and token fields remain one reading.
func (w Window) relation() string {
	if w.End > 0 {
		return "(SELECT *, 1 AS events FROM event)"
	}
	return "event_daily"
}

// bucket returns UTC instant keys for viewer-local hours in timestamp windows. The first
// and last bucket may be partial; where still clips events to the exact bounds.
func (w Window) bucket() string {
	if w.End > 0 {
		return "strftime('%Y-%m-%dT%H:%M:%SZ', " + w.hourSQL(w.Zone, true) + ", 'unixepoch')"
	}
	return "day"
}
