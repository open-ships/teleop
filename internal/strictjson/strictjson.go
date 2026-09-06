// Package strictjson rejects ambiguous JSON before security-sensitive decoding.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// Validate bounds bytes/depth, requires valid UTF-8 and exactly one JSON value,
// and rejects duplicate (including escaped-equivalent) object field names.
func Validate(data []byte, maximum int) error {
	if len(data) == 0 || len(data) > maximum || !utf8.Valid(data) {
		return errors.New("invalid JSON size or UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := scan(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON content")
	}
	return nil
}

func scan(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON depth exceeded")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return errors.New("invalid delimiter")
	}
	seen := make(map[string]bool)
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("invalid or duplicate JSON field %q", key)
			}
			seen[name] = true
		}
		if err := scan(d, depth+1); err != nil {
			return err
		}
	}
	end, err := d.Token()
	if err != nil {
		return err
	}
	if (delim == '{' && end != json.Delim('}')) || (delim == '[' && end != json.Delim(']')) {
		return errors.New("invalid closing delimiter")
	}
	return nil
}

// Object requires exactly these case-sensitive, non-null fields. Validate must
// be called on the complete enclosing document first.
func Object(data []byte, names ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if len(fields) != len(names) {
		return nil, errors.New("unexpected or missing JSON fields")
	}
	for _, name := range names {
		if value, ok := fields[name]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("missing or null field %q", name)
		}
	}
	return fields, nil
}
