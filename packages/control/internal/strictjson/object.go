package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
)

// CheckKeys rejects duplicate object keys at every nested depth.
func CheckKeys(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate or malformed JSON field")
			}
			seen[name] = true
			if err := CheckKeys(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := CheckKeys(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("duplicate or malformed JSON field")
	}
	_, err = d.Token()
	return err
}

// FieldString decodes an exact JSON string field without changing its value.
func FieldString(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := fields[key]
	if !ok {
		return "", false
	}
	value, err := String(raw)
	return value, err == nil
}

// ObjectField decodes an object-valued field. Missing, null and non-object
// values cannot provide an association and report false.
func ObjectField(fields map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, ok := fields[key]
	if !ok {
		return nil, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return nil, false
	}
	return object, true
}
