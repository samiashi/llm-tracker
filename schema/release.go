package schema

import (
	"strconv"
	"strings"
)

// ReleaseVersion parses a vX.Y.Z release tag, the only form a release has.
// A build from a working tree reports "<sha>-dirty" or "dev", which is not
// something to upgrade to or compare against.
func ReleaseVersion(v string) ([3]int, bool) {
	var r [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != len(r) {
		return r, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || strings.Trim(p, "0123456789") != "" {
			return r, false
		}
		r[i] = n
	}
	return r, true
}
