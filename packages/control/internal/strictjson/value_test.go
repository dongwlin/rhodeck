package strictjson

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"
)

func TestRawValuesRetainUnknownFieldsAndIntegerLiterals(t *testing.T) {
	const source = ` {"future":{"large":9007199254740993},"items":[9007199254740993,{"unknown":-9223372036854775808}]} `
	raw := json.RawMessage(source)
	if !ValidObject(raw, uint64(len(raw))) || ValidObject(raw, uint64(len(raw)-1)) {
		t.Fatal("object byte boundary was not enforced")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	items, err := Array(fields["items"])
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || string(items[0]) != "9007199254740993" || string(items[1]) != `{"unknown":-9223372036854775808}` {
		t.Fatalf("raw array changed number or unknown field literals: %s", fields["items"])
	}
	if !bytes.Equal(raw, []byte(source)) {
		t.Fatal("validation mutated the caller's input")
	}
	for _, invalid := range []string{`null`, `[]`, `{"a":1} {}`, "{\"a\":\"\xff\"}", `{`} {
		if ValidObject(json.RawMessage(invalid), 1024) {
			t.Fatalf("invalid object accepted: %q", invalid)
		}
	}
	for _, invalid := range []string{`null`, `{}`, `[1`, `"[]"`} {
		if _, err := Array(json.RawMessage(invalid)); err == nil {
			t.Fatalf("invalid array accepted: %q", invalid)
		}
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
		if err != nil || got != want {
			t.Fatalf("Integer(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{`1.0`, `1e3`, `"1"`, `null`, `true`, `[]`, `9223372036854775808`, `-9223372036854775809`} {
		if _, err := Integer(json.RawMessage(raw)); err == nil {
			t.Fatalf("non-integer or overflowing literal accepted: %s", raw)
		}
	}
}

func TestStringPreservesNamesAndRejectsUnpairedSurrogates(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{`"\uD83D\uDE80"`, "🚀"}, {`"\ufffd"`, "�"}, {`" Alpha / β "`, " Alpha / β "},
		{`"quote\" slash\\ line\n"`, "quote\" slash\\ line\n"},
	} {
		got, err := String(json.RawMessage(test.raw))
		if err != nil || got != test.want {
			t.Fatalf("String(%s) = %q, %v; want %q", test.raw, got, err, test.want)
		}
	}
	for _, raw := range []string{`"\uD800"`, `"\uDC00"`, `"\uD800x"`, `"\uD800\u0041"`, `"\u12"`, `null`, `1`, `"\q"`} {
		if _, err := String(json.RawMessage(raw)); err == nil {
			t.Fatalf("malformed string accepted: %s", raw)
		}
	}
	for _, test := range []struct {
		value       string
		max         uint64
		empty, want bool
	}{
		{"甲", 3, false, true}, {"甲", 2, false, false}, {"", 0, true, true},
		{"", 5, false, false}, {"a\x00b", 3, false, false}, {string([]byte{0xff}), 1, false, false},
	} {
		if got := ValidText(test.value, test.max, test.empty); got != test.want {
			t.Fatalf("ValidText(%q, %d, %v) = %v", test.value, test.max, test.empty, got)
		}
	}
}
