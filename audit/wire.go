package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// decodeDiskRecord applies the wire schema before decoding into diskRecord.
// Cryptographic verification must never accept a field that the selected
// format version does not authenticate.
func decodeDiskRecord(encoded []byte) (diskRecord, error) {
	var record diskRecord
	if !utf8.Valid(encoded) {
		return record, errors.New("audit record is not valid UTF-8")
	}
	if err := rejectDuplicateJSONFields(encoded); err != nil {
		return record, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return record, err
	}
	rawVersion, ok := fields["version"]
	if !ok {
		return record, errors.New(`audit record field "version" is required`)
	}
	var version int
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return record, fmt.Errorf(`decode audit record field "version": %w`, err)
	}

	if version >= 1 && version <= FormatVersion {
		for name := range fields {
			if !diskFieldAllowed(version, name) {
				return record, fmt.Errorf(
					"audit record field %q is not valid in format version %d",
					name,
					version,
				)
			}
		}
	}
	if err := json.Unmarshal(encoded, &record); err != nil {
		return record, err
	}
	if version == FormatVersion {
		if err := validateCurrentRecordShape(record, fields); err != nil {
			return diskRecord{}, err
		}
	}
	if raw, ok := fields["provenance"]; ok {
		record.provenanceRaw = append(json.RawMessage(nil), raw...)
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			var provenance Provenance
			if err := decoder.Decode(&provenance); err != nil {
				return diskRecord{}, fmt.Errorf("decode audit provenance: %w", err)
			}
		}
	}
	return record, nil
}

func diskFieldAllowed(version int, name string) bool {
	switch name {
	case "version",
		"record_type",
		"recorded_at",
		"kind",
		"payload",
		"event_count",
		"previous_hash",
		"hash":
		return true
	case "header":
		return version == 1
	case "chain", "control_schema", "durability", "encoding_error":
		return version >= 2
	case "session",
		"tree_size",
		"tree_root",
		"signature",
		"key_id",
		"public_key",
		"provenance",
		"reason":
		return version >= 3
	default:
		return false
	}
}

func validateCurrentRecordShape(record diskRecord, fields map[string]json.RawMessage) error {
	for _, required := range []string{"version", "record_type", "recorded_at", "tree_size"} {
		if _, ok := fields[required]; !ok {
			return fmt.Errorf("audit record field %q is required in format version 3", required)
		}
	}
	if err := rejectPresentZeroFields(record, fields); err != nil {
		return err
	}

	allowedForType := func(name string) bool {
		switch record.RecordType {
		case "manifest":
			switch name {
			case "kind", "payload", "event_count", "encoding_error", "reason":
				return false
			}
		case "event":
			switch name {
			case "event_count",
				"chain",
				"control_schema",
				"durability",
				"session",
				"tree_root",
				"signature",
				"key_id",
				"public_key",
				"provenance",
				"reason":
				return false
			}
		case "checkpoint":
			switch name {
			case "kind",
				"payload",
				"chain",
				"control_schema",
				"durability",
				"encoding_error",
				"session",
				"key_id",
				"public_key",
				"provenance":
				return false
			}
		case "footer":
			switch name {
			case "kind",
				"payload",
				"chain",
				"control_schema",
				"durability",
				"encoding_error",
				"session",
				"key_id",
				"public_key",
				"provenance",
				"reason":
				return false
			}
		}
		return true
	}
	for name := range fields {
		if !allowedForType(name) {
			return fmt.Errorf(
				"audit record field %q is not valid on record type %q",
				name,
				record.RecordType,
			)
		}
	}
	return nil
}

func rejectPresentZeroFields(
	record diskRecord,
	fields map[string]json.RawMessage,
) error {
	zero := map[string]bool{
		"kind":           record.Kind == "",
		"payload":        len(record.Payload) == 0,
		"event_count":    record.EventCount == 0,
		"previous_hash":  record.PreviousHash == "",
		"hash":           record.Hash == "",
		"chain":          record.Chain == "",
		"control_schema": record.ControlSchema == "",
		"durability":     record.Durability == "",
		"encoding_error": record.EncodingError == "",
		"session":        record.Session == nil,
		"tree_root":      record.TreeRoot == "",
		"signature":      record.Signature == "",
		"key_id":         record.KeyID == "",
		"public_key":     record.PublicKey == "",
		"provenance":     record.Provenance == nil,
		"reason":         record.Reason == "",
	}
	for name, isZero := range zero {
		if _, present := fields[name]; present && isZero {
			return fmt.Errorf("audit record field %q must be omitted when empty", name)
		}
	}
	return nil
}

func rejectDuplicateJSONFields(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("audit record contains more than one JSON value")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := token.(string)
			if !ok {
				return errors.New("audit JSON object contains a non-string field name")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("audit JSON object contains duplicate field %q", name)
			}
			seen[name] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("audit JSON object is not terminated")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("audit JSON array is not terminated")
		}
	default:
		return errors.New("audit JSON contains an unexpected delimiter")
	}
	return nil
}
