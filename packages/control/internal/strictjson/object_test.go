package strictjson

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckKeysRejectsAmbiguousNestedInput(t *testing.T) {
	for _, test := range []struct {
		raw       string
		wantError bool
	}{
		{`{"params":{"future":9007199254740993},"json":{"future":9223372036854775807}}`, false},
		{`{"a":{"id":1},"b":{"id":2}}`, false},
		{`[{"name":"a"},{"name":"b"}]`, false},
		{`{"taskid":1,"taskid":2}`, true},
		{`{"params":{"value":1,"value":2}}`, true},
		{`{"json":[{"name":"a","name":"b"}]}`, true},
		{`{"name":"a","\u006eame":"b"}`, true},
		{`{"items":[{"ok":true},{"nested":{"id":1,"id":2}}]}`, true},
		{`{"nested":[{`, true},
	} {
		decoder := json.NewDecoder(strings.NewReader(test.raw))
		decoder.UseNumber()
		if err := CheckKeys(decoder); (err != nil) != test.wantError {
			t.Fatalf("CheckKeys(%s) = %v; want error=%v", test.raw, err, test.wantError)
		}
	}
}

func TestAssociationFieldsRequireExactValueKinds(t *testing.T) {
	fields := map[string]json.RawMessage{
		"name":         json.RawMessage(`"\uD83D\uDE80"`),
		"invalid_name": json.RawMessage(`"\uD800"`),
		"null":         json.RawMessage(`null`),
		"array":        json.RawMessage(`[]`),
		"details":      json.RawMessage(`{"id":9007199254740993,"future":{"flag":true}}`),
	}
	if got, ok := FieldString(fields, "name"); !ok || got != "🚀" {
		t.Fatalf("exact name = %q/%v", got, ok)
	}
	for _, key := range []string{"missing", "invalid_name", "null", "details", "array"} {
		if got, ok := FieldString(fields, key); ok || got != "" {
			t.Fatalf("invalid string association %q = %q/%v", key, got, ok)
		}
	}
	object, ok := ObjectField(fields, "details")
	if !ok || string(object["id"]) != "9007199254740993" || string(object["future"]) != `{"flag":true}` {
		t.Fatalf("association object changed: %v/%v", object, ok)
	}
	for _, key := range []string{"missing", "null", "array", "name"} {
		if got, ok := ObjectField(fields, key); ok || got != nil {
			t.Fatalf("invalid object association %q = %v/%v", key, got, ok)
		}
	}
}
