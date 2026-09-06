package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
)

func TestRetainedWitnessDetectsSignedForkAndTruncation(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	makeLog := func(pressed bool) ([]byte, audit.Checkpoint) {
		t.Helper()
		var output bytes.Buffer
		recorder := audit.NewRecorder(&output, audit.WithSigner(key), audit.WithAnchor(audit.AnchorFunc(func(context.Context, audit.Checkpoint) error { return nil })), audit.WithRequiredWitness(true))
		header := teleop.Header{ID: teleop.EventID{Session: teleop.SessionID{1}, Stream: "input", Sequence: 1}}
		if err := recorder.Record(t.Context(), teleop.ButtonEvent{Meta: header, Button: teleop.ButtonFaceSouth, Pressed: pressed}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := recorder.CheckpointAndWait(ctx, "before footer"); err != nil {
			t.Fatal(err)
		}
		if err := recorder.Close(); err != nil {
			t.Fatal(err)
		}
		return append([]byte(nil), output.Bytes()...), recorder.EvidenceStatus().LastWitness.Checkpoint
	}
	data, witness := makeLog(true)
	options := audit.VerifyOptions{PublicKey: public, RequireFooter: true, RequireWitness: true, Witnesses: []audit.Checkpoint{witness}}
	verification, err := audit.Verify(bytes.NewReader(data), options, nil)
	if err != nil || verification.MatchedWitnesses != 1 || verification.WitnessedEvents != 1 || !verification.Trusted {
		t.Fatalf("exact witness verification=%+v err=%v", verification, err)
	}
	fork, _ := makeLog(false)
	if _, _, err := audit.ReadTrusted(bytes.NewReader(fork), public); err != nil {
		t.Fatalf("test fork must be validly signed: %v", err)
	}
	if _, err := audit.Verify(bytes.NewReader(fork), options, nil); !errors.Is(err, audit.ErrWitnessMismatch) {
		t.Fatalf("signed fork accepted: %v", err)
	}
	// This prefix is internally signed and valid, but omits the independently
	// retained final head. Optional footer checking must not hide truncation.
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	truncated := append(bytes.Join(lines[:len(lines)-1], []byte("\n")), '\n')
	options.RequireFooter = false
	if _, err := audit.Verify(bytes.NewReader(truncated), options, nil); !errors.Is(err, audit.ErrWitnessMismatch) {
		t.Fatalf("witnessed footer truncation accepted: %v", err)
	}
	options.Witnesses = nil
	if _, err := audit.Verify(bytes.NewReader(data), options, nil); !errors.Is(err, audit.ErrWitnessRequired) {
		t.Fatalf("missing witness accepted: %v", err)
	}
}
