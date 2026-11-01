package strictjson

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
		err := CheckKeys(decoder)
		require.Equal(t, test.wantError, err != nil, "CheckKeys(%s) = %v; want error=%v", test.raw, err, test.wantError)
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
	got, ok := FieldString(fields, "name")
	require.True(t, ok, "exact name = %q/%v", got, ok)
	require.Equal(t, "🚀", got, "exact name = %q/%v", got, ok)
	for _, key := range []string{"missing", "invalid_name", "null", "details", "array"} {
		got, ok := FieldString(fields, key)
		require.False(t, ok, "invalid string association %q = %q/%v", key, got, ok)
		require.Empty(t, got, "invalid string association %q = %q/%v", key, got, ok)
	}
	object, ok := ObjectField(fields, "details")
	require.True(t, ok, "association object changed: %v/%v", object, ok)
	require.Equal(t, "9007199254740993", string(object["id"]), "association object changed: %v/%v", object, ok)
	require.Equal(t, `{"flag":true}`, string(object["future"]), "association object changed: %v/%v", object, ok)
	for _, key := range []string{"missing", "null", "array", "name"} {
		got, ok := ObjectField(fields, key)
		require.False(t, ok, "invalid object association %q = %v/%v", key, got, ok)
		require.Nil(t, got, "invalid object association %q = %v/%v", key, got, ok)
	}
}
