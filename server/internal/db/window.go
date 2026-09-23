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

// where builds the shared filter clause and its arguments.
//
// The person filter resolves through the account table as a subquery rather
// than a join, so every query can apply it the same way without each one
// growing a join it otherwise would not need.
func (w Window) where(col string) (string, []any) {
	clause := col + "day BETWEEN ? AND ?"
	args := []any{w.From, w.To}
	if w.Person != "" {
		// Match the ref directly as well as through the email: ByPerson keys
		// an account with no email by its ref, and the dashboard feeds that
		// key back as the filter, which no email matches.
		clause += " AND (" + col + "account_ref IN (SELECT ref FROM account WHERE email = ?)" +
			" OR " + col + "account_ref = ?)"
		args = append(args, w.Person, w.Person)
	}
	return clause, args
}
