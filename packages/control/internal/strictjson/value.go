// Package strictjson validates JSON values without changing integer literals
// or replacing malformed UTF-16 string escapes.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValidText reports whether s satisfies the byte, UTF-8 and NUL constraints.
func ValidText(s string, max uint64, empty bool) bool {
	return (empty || len(s) > 0) && uint64(len(s)) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// ValidObject reports whether raw is a bounded UTF-8 JSON object.
func ValidObject(raw json.RawMessage, max uint64) bool {
	b := bytes.TrimSpace(raw)
	return uint64(len(raw)) <= max && len(b) > 0 && b[0] == '{' && utf8.Valid(raw) && json.Valid(raw)
}

// Array decodes a JSON array while preserving its element literals.
func Array(raw json.RawMessage) ([]json.RawMessage, error) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || b[0] != '[' {
		return nil, errors.New("not a JSON array")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// Integer accepts only an unquoted JSON integer literal. Fractional,
// exponent, quoted and non-number forms are rejected without a float64 roundtrip.
func Integer(raw json.RawMessage) (int, error) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || (b[0] != '-' && (b[0] < '0' || b[0] > '9')) {
		return 0, errors.New("not a JSON number")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return 0, err
	}
	v, err := n.Int64()
	if err != nil {
		return 0, err
	}
	if int64(int(v)) != v {
		return 0, errors.New("integer out of range")
	}
	return int(v), nil
}

// String decodes one JSON string literal and rejects unpaired
// UTF-16 surrogate escapes. encoding/json would silently replace them with
// U+FFFD, which would rename the caller's target; a literal U+FFFD escape is
// accepted because it names that character exactly.
func String(raw json.RawMessage) (string, error) {
	b := bytes.TrimSpace(raw)
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return "", errors.New("not a JSON string")
	}
	end := len(b) - 1
	var out strings.Builder
	out.Grow(end - 1)
	for i := 1; i < end; {
		c := b[i]
		if c != '\\' {
			out.WriteByte(c)
			i++
			continue
		}
		if i+1 >= end {
			return "", errors.New("truncated escape")
		}
		switch b[i+1] {
		case '"', '\\', '/':
			out.WriteByte(b[i+1])
			i += 2
		case 'b':
			out.WriteByte('\b')
			i += 2
		case 'f':
			out.WriteByte('\f')
			i += 2
		case 'n':
			out.WriteByte('\n')
			i += 2
		case 'r':
			out.WriteByte('\r')
			i += 2
		case 't':
			out.WriteByte('\t')
			i += 2
		case 'u':
			unit, next, err := hexUnit(b, i, end)
			if err != nil {
				return "", err
			}
			i = next
			if unit >= 0xD800 && unit <= 0xDBFF {
				if i+1 >= end || b[i] != '\\' || b[i+1] != 'u' {
					return "", errors.New("unpaired UTF-16 surrogate")
				}
				low, next, err := hexUnit(b, i, end)
				if err != nil || low < 0xDC00 || low > 0xDFFF {
					return "", errors.New("unpaired UTF-16 surrogate")
				}
				i = next
				out.WriteRune(0x10000 + (rune(unit)-0xD800)<<10 + (rune(low) - 0xDC00))
			} else if unit >= 0xDC00 && unit <= 0xDFFF {
				return "", errors.New("unpaired UTF-16 surrogate")
			} else {
				out.WriteRune(rune(unit))
			}
		default:
			return "", errors.New("invalid escape")
		}
	}
	return out.String(), nil
}

// hexUnit decodes the \uXXXX escape starting at b[start] and returns
// the UTF-16 code unit plus the index just past the escape.
func hexUnit(b []byte, start, end int) (uint64, int, error) {
	if start+6 > end {
		return 0, 0, errors.New("truncated unicode escape")
	}
	unit, err := strconv.ParseUint(string(b[start+2:start+6]), 16, 16)
	if err != nil {
		return 0, 0, errors.New("invalid unicode escape")
	}
	return unit, start + 6, nil
}
