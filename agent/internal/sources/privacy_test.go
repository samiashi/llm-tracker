package sources

import (
	"encoding"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// No prompt, completion, source code or tool argument can leave a machine,
// because no wire type has a field to hold one. The checks read the types by
// reflection, not a marshalled instance: an unset omitempty field is absent
// from the JSON, so a new Title or Summary field would pass unseen.

// wireRoots are the two things the agent sends: an upload and an enrolment.
// Every type either can reach is on the wire. One marshalled from an
// agent-local copy instead escapes this test, which is why the upload's
// shadowing wireBatch is pinned to schema.Batch (TestWireBatchIsSchemaBatch).
var wireRoots = []reflect.Type{reflect.TypeFor[schema.Batch](), reflect.TypeFor[schema.EnrollRequest]()}

// wireKeys is, for each wire type, every key it may emit. A key is listed
// because somebody decided it cannot hold message text -- adding one is the
// deliberate act this test exists to force -- and only for its own type: a
// "note" cleared for UnknownSource's findings is not cleared for Event.
var wireKeys = map[reflect.Type][]string{
	reflect.TypeFor[schema.Batch](): {"v", "machine_id", "hostname", "agent_version",
		"events", "unknown_sources", "unknown_complete", "accounts"},
	reflect.TypeFor[schema.Event](): {"v", "id", "native_id", "source", "surface", "ts",
		"machine_id", "account_ref", "provider", "model", "endpoint", "usage",
		"inference_geo", "speed", "effort", "cost_basis", "native_cost_usd",
		"session_id", "is_subagent", "agent_version",
		// The collector version that read the event: an integer.
		"collector"},
	reflect.TypeFor[schema.Usage](): {"input_tokens", "output_tokens", "cache_read_tokens",
		"cache_write_5m_tokens", "cache_write_1h_tokens", "reasoning_tokens",
		"web_search_calls", "web_fetch_calls"},
	// A harness directory found on the machine, and the finding recorded
	// against it by us, never by the user.
	reflect.TypeFor[schema.UnknownSource](): {"v", "machine_id", "path", "hint",
		"size_bytes", "first_seen", "status", "note"},
	// The email is personal data, carried deliberately so usage can be
	// attributed to a colleague; it is an address, never content.
	reflect.TypeFor[schema.Account]():       {"ref", "provider", "email", "plan_type"},
	reflect.TypeFor[schema.EnrollRequest](): {"hostname"},
}

// wireField is one key a struct puts on the wire, and the type behind it.
type wireField struct {
	key string
	typ reflect.Type
}

// wireFields lists the keys a struct type can emit, including the ones it
// omits when empty, as encoding/json names them: an embedded struct's fields
// are promoted into it, and unexported fields and "-" never reach the wire.
func wireFields(rt reflect.Type) []wireField {
	var out []wireField
	for i := range rt.NumField() {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Anonymous && name == "" && ft.Kind() == reflect.Struct {
			out = append(out, wireFields(ft)...)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, wireField{name, f.Type})
	}
	return out
}

var (
	timeType      = reflect.TypeFor[time.Time]()
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

func TestNoWireTypeCanCarryContent(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(rt reflect.Type, where string)
	walk = func(rt reflect.Type, where string) {
		if rt == timeType {
			return
		}
		// Anything that writes its own JSON -- json.RawMessage above all --
		// can put whatever it likes past the key list.
		if rt.Implements(jsonMarshaler) || reflect.PointerTo(rt).Implements(jsonMarshaler) ||
			rt.Implements(textMarshaler) || reflect.PointerTo(rt).Implements(textMarshaler) {
			t.Errorf("%s is %s, which writes its own JSON and can carry anything", where, rt)
			return
		}
		switch rt.Kind() {
		case reflect.Bool, reflect.String,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(rt.Elem(), where)
		case reflect.Struct:
			if seen[rt] {
				return
			}
			seen[rt] = true
			allowed, ok := wireKeys[rt]
			if !ok {
				t.Errorf("%s reaches the wire as %s, which has no list of keys here: "+
					"add one, naming only keys that cannot hold message text", where, rt)
			}
			var keys []string
			for _, f := range wireFields(rt) {
				keys = append(keys, f.key)
				if ok && !slices.Contains(allowed, f.key) {
					t.Errorf("unexpected field %q on %s: if it can hold message text it "+
						"must not exist; if it cannot, list it for %s deliberately", f.key, rt, rt.Name())
				}
				walk(f.typ, where+"."+f.key)
			}
			for _, k := range allowed {
				if !slices.Contains(keys, k) {
					t.Errorf("%s no longer emits %q: drop it from the list, or it lets the "+
						"field come back unreviewed", rt, k)
				}
			}
		default:
			// Interfaces and maps above all: keys nobody listed.
			t.Errorf("%s is a %s, which can carry anything", where, rt.Kind())
		}
	}
	for _, root := range wireRoots {
		walk(root, root.Name())
	}
	for rt := range wireKeys {
		if !seen[rt] {
			t.Errorf("%s has a list of keys but no wire type reaches it", rt)
		}
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
