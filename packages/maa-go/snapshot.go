package maa

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

func addChecked(a, b uint64) (uint64, bool) {
	sum := a + b
	if sum < a {
		return 0, false
	}
	return sum, true
}

func validateStringField(field, value string, max uint64, allowEmpty bool) error {
	switch {
	case value == "" && !allowEmpty:
		return fmt.Errorf("maa: %s must not be empty: %w", field, native.ErrInvalidInput)
	case !utf8.ValidString(value):
		return fmt.Errorf("maa: %s is not valid UTF-8: %w", field, native.ErrInvalidInput)
	case strings.IndexByte(value, '\x00') >= 0:
		return fmt.Errorf("maa: %s contains an embedded NUL byte: %w", field, native.ErrInvalidInput)
	case uint64(len(value)) > max:
		return fmt.Errorf("maa: %s length %d exceeds Limits.InputStringBytes %d: %w", field, len(value), max, native.ErrInvalidInput)
	}
	return nil
}

func validatePathField(field, value string, max uint64) error {
	if err := validateStringField(field, value, max, false); err != nil {
		return err
	}
	if !filepath.IsAbs(value) {
		return fmt.Errorf("maa: %s %q is not an absolute path: %w", field, value, native.ErrInvalidInput)
	}
	return nil
}

func validateJSONObject(field string, raw json.RawMessage, max uint64) error {
	switch {
	case bytes.IndexByte(raw, 0x00) >= 0:
		return fmt.Errorf("maa: %s contains a literal NUL byte: %w", field, native.ErrInvalidInput)
	case !utf8.Valid(raw):
		return fmt.Errorf("maa: %s is not valid UTF-8: %w", field, native.ErrInvalidInput)
	case uint64(len(raw)) > max:
		return fmt.Errorf("maa: %s length %d exceeds Limits.InputJSONBytes %d: %w", field, len(raw), max, native.ErrInvalidInput)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("maa: %s is not valid JSON (%v): %w", field, err, native.ErrInvalidInput)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("maa: %s must be a JSON object, got %v: %w", field, tok, native.ErrInvalidInput)
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("maa: %s is not a complete JSON object (%v): %w", field, err, native.ErrInvalidInput)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("maa: %s has trailing content after the JSON object: %w", field, native.ErrInvalidInput)
	}
	return nil
}

func cString(s string) (*byte, []byte) {
	buf := make([]byte, len(s)+1)
	copy(buf, s)
	return &buf[0], buf
}
