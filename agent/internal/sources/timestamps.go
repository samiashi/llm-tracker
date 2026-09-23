package sources

import (
	"os"
	"time"
)

// parseTS accepts the RFC3339 timestamps every harness here emits.
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// fileMTime is the fallback timestamp for records a harness did not date.
// The zero time would park them before every window the dashboard asks for,
// which reads as "no usage" rather than "usage we could not date".
func fileMTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime().UTC()
}

// unixOrZero converts a unix timestamp, treating 0 as absent.
func unixOrZero(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// pickTime takes whichever timestamp form a record carries: harnesses disagree
// on RFC3339 strings versus unix seconds, sometimes within one format.
func pickTime(rfc3339 string, unixSec int64) time.Time {
	if t := parseTS(rfc3339); !t.IsZero() {
		return t
	}
	return unixOrZero(unixSec)
}

// unixMilliOrZero converts Cline's millisecond timestamps, tolerating the
// zero that a malformed entry yields rather than dating it to 1970.
func unixMilliOrZero(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
