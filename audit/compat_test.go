package audit

import (
	"bytes"
	"encoding/json"
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
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		buffer.Write(encoded)
		buffer.WriteByte('\n')
	}
	return bytes.NewReader(buffer.Bytes())
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
