package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-ships/teleop"
)

// These tests are internal so they can reach diskRecord when reconstructing a
// log's Merkle leaves the way an independent verifier would.

func testButtonEvent(sequence uint64) teleop.ButtonEvent {
	var session teleop.SessionID
	session[0] = 1
	return teleop.ButtonEvent{
		Meta: teleop.Header{
			ID: teleop.EventID{
				Session:  session,
				Stream:   "input",
				Sequence: sequence,
			},
			DeviceID:   "test:0",
			ObservedAt: time.Unix(int64(sequence), 0).UTC(),
			Monotonic:  time.Duration(sequence) * time.Millisecond,
		},
		Button:  teleop.ButtonFaceSouth,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	}
}

func signedLog(t *testing.T, events int, options ...Option) (*bytes.Buffer, ed25519.PublicKey) {
	t.Helper()
	public, private, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	buffer := &bytes.Buffer{}
	recorder := NewRecorder(buffer, append([]Option{WithSigner(private)}, options...)...)
	for sequence := 1; sequence <= events; sequence++ {
		if err := recorder.Record(t.Context(), testButtonEvent(uint64(sequence))); err != nil {
			t.Fatalf("record %d: %v", sequence, err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer, public
}

func TestSignedLogVerifiesAgainstTrustedKey(t *testing.T) {
	buffer, public := signedLog(t, 5)

	records, verification, err := Read(buffer, VerifyOptions{
		RequireFooter:    true,
		RequireSignature: true,
		PublicKey:        public,
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(records) != 5 {
		t.Fatalf("records = %d, want 5", len(records))
	}
	if !verification.Signed {
		t.Fatal("verification.Signed must be true")
	}
	if !verification.Trusted {
		t.Fatal("verification.Trusted must be true when a key was supplied")
	}
	if verification.KeyID != KeyID(public) {
		t.Fatalf("key ID = %q, want %q", verification.KeyID, KeyID(public))
	}
	// Manifest, five events, and footer are all leaves.
	if verification.TreeSize != 7 {
		t.Fatalf("tree size = %d, want 7", verification.TreeSize)
	}
}

// TestVerifyRejectsUntrustedKey covers the substitution a forger would attempt:
// rewrite the log and re-sign it with a key they control. The log stays
// internally consistent, so only comparison against an out-of-band key catches
// it.
func TestVerifyRejectsUntrustedKey(t *testing.T) {
	buffer, _ := signedLog(t, 3)
	other, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = Read(buffer, VerifyOptions{RequireFooter: true, PublicKey: other})
	if !errors.Is(err, ErrUntrustedKey) {
		t.Fatalf("err = %v, want ErrUntrustedKey", err)
	}
}

func TestVerifyDistinguishesSelfDeclaredKeyFromTrustedKey(t *testing.T) {
	buffer, _ := signedLog(t, 3)

	_, verification, err := Read(buffer, VerifyOptions{RequireFooter: true})
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Signed {
		t.Fatal("signatures must verify against the log's declared key")
	}
	if verification.Trusted {
		t.Fatal("Trusted must be false without a caller-supplied key")
	}
}

func TestVerifyRequiresSignatureWhenDemanded(t *testing.T) {
	buffer := &bytes.Buffer{}
	recorder := NewRecorder(buffer)
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err := Read(buffer, VerifyOptions{RequireFooter: true, RequireSignature: true})
	if !errors.Is(err, ErrSignatureRequired) {
		t.Fatalf("err = %v, want ErrSignatureRequired", err)
	}
}

// TestSignatureDetectsTamperedProvenance guards the field an operator has the
// most incentive to change after the fact: which build and configuration were
// actually running.
func TestSignatureDetectsTamperedProvenance(t *testing.T) {
	provenance := CaptureProvenance()
	provenance.Application = "vessel-teleop"
	provenance.Operator = "operator-7"
	provenance.Config = map[string]any{"dead_zone": 0.12}

	buffer, public := signedLog(t, 2, WithProvenance(provenance))

	lines := splitLines(t, buffer.Bytes())
	var manifest map[string]any
	if err := json.Unmarshal(lines[0], &manifest); err != nil {
		t.Fatal(err)
	}
	recorded, ok := manifest["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("manifest carries no provenance: %v", manifest)
	}
	if recorded["operator"] != "operator-7" {
		t.Fatalf("operator = %v", recorded["operator"])
	}

	recorded["operator"] = "operator-9"
	manifest["provenance"] = recorded
	rewritten, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	lines[0] = rewritten

	_, _, err = Read(joinLines(lines), VerifyOptions{
		RequireFooter: true,
		PublicKey:     public,
	})
	if err == nil {
		t.Fatal("a rewritten operator identity must not verify")
	}
}

// TestCheckpointDetectsExcisedRecords is the truncation case. A forger who
// removes records from the middle and repairs the hash chain still cannot
// produce a checkpoint whose Merkle head matches the shortened history.
func TestCheckpointDetectsExcisedRecords(t *testing.T) {
	buffer, public := signedLog(t, 12, WithCheckpoints(0, 4))

	lines := splitLines(t, buffer.Bytes())
	// Drop one event record that a checkpoint already committed to.
	var kept [][]byte
	removed := false
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if !removed && record["record_type"] == "event" {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		t.Fatal("no event record found to remove")
	}

	_, _, err := Read(joinLines(kept), VerifyOptions{
		RequireFooter: true,
		PublicKey:     public,
	})
	if err == nil {
		t.Fatal("an excised record must not verify")
	}
}

func TestCheckpointsAreEmittedByCount(t *testing.T) {
	buffer, public := signedLog(t, 10, WithCheckpoints(0, 4))

	_, verification, err := Read(buffer, VerifyOptions{
		RequireFooter: true,
		PublicKey:     public,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ten events with a checkpoint every four yields checkpoints after events
	// four and eight.
	if verification.Checkpoints != 2 {
		t.Fatalf("checkpoints = %d, want 2", verification.Checkpoints)
	}
}

func TestCheckpointsAreEmittedByInterval(t *testing.T) {
	public, private, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	buffer := &bytes.Buffer{}
	recorder := NewRecorder(
		buffer,
		WithSigner(private),
		WithCheckpoints(time.Second, 0),
		WithClock(func() time.Time {
			now = now.Add(400 * time.Millisecond)
			return now
		}),
	)
	for sequence := 1; sequence <= 10; sequence++ {
		if err := recorder.Record(t.Context(), testButtonEvent(uint64(sequence))); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	_, verification, err := Read(buffer, VerifyOptions{
		RequireFooter: true,
		PublicKey:     public,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verification.Checkpoints == 0 {
		t.Fatal("elapsed time must produce checkpoints")
	}
}

func TestAnchorReceivesSignedCheckpoints(t *testing.T) {
	public, private, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	anchored := make(chan Checkpoint, 32)
	buffer := &bytes.Buffer{}
	recorder := NewRecorder(
		buffer,
		WithSigner(private),
		WithCheckpoints(0, 3),
		WithAnchor(AnchorFunc(func(_ context.Context, checkpoint Checkpoint) error {
			anchored <- checkpoint
			return nil
		})),
	)
	for sequence := 1; sequence <= 6; sequence++ {
		if err := recorder.Record(t.Context(), testButtonEvent(uint64(sequence))); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	close(anchored)

	var checkpoints []Checkpoint
	for checkpoint := range anchored {
		checkpoints = append(checkpoints, checkpoint)
	}
	// Manifest, two count-triggered checkpoints, and the footer.
	if len(checkpoints) != 4 {
		t.Fatalf("anchored %d checkpoints, want 4", len(checkpoints))
	}

	// An anchored head must be independently verifiable, which is the whole
	// point of publishing it outside the log.
	last := checkpoints[len(checkpoints)-1]
	root, err := hex.DecodeString(last.Root)
	if err != nil {
		t.Fatal(err)
	}
	var session teleop.SessionID
	session[0] = 1
	head := TreeHead{
		Version:    FormatVersion,
		RecordType: "footer",
		Session:    session,
		Size:       last.Size,
		Root:       root,
		ChainHead:  last.ChainHead,
		EventCount: last.EventCount,
		RecordedAt: last.RecordedAt,
	}
	if err := VerifyTreeHead(public, head, last.Signature); err != nil {
		t.Fatalf("anchored footer head must verify: %v", err)
	}

	if stats := recorder.AnchorStats(); stats.Failed != 0 || stats.Dropped != 0 {
		t.Fatalf("anchor stats = %+v", stats)
	}
}

func TestAnchorFailureIsReportedNotFatal(t *testing.T) {
	_, private, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	buffer := &bytes.Buffer{}
	recorder := NewRecorder(
		buffer,
		WithSigner(private),
		WithCheckpoints(0, 2),
		WithAnchor(AnchorFunc(func(context.Context, Checkpoint) error {
			return errors.New("anchor unreachable")
		})),
	)
	for sequence := 1; sequence <= 4; sequence++ {
		if err := recorder.Record(t.Context(), testButtonEvent(uint64(sequence))); err != nil {
			t.Fatalf("an unreachable anchor must not stop recording: %v", err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	stats := recorder.AnchorStats()
	if stats.Failed == 0 {
		t.Fatal("anchor failures must be counted")
	}
	if stats.Err == nil {
		t.Fatal("anchor failure must be retrievable")
	}
}

// TestInclusionProofOverRecordedLog demonstrates selective disclosure: one
// record proved to belong to a signed head without revealing the others.
func TestInclusionProofOverRecordedLog(t *testing.T) {
	buffer, public := signedLog(t, 8)
	raw := buffer.Bytes()

	lines := splitLines(t, raw)
	leaves := make([][]byte, 0, len(lines))
	for _, line := range lines {
		var record diskRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		hashBytes, err := hex.DecodeString(record.Hash)
		if err != nil {
			t.Fatal(err)
		}
		leaves = append(leaves, HashLeaf(hashBytes))
	}

	_, verification, err := Read(bytes.NewReader(raw), VerifyOptions{
		RequireFooter: true,
		PublicKey:     public,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Root(leaves), verification.TreeRoot) {
		t.Fatal("independently computed root must match verification")
	}

	// Prove the fourth record without disclosing any other record's contents.
	proof, err := InclusionProof(3, leaves)
	if err != nil {
		t.Fatal(err)
	}
	err = VerifyInclusion(
		3,
		uint64(len(leaves)),
		leaves[3],
		verification.TreeRoot,
		proof,
	)
	if err != nil {
		t.Fatalf("inclusion proof must verify: %v", err)
	}
}

func TestProvenanceRoundTrips(t *testing.T) {
	provenance := CaptureProvenance()
	provenance.Application = "harbor-tug"
	provenance.ApplicationVersion = "2.4.0"
	provenance.Authorization = "work-order-8812"
	provenance.Config = map[string]any{"dead_zone": 0.15, "rate_limit_hz": 50}
	provenance.Platform = map[string]string{"driver": "xpadneo 0.9.5"}

	buffer, public := signedLog(t, 2, WithProvenance(provenance))

	_, verification, err := Read(buffer, VerifyOptions{
		RequireFooter: true,
		PublicKey:     public,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verification.Provenance == nil {
		t.Fatal("provenance must survive verification")
	}
	if verification.Provenance.Application != "harbor-tug" {
		t.Fatalf("application = %q", verification.Provenance.Application)
	}
	if verification.Provenance.Authorization != "work-order-8812" {
		t.Fatalf("authorization = %q", verification.Provenance.Authorization)
	}
	if verification.Provenance.GoVersion == "" {
		t.Fatal("captured provenance must include the Go version")
	}
	if verification.Provenance.Config["rate_limit_hz"] != float64(50) {
		t.Fatalf("config = %v", verification.Provenance.Config)
	}
}

func TestManifestBindsSession(t *testing.T) {
	buffer, public := signedLog(t, 2)

	_, verification, err := Read(buffer, VerifyOptions{
		RequireFooter: true,
		PublicKey:     public,
	})
	if err != nil {
		t.Fatal(err)
	}
	var want teleop.SessionID
	want[0] = 1
	if verification.Session != want {
		t.Fatalf("session = %v, want %v", verification.Session, want)
	}
}

func splitLines(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	var lines [][]byte
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		lines = append(lines, []byte(line))
	}
	return lines
}

func joinLines(lines [][]byte) *bytes.Reader {
	return bytes.NewReader(append(bytes.Join(lines, []byte("\n")), '\n'))
}
