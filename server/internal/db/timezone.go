package db

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // Named zones must work even when the host has no zoneinfo files.
)

// hourSQL generates only numeric literals from trusted timezone data, never
// request text. Each DST interval gets its own offset. A fixed midpoint offset
// incorrectly moves every event on one side of a transition by an hour.
// absolute keeps distinct UTC bucket ids for repeated wall-clock hours; the
// heatmap deliberately combines repeated hours into its 24-column day grid.
func (w Window) hourSQL(loc *time.Location, absolute bool) string {
	w = w.Normalise()
	start, end := dayStartUnix(w.From), dayStartUnix(w.To)+86400
	if w.End > 0 {
		start, end = w.Start, w.End
	}
	if loc == nil {
		loc = time.UTC
	}
	var clauses []string
	for at := start; at < end; {
		t := time.Unix(at, 0).In(loc)
		_, offset := t.Zone()
		begins, ends := t.ZoneBounds()
		expr := fmt.Sprintf("((ts + %d) / 3600)", offset)
		if absolute {
			expr = fmt.Sprintf("(%s * 3600 - %d)", expr, offset)
			// A half-hour DST jump can begin partway through a clock hour.
			if !begins.IsZero() {
				expr = fmt.Sprintf("MAX(%s, %d)", expr, begins.Unix())
			}
		}
		if ends.IsZero() || ends.Unix() >= end {
			if len(clauses) == 0 {
				return expr
			}
			return "CASE " + strings.Join(clauses, " ") + " ELSE " + expr + " END"
		}
		clauses = append(clauses, fmt.Sprintf("WHEN ts < %d THEN %s", ends.Unix(), expr))
		at = ends.Unix()
	}
	return "ts / 3600"
}
