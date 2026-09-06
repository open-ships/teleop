package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
)

func TestSignedProvenancePreservesSliceViewsAndExactNumbers(t *testing.T) {
	shared := []int{10, 20, 30}
	provenance := audit.Provenance{Config: map[string]any{
		"short": shared[:1], "long": shared,
		"empty": shared[:0], "integer": uint64(9007199254740993),
		"nested": map[string]any{"max": uint64(18446744073709551615)},
	}}
	want, err := json.Marshal(provenance.Config)
	if err != nil {
		t.Fatal(err)
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	recorder := audit.NewRecorder(&output, audit.WithSigner(key), audit.WithProvenance(provenance))
	shared[0] = 99
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	_, verification, err := audit.ReadTrusted(bytes.NewReader(output.Bytes()), public)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(verification.Provenance.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("verified config = %s, want %s", got, want)
	}
}

func TestLegacyEncodingFailureNeverManufacturesReplayState(t *testing.T) {
	record := audit.Record{Kind: teleop.EventConnection, EncodingError: "lost payload", Payload: json.RawMessage(`{"header":{}}`)}
	if _, found, err := audit.Descriptor([]audit.Record{record}); err == nil || found {
		t.Fatalf("descriptor found=%v err=%v", found, err)
	}
	record.Kind = teleop.EventObservation
	observations, err := audit.Observations([]audit.Record{record})
	var fault *audit.ReplayFaultError
	if !errors.As(err, &fault) || len(observations) != 0 {
		t.Fatalf("observations=%v err=%v", observations, err)
	}
	event, err := audit.DecodeEvent(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(event); err == nil {
		t.Fatal("encoding failure was serialized as a real event")
	}
}

func TestUnknownEventCanBeRecordedAfterForensicDecode(t *testing.T) {
	var session teleop.SessionID
	session[0] = 1
	header := teleop.Header{ID: teleop.EventID{Session: session, Stream: "extension", Sequence: 1}}
	encodedHeader, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"header":` + string(encodedHeader) + `,"future":{"integer":9007199254740993}}`)
	event, err := audit.DecodeEvent(audit.Record{Kind: "vessel.future", Header: header, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	recorder := audit.NewRecorder(&output)
	if err := recorder.Record(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := audit.ReadAll(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !bytes.Equal(records[0].Payload, payload) {
		t.Fatalf("round trip changed extension payload: %+v", records)
	}
}
