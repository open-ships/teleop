package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
)

func TestIncidentVerifierRequiresIndependentTrustAndWithholdsFailedTimeline(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	recorder := audit.NewRecorder(&data, audit.WithSigner(key), audit.WithRequiredWitness(true), audit.WithAnchor(audit.AnchorFunc(func(context.Context, audit.Checkpoint) error { return nil })))
	if err := recorder.Record(t.Context(), teleop.ButtonEvent{Meta: teleop.Header{ID: teleop.EventID{Session: teleop.SessionID{1}, Stream: "input", Sequence: 1}}, Button: teleop.ButtonFaceSouth, Pressed: true}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath, logPath, witnessPath := filepath.Join(dir, "public.hex"), filepath.Join(dir, "evidence.jsonl"), filepath.Join(dir, "witness.jsonl")
	witness, err := json.Marshal(recorder.EvidenceStatus().LastWitness.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string][]byte{keyPath: []byte(hex.EncodeToString(public)), logPath: data.Bytes(), witnessPath: append(witness, '\n')} {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var output, diagnostics bytes.Buffer
	if err := run([]string{"-key", keyPath, logPath}, &output, &diagnostics); err == nil {
		t.Fatal("implicit local-only verification accepted")
	}
	args := []string{"-key", keyPath, "-witnesses", witnessPath, "-events", logPath}
	if err := run(args, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"type":"event"`)) || !bytes.Contains(output.Bytes(), []byte(`"MatchedWitnesses":1`)) {
		t.Fatalf("missing witnessed timeline: %s", output.Bytes())
	}
	output.Reset()
	if err := os.WriteFile(logPath, append(data.Bytes(), []byte("{}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(args, &output, &diagnostics); err == nil {
		t.Fatal("trailing corruption accepted")
	}
	if bytes.Contains(output.Bytes(), []byte(`"type":"event"`)) {
		t.Fatal("timeline exposed before complete verification")
	}
	if err := os.WriteFile(logPath, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(witnessPath, append([]byte(`{"version":3,`), witness[1:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(args, &output, &diagnostics); err == nil {
		t.Fatal("ambiguous duplicate witness field accepted")
	}
	if bytes.Contains(output.Bytes(), []byte(`"type":"event"`)) {
		t.Fatal("ambiguous witness released timeline")
	}
}
