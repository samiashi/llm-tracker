package db

import (
	"time"
)

// Window is an inclusive day range plus an optional person filter.
//
// Person is an email rather than an account reference, because one colleague
// holds several logins and drilling into "them" means all of them.
type Window struct{ From, To, Person string }

// Normalise fills in the default window, the last 30 UTC days. A caller that
// reports the range it served, like the CSV filename, settles it first.
func (w Window) Normalise() Window {
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
