package schema

import (
	"strconv"
	"strings"
	"testing"
)

func TestUltraIsTheTop(t *testing.T) {
	for _, lower := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		if EffortRank(lower) >= EffortRank("ultra") {
			t.Errorf("%s must rank below ultra", lower)
		}
	}
}

func TestUltracodeRanksAsXhigh(t *testing.T) {
	if EffortRank("ultracode") != EffortRank("xhigh") {
		t.Errorf("ultracode ranks %d, xhigh ranks %d -- they are the same effort",
			EffortRank("ultracode"), EffortRank("xhigh"))
	}
}

func TestEffortRankFollowsIntensityNotAlphabet(t *testing.T) {
	if EffortRank("medium") >= EffortRank("max") {
		t.Error("medium must rank below max")
	}
	if EffortRank("xhigh") <= EffortRank("high") {
		t.Error("xhigh must rank above high")
	}
	if EffortRank("minimal") <= EffortRank("default") {
		t.Errorf("minimal should rank above default, got %d vs %d",
			EffortRank("minimal"), EffortRank("default"))
	}
	if EffortRank("default") != 0 {
		t.Errorf("default should be the lowest, got rank %d", EffortRank("default"))
	}
}

// opencode writes its reasoning level as a model "variant"; the names it uses
// are already on this scale.
func TestOpenCodeVariantsAreKnownLevels(t *testing.T) {
	for _, v := range []string{"max", "xhigh", "default"} {
		if EffortRank(v) >= len(EffortOrder) {
			t.Errorf("opencode variant %q is not on the scale", v)
		}
	}
	if EffortRank("default") >= EffortRank("xhigh") {
		t.Error("default must rank below an explicitly chosen level")
	}
}

func TestEffortRankIsMonotonic(t *testing.T) {
	for i := 1; i < len(EffortOrder); i++ {
		if EffortRank(EffortOrder[i-1]) >= EffortRank(EffortOrder[i]) {
			t.Fatalf("%s must rank below %s", EffortOrder[i-1], EffortOrder[i])
		}
	}
}

func TestUnknownEffortSortsLast(t *testing.T) {
	if EffortRank("something-new") <= EffortRank("ultra") {
		t.Error("an unknown level must sort after every known one")
	}
	if EffortRank("") <= EffortRank("ultra") {
		t.Error("an empty level must sort last")
	}
}

func TestEffortRankIsCaseAndSpaceInsensitive(t *testing.T) {
	if EffortRank("  XHigh ") != EffortRank("xhigh") {
		t.Error("rank should normalise case and surrounding space")
	}
}

func TestEffortOrderSQLCoversEveryLevel(t *testing.T) {
	sql := EffortOrderSQL("effort")
	for _, e := range EffortOrder {
		if !contains(sql, "'"+e+"'") {
			t.Errorf("generated SQL omits %q", e)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// A rank rendered as one rune ('0'+i) is ":" at ten levels, a named-parameter
// sigil in SQLite.
func TestEffortOrderSQLStaysValidBeyondNineLevels(t *testing.T) {
	saved := EffortOrder
	t.Cleanup(func() { EffortOrder = saved; effortRank = buildEffortRank() })

	EffortOrder = append(append([]string{}, saved...), "hyper", "omega", "singularity")
	effortRank = buildEffortRank()

	got := EffortOrderSQL("event.effort")
	for _, bad := range []string{"THEN :", "ELSE :", "THEN ;", "ELSE ;", "THEN <", "ELSE <"} {
		if strings.Contains(got, bad) {
			t.Fatalf("generated invalid SQL %q in: %s", bad, got)
		}
	}
	want := "ELSE " + strconv.Itoa(len(EffortOrder))
	if !strings.Contains(got, want) {
		t.Fatalf("missing %q in: %s", want, got)
	}
}

func TestEffortOrderSQLNormalisesLikeEffortRank(t *testing.T) {
	got := EffortOrderSQL("event.effort")
	if !strings.Contains(got, "lower(trim(event.effort))") {
		t.Fatalf("the CASE does not normalise its column: %s", got)
	}
}

func TestEffortOrderSQLIsDeterministic(t *testing.T) {
	first := EffortOrderSQL("event.effort")
	for range 20 {
		if got := EffortOrderSQL("event.effort"); got != first {
			t.Fatalf("generated SQL varies between calls:\n%s\n%s", first, got)
		}
	}
}
