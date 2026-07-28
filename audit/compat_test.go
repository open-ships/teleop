package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

// Bumping FormatVersion to 3 kept the version 1 and 2 read paths, but every
// other test now writes version 3. These tests build legacy streams by hand so
// a regression in the older hashing or decoding paths cannot pass unnoticed:
// logs already on disk must stay readable, since a log that cannot be verified
// later is not evidence.

func encodeLegacy(t *testing.T, records []diskRecord, hashFor func(diskRecord) (string, error)) *bytes.Reader {
	t.Helper()
	buffer := &bytes.Buffer{}
	previous := ""
	for index := range records {
		record := records[index]
		record.PreviousHash = previous
		record.Hash = ""
		digest, err := hashFor(record)
		if err != nil {
			t.Fatal(err)
		}
		record.Hash = digest
		previous = digest
		wire := map[string]any{
			"version":       record.Version,
			"record_type":   record.RecordType,
			"recorded_at":   record.RecordedAt,
			"previous_hash": record.PreviousHash,
			"hash":          record.Hash,
		}
		if record.Kind != "" {
			wire["kind"] = record.Kind
		}
		if record.Header != nil {
			wire["header"] = record.Header
		}
		if len(record.Payload) > 0 {
			wire["payload"] = record.Payload
		}
		if record.EventCount > 0 {
			wire["event_count"] = record.EventCount
		}
		if record.Version == 2 {
			if record.Chain != "" {
				wire["chain"] = record.Chain
			}
			if record.ControlSchema != "" {
				wire["control_schema"] = record.ControlSchema
			}
			if record.Durability != "" {
				wire["durability"] = record.Durability
			}
			if record.EncodingError != "" {
				wire["encoding_error"] = record.EncodingError
			}
			delete(wire, "header")
		}
		if record.PreviousHash == "" {
			delete(wire, "previous_hash")
		}
		encoded, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		buffer.Write(encoded)
		buffer.WriteByte('\n')
	}
	return bytes.NewReader(buffer.Bytes())
}

func TestLegacyVersionCannotSatisfyTrustedVerification(t *testing.T) {
	recordedAt := time.Unix(1700000000, 0).UTC()
	event := testButtonEvent(1)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	header := event.Meta
	reader := encodeLegacy(t, []diskRecord{
		{
			Version:    1,
			RecordType: "event",
			RecordedAt: recordedAt,
			Kind:       event.Kind(),
			Header:     &header,
			Payload:    payload,
		},
		{
			Version:    1,
			RecordType: "footer",
			RecordedAt: recordedAt,
			EventCount: 1,
		},
	}, recordHashV1)
	public, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	consumed := 0
	_, err = Verify(reader, VerifyOptions{PublicKey: public}, func(Record) error {
		consumed++
		return nil
	})
	if !errors.Is(err, ErrSignatureRequired) {
		t.Fatalf("err = %v, want ErrSignatureRequired", err)
	}
	if consumed != 0 {
		t.Fatalf("consumed %d legacy events during trusted verification", consumed)
	}
}

func TestVersion2RejectsUnauthenticatedVersion3Metadata(t *testing.T) {
	recordedAt := time.Unix(1700000000, 0).UTC()
	key := bytes.Repeat([]byte{0x42}, 32)
	reader := encodeLegacy(t, []diskRecord{
		{
			Version:       2,
			RecordType:    "manifest",
			RecordedAt:    recordedAt,
			Chain:         chainHMAC,
			ControlSchema: ControlSchemaVersion,
			Durability:    "fsync-every-record",
		},
		{
			Version:    2,
			RecordType: "footer",
			RecordedAt: recordedAt,
		},
	}, func(record diskRecord) (string, error) {
		return recordHashV2(record, chainHMAC, key)
	})
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(t, raw)
	var manifest map[string]any
	if err := json.Unmarshal(lines[0], &manifest); err != nil {
		t.Fatal(err)
	}
	// Version 2 never authenticated provenance. A permissive decoder would
	// expose this injected value while still reporting the HMAC chain valid.
	manifest["provenance"] = map[string]any{"operator": "injected"}
	lines[0], err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	_, verification, err := ReadAuthenticated(joinLines(lines), key)
	if err == nil {
		t.Fatalf("smuggled v3 metadata authenticated: %+v", verification)
	}
}

func TestVersion2StreamsRemainReadable(t *testing.T) {
	recordedAt := time.Unix(1700000000, 0).UTC()
	payload, err := json.Marshal(testButtonEvent(1))
	if err != nil {
		t.Fatal(err)
	}
	records := []diskRecord{
		{
			Version:       2,
			RecordType:    "manifest",
			RecordedAt:    recordedAt,
			Chain:         chainSHA256,
			ControlSchema: ControlSchemaVersion,
			Durability:    "fsync-every-record",
		},
		{
			Version:    2,
			RecordType: "event",
			RecordedAt: recordedAt,
			Kind:       testButtonEvent(1).Kind(),
			Payload:    payload,
		},
		{
			Version:    2,
			RecordType: "footer",
			RecordedAt: recordedAt,
			EventCount: 1,
		},
	}

	reader := encodeLegacy(t, records, func(record diskRecord) (string, error) {
		return recordHashV2(record, chainSHA256, nil)
	})

	decoded, verification, err := Read(reader, VerifyOptions{RequireFooter: true})
	if err != nil {
		t.Fatalf("a version 2 log must still verify: %v", err)
	}
	if verification.Version != 2 {
		t.Fatalf("version = %d, want 2", verification.Version)
	}
	if !verification.Integrity || !verification.Complete {
		t.Fatalf("verification = %+v", verification)
	}
	if len(decoded) != 1 {
		t.Fatalf("records = %d, want 1", len(decoded))
	}
	// Version 2 predates the Merkle tree, so no head is claimed or checked.
	if verification.TreeSize != 0 {
		t.Fatalf("tree size = %d, want 0 for a pre-tree format", verification.TreeSize)
	}
	if verification.Signed || verification.Trusted {
		t.Fatal("a version 2 log carries no signatures")
	}
}

func TestVersion1StreamsRemainReadable(t *testing.T) {
	recordedAt := time.Unix(1700000000, 0).UTC()
	event := testButtonEvent(1)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	header := event.Meta
	records := []diskRecord{
		{
			Version:    1,
			RecordType: "event",
			RecordedAt: recordedAt,
			Kind:       event.Kind(),
			Header:     &header,
			Payload:    payload,
		},
		{
			Version:    1,
			RecordType: "footer",
			RecordedAt: recordedAt,
			EventCount: 1,
		},
	}

	reader := encodeLegacy(t, records, recordHashV1)

	decoded, verification, err := Read(reader, VerifyOptions{RequireFooter: true})
	if err != nil {
		t.Fatalf("a version 1 log must still verify: %v", err)
	}
	if verification.Version != 1 {
		t.Fatalf("version = %d, want 1", verification.Version)
	}
	if len(decoded) != 1 {
		t.Fatalf("records = %d, want 1", len(decoded))
	}
}

// TestVersion3RejectsDowngradedRecords guards the seam between formats: a
// version 3 stream that claims a lower version on a later line must not slip
// past the stricter version 3 checks.
func TestVersion3RejectsDowngradedRecords(t *testing.T) {
	buffer, public := signedLog(t, 3)
	lines := splitLines(t, buffer.Bytes())

	var record map[string]any
	if err := json.Unmarshal(lines[2], &record); err != nil {
		t.Fatal(err)
	}
	record["version"] = 2
	rewritten, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	lines[2] = rewritten

	_, _, err = Read(joinLines(lines), VerifyOptions{
		RequireFooter: true,
		PublicKey:     public,
	})
	if err == nil {
		t.Fatal("a downgraded record must not verify")
	}
}
