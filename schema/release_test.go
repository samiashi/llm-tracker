package schema

import "testing"

// Only an exact vX.Y.Z is a release. A build from a working tree past a tag
// reports v1.4.0-3-g6387414-dirty, which must not pass for v1.4.0, or upgrade
// and the stale-agent check take an unreleased build for that release.
func TestOnlyAnExactTagIsARelease(t *testing.T) {
	for v, want := range map[string][3]int{
		"v1.4.0":  {1, 4, 0},
		"1.4.0":   {1, 4, 0},
		"v1.10.2": {1, 10, 2},
	} {
		if got, ok := ReleaseVersion(v); !ok || got != want {
			t.Errorf("ReleaseVersion(%q) = %v, %v; want %v, true", v, got, ok, want)
		}
	}
	for _, v := range []string{
		"v1.4", "v1.4.0.1", "v1.4.0-rc1", "v1.4.0-3-g6387414-dirty", "6387414-dirty",
		"dev", "", "v1.-4.0", "v1.+4.0", " v1.4.0", "vv1.4.0",
	} {
		if _, ok := ReleaseVersion(v); ok {
			t.Errorf("ReleaseVersion(%q) is a release; only an exact vX.Y.Z is", v)
		}
	}
}
