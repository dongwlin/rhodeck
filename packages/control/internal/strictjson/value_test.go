package strictjson

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRawValuesRetainUnknownFieldsAndIntegerLiterals(t *testing.T) {
	const source = ` {"future":{"large":9007199254740993},"items":[9007199254740993,{"unknown":-9223372036854775808}]} `
	raw := json.RawMessage(source)
	require.True(t, ValidObject(raw, uint64(len(raw))), "object byte boundary was not enforced")
	require.False(t, ValidObject(raw, uint64(len(raw)-1)), "object byte boundary was not enforced")
	var fields map[string]json.RawMessage
	err := json.Unmarshal(raw, &fields)
	require.NoError(t, err)
	items, err := Array(fields["items"])
	require.NoError(t, err)
	require.Len(t, items, 2, "raw array changed number or unknown field literals: %s", fields["items"])
	require.Equal(t, "9007199254740993", string(items[0]), "raw array changed number or unknown field literals: %s", fields["items"])
	require.Equal(t, `{"unknown":-9223372036854775808}`, string(items[1]), "raw array changed number or unknown field literals: %s", fields["items"])
	require.True(t, bytes.Equal(raw, []byte(source)), "validation mutated the caller's input")
	for _, invalid := range []string{`null`, `[]`, `{"a":1} {}`, "{\"a\":\"\xff\"}", `{`} {
		require.False(t, ValidObject(json.RawMessage(invalid), 1024), "invalid object accepted: %q", invalid)
	}
	for _, invalid := range []string{`null`, `{}`, `[1`, `"[]"`} {
		_, err := Array(json.RawMessage(invalid))
		require.Error(t, err, "invalid array accepted: %q", invalid)
	}
}

func TestIntegerAcceptsExactInRangeLiterals(t *testing.T) {
	values := []int{0, 1, -1, int(^uint(0) >> 1), -int(^uint(0)>>1) - 1}
	if strconv.IntSize == 64 {
		exact := int64(9007199254740993)
		values = append(values, int(exact))
	}
	for _, want := range values {
		raw := " \n" + strconv.FormatInt(int64(want), 10) + "\t"
		got, err := Integer(json.RawMessage(raw))
		require.NoError(t, err, "Integer(%q) = %d, %v; want %d", raw, got, err, want)
		require.Equal(t, want, got, "Integer(%q) = %d, %v; want %d", raw, got, err, want)
	}
	for _, raw := range []string{`1.0`, `1e3`, `"1"`, `null`, `true`, `[]`, `9223372036854775808`, `-9223372036854775809`} {
		_, err := Integer(json.RawMessage(raw))
		require.Error(t, err, "non-integer or overflowing literal accepted: %s", raw)
	}
}

func TestStringPreservesNamesAndRejectsUnpairedSurrogates(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{`"\uD83D\uDE80"`, "🚀"}, {`"\ufffd"`, "�"}, {`" Alpha / β "`, " Alpha / β "},
		{`"quote\" slash\\ line\n"`, "quote\" slash\\ line\n"},
	} {
		got, err := String(json.RawMessage(test.raw))
		require.NoError(t, err, "String(%s) = %q, %v; want %q", test.raw, got, err, test.want)
		require.Equal(t, test.want, got, "String(%s) = %q, %v; want %q", test.raw, got, err, test.want)
	}
	for _, raw := range []string{`"\uD800"`, `"\uDC00"`, `"\uD800x"`, `"\uD800\u0041"`, `"\u12"`, `null`, `1`, `"\q"`} {
		_, err := String(json.RawMessage(raw))
		require.Error(t, err, "malformed string accepted: %s", raw)
	}
	for _, test := range []struct {
		value       string
		max         uint64
		empty, want bool
	}{
		{"甲", 3, false, true}, {"甲", 2, false, false}, {"", 0, true, true},
		{"", 5, false, false}, {"a\x00b", 3, false, false}, {string([]byte{0xff}), 1, false, false},
	} {
		got := ValidText(test.value, test.max, test.empty)
		require.Equal(t, test.want, got, "ValidText(%q, %d, %v) = %v", test.value, test.max, test.empty, got)
	}
}
