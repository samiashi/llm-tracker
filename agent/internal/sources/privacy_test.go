package sources

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

// No prompt, completion, source code or tool argument can leave a machine,
// because no wire type has a field to hold one. The checks read the types by
// reflection, not a marshalled instance: an unset omitempty field is absent
// from the JSON, so a new Title or Summary field would pass unseen.

// jsonFields returns every JSON key a type can put on the wire, including the
// ones it would omit when empty.
func jsonFields(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}

// wireTypes is every type the agent sends, not just Event: UnknownSource
// carries a path, Batch a hostname and Account an email. A type marshalled
// from an agent-local copy instead of schema escapes this test.
var wireTypes = map[string]any{
	"Event":         schema.Event{},
	"Usage":         schema.Usage{},
	"UnknownSource": schema.UnknownSource{},
	"Batch":         schema.Batch{},
	"Account":       schema.Account{},
	"EnrollRequest": schema.EnrollRequest{},
}

func TestNoWireTypeCanCarryContent(t *testing.T) {
	// Every key any of these types may emit. A field is listed here because
	// somebody decided it cannot hold message text -- adding one is the
	// deliberate act this test exists to force.
	allowed := map[string]bool{
		// Event
		"v": true, "id": true, "native_id": true, "source": true, "surface": true,
		"ts": true, "machine_id": true, "account_ref": true, "provider": true,
		"model": true, "endpoint": true, "usage": true, "cost_basis": true,
		"native_cost_usd": true, "inference_geo": true, "speed": true, "effort": true,
		"session_id": true, "project_path": true, "git_branch": true,
		"is_subagent": true, "agent_version": true,
		// The collector version that read the event: an integer.
		"collector": true,
		// Usage
		"input_tokens": true, "output_tokens": true, "cache_read_tokens": true,
		"cache_write_5m_tokens": true, "cache_write_1h_tokens": true,
		"reasoning_tokens": true, "web_search_calls": true, "web_fetch_calls": true,
		// UnknownSource
		"path": true, "hint": true, "size_bytes": true, "first_seen": true,
		"status": true, "note": true,
		// Batch
		"hostname": true, "events": true,
		"unknown_sources": true, "unknown_complete": true, "accounts": true,
		// Account. The email is personal data, carried deliberately so usage
		// can be attributed to a colleague; it is an address, never content.
		"ref": true, "email": true, "plan_type": true,
	}

	for name, zero := range wireTypes {
		t.Run(name, func(t *testing.T) {
			rt := reflect.TypeOf(zero)

			// A field the type can emit but nobody has named.
			for _, key := range jsonFields(rt) {
				if !allowed[key] {
					t.Errorf("unexpected field %q on the wire: if it can hold message "+
						"text it must not exist; if it cannot, add it to this list "+
						"deliberately", key)
				}
			}

			// No escape hatch that could smuggle arbitrary JSON past the
			// field list.
			for i := range rt.NumField() {
				f := rt.Field(i)
				switch f.Type.Kind() {
				case reflect.Interface, reflect.Map:
					t.Errorf("%s.%s is a %s, which can carry anything",
						name, f.Name, f.Type.Kind())
				}
				if f.Type == reflect.TypeOf(json.RawMessage(nil)) {
					t.Errorf("%s.%s is json.RawMessage, which can carry anything",
						name, f.Name)
				}
			}

		})
	}
}

func TestUsageIsNumericOnly(t *testing.T) {
	b, err := json.Marshal(schema.Usage{InputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	for k, v := range wire {
		if _, ok := v.(float64); !ok {
			t.Errorf("usage field %q is %T, not a number", k, v)
		}
	}
}
