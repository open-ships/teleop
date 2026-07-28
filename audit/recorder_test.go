package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/action"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/gesture"
)

func TestHashChainDetectsTampering(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	recorder := audit.NewRecorder(&output)
	var session teleop.SessionID
	session[0] = 1
	for sequence := uint64(1); sequence <= 2; sequence++ {
		err := recorder.Record(context.Background(), teleop.ButtonEvent{
			Meta: teleop.Header{
				ID: teleop.EventID{
					Session:  session,
					Stream:   "input",
					Sequence: sequence,
				},
				ObservedAt: time.Unix(int64(sequence), 0),
			},
			Button:  teleop.ButtonFaceSouth,
			Pressed: true,
			Phase:   teleop.PhasePressed,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.ReadAll(bytes.NewReader(output.Bytes())); err != nil {
		t.Fatalf("valid log: %v", err)
	}

	tampered := bytes.Replace(output.Bytes(), []byte(`"pressed":true`), []byte(`"pressed":false`), 1)
	if _, err := audit.ReadAll(bytes.NewReader(tampered)); err == nil {
		t.Fatal("tampered log passed verification")
	}

	lastLine := bytes.LastIndexByte(output.Bytes()[:output.Len()-1], '\n')
	truncated := output.Bytes()[:lastLine+1]
	if _, err := audit.ReadAll(bytes.NewReader(truncated)); err == nil {
		t.Fatal("truncated log passed complete verification")
	}
	if _, err := audit.ReadPartial(bytes.NewReader(truncated)); err != nil {
		t.Fatalf("partial verification: %v", err)
	}
}

func TestObservationsExtractReplayableState(t *testing.T) {
	t.Parallel()

	state := teleop.State{LeftTrigger: 0.75}
	var session teleop.SessionID
	session[0] = 1
	event := teleop.ObservationEvent{
		Meta: teleop.Header{
			ID: teleop.EventID{
				Session:  session,
				Stream:   "input",
				Sequence: 1,
			},
			ObservedAt: time.Unix(20, 0),
		},
		Current: state,
		Native:  teleop.NativeInput{Format: "test"},
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
	observations, err := audit.Observations(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].State.LeftTrigger != state.LeftTrigger {
		t.Fatalf("observations = %#v", observations)
	}
}

func TestReadRejectsMissingHashesAndRequiresUnverifiedOptIn(t *testing.T) {
	t.Parallel()

	var chained bytes.Buffer
	recorder := audit.NewRecorder(&chained)
	if err := recorder.Record(context.Background(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(chained.Bytes()), []byte{'\n'})
	var eventLine map[string]any
	if err := json.Unmarshal(lines[1], &eventLine); err != nil {
		t.Fatal(err)
	}
	delete(eventLine, "hash")
	lines[1], _ = json.Marshal(eventLine)
	dehashed := append(bytes.Join(lines, []byte{'\n'}), '\n')
	if _, err := audit.ReadAll(bytes.NewReader(dehashed)); err == nil {
		t.Fatal("chained stream with a missing record hash was accepted")
	}

	var unchained bytes.Buffer
	recorder = audit.NewRecorder(&unchained, audit.WithHashChain(false))
	if err := recorder.Record(context.Background(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.ReadAll(bytes.NewReader(unchained.Bytes())); !errors.Is(err, audit.ErrUnverified) {
		t.Fatalf("ReadAll unchained error = %v, want ErrUnverified", err)
	}
	records, verification, err := audit.Read(
		bytes.NewReader(unchained.Bytes()),
		audit.VerifyOptions{RequireFooter: true, AllowUnverified: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || verification.Integrity || !verification.Complete {
		t.Fatalf("unverified read = %#v, %#v", records, verification)
	}
}

func TestHMACRequiresAndAuthenticatesKey(t *testing.T) {
	t.Parallel()

	key := []byte("controller audit secret")
	var output bytes.Buffer
	recorder := audit.NewRecorder(&output, audit.WithHMAC(key))
	key[0] ^= 0xff // Recorder owns a defensive copy.
	if err := recorder.Record(context.Background(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.ReadAll(bytes.NewReader(output.Bytes())); !errors.Is(
		err,
		audit.ErrAuthenticationRequired,
	) {
		t.Fatalf("ReadAll HMAC error = %v", err)
	}
	var integrityOnly bytes.Buffer
	integrityRecorder := audit.NewRecorder(&integrityOnly)
	if err := integrityRecorder.Record(context.Background(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	if err := integrityRecorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := audit.ReadAuthenticated(
		bytes.NewReader(integrityOnly.Bytes()),
		[]byte("controller audit secret"),
	); !errors.Is(err, audit.ErrUnauthenticated) {
		t.Fatalf("SHA stream authenticated error = %v", err)
	}
	if _, _, err := audit.ReadAuthenticated(
		bytes.NewReader(output.Bytes()),
		[]byte("wrong key"),
	); err == nil {
		t.Fatal("wrong HMAC key authenticated")
	}
	records, verification, err := audit.ReadAuthenticated(
		bytes.NewReader(output.Bytes()),
		[]byte("controller audit secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !verification.Integrity || !verification.Authenticated {
		t.Fatalf("authenticated read = %#v, %#v", records, verification)
	}

	var invalid bytes.Buffer
	emptyKey := audit.NewRecorder(&invalid, audit.WithHMAC(nil))
	if err := emptyKey.Record(context.Background(), testButtonEvent(1)); !errors.Is(
		err,
		audit.ErrAuthenticationRequired,
	) {
		t.Fatalf("empty HMAC key error = %v", err)
	}
}

func TestRecorderRequiresOneNonZeroSession(t *testing.T) {
	t.Parallel()

	t.Run("zero session", func(t *testing.T) {
		var output bytes.Buffer
		recorder := audit.NewRecorder(&output)
		event := testButtonEvent(1)
		event.Meta.ID.Session = teleop.SessionID{}

		if err := recorder.Record(context.Background(), event); !errors.Is(
			err,
			audit.ErrSessionRequired,
		) {
			t.Fatalf("zero-session Record error = %v, want ErrSessionRequired", err)
		}
	})

	t.Run("session change", func(t *testing.T) {
		var output bytes.Buffer
		recorder := audit.NewRecorder(&output)
		if err := recorder.Record(context.Background(), testButtonEvent(1)); err != nil {
			t.Fatal(err)
		}
		event := testButtonEvent(2)
		event.Meta.ID.Session[0] = 2

		if err := recorder.Record(context.Background(), event); err == nil {
			t.Fatal("Record accepted an event from a different session")
		}
	})

	t.Run("checkpoint before session", func(t *testing.T) {
		var output bytes.Buffer
		recorder := audit.NewRecorder(&output)
		if err := recorder.Checkpoint("arming"); !errors.Is(
			err,
			audit.ErrSessionRequired,
		) {
			t.Fatalf("pre-session Checkpoint error = %v, want ErrSessionRequired", err)
		}
		if err := recorder.Record(context.Background(), testButtonEvent(1)); err != nil {
			t.Fatal(err)
		}
		if err := recorder.Checkpoint("arming"); err != nil {
			t.Fatalf("bound Checkpoint: %v", err)
		}
		if err := recorder.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRecorderRetainsPublicKeyAfterClose(t *testing.T) {
	t.Parallel()

	public, private, err := audit.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	hmacKey := bytes.Repeat([]byte{0x42}, 32)
	var output bytes.Buffer
	recorder := audit.NewRecorder(
		&output,
		audit.WithHMAC(hmacKey),
		audit.WithSigner(private),
	)
	if err := recorder.Record(context.Background(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	// Public identity remains available after Close, while the caller continues
	// to own its original HMAC key.
	if got := recorder.PublicKey(); !got.Equal(public) {
		t.Fatalf("PublicKey after Close = %x, want %x", got, public)
	}
	if hmacKey[0] != 0x42 {
		t.Fatal("Close modified the caller-owned HMAC key")
	}
}

func TestReadPartialReturnsVerifiedPrefixOfTornStream(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	recorder := audit.NewRecorder(&output)
	for sequence := uint64(1); sequence <= 2; sequence++ {
		if err := recorder.Record(context.Background(), testButtonEvent(sequence)); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	torn := append(bytes.Join(lines[:2], []byte{'\n'}), '\n')
	torn = append(torn, lines[2][:len(lines[2])/2]...)

	records, err := audit.ReadPartial(bytes.NewReader(torn))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Header.ID.Sequence != 1 {
		t.Fatalf("partial records = %#v", records)
	}
	records, err = audit.ReadAll(bytes.NewReader(torn))
	if !errors.Is(err, audit.ErrIncomplete) || len(records) != 1 {
		t.Fatalf("complete read = %d records, %v", len(records), err)
	}
}

func TestRecorderFailureIsSticky(t *testing.T) {
	t.Parallel()

	writer := &failAfterWrites{allowed: 1}
	recorder := audit.NewRecorder(writer)
	err := recorder.Record(context.Background(), testButtonEvent(1))
	if !errors.Is(err, audit.ErrFailed) || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("first failure = %v", err)
	}
	calls := writer.calls
	if err := recorder.Record(context.Background(), testButtonEvent(2)); !errors.Is(
		err,
		io.ErrClosedPipe,
	) {
		t.Fatalf("sticky Record error = %v", err)
	}
	if err := recorder.Close(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("sticky Close error = %v", err)
	}
	if writer.calls != calls {
		t.Fatalf("failed recorder wrote again: calls %d -> %d", calls, writer.calls)
	}
}

func TestDecodeEventPreservesEncodingFailureAndDerivedTypes(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	recorder := audit.NewRecorder(&output)
	unrepresentable := badEvent{
		Meta:  testHeader("bad", 1),
		Value: math.NaN(),
	}
	if err := recorder.Record(context.Background(), unrepresentable); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := audit.ReadAll(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := audit.DecodeEvent(records[0])
	if err != nil {
		t.Fatal(err)
	}
	encodingFailure, ok := decoded.(audit.EncodingErrorEvent)
	if !ok || encodingFailure.Kind() != unrepresentable.Kind() || encodingFailure.Message == "" {
		t.Fatalf("encoding failure = %#v", decoded)
	}

	gestureEvent := gesture.Event{
		Meta:     testHeader("gesture", 1),
		Type:     gesture.Chord,
		Phase:    teleop.PhaseStarted,
		Controls: []teleop.ControlID{teleop.ButtonFaceSouth, teleop.ButtonBumperLeft},
		Region:   "arm",
	}
	actionEvent := action.Event{
		Meta:     testHeader("action", 1),
		Action:   "arm",
		Phase:    teleop.PhaseStarted,
		Controls: append([]teleop.ControlID(nil), gestureEvent.Controls...),
	}
	for _, event := range []teleop.Event{gestureEvent, actionEvent} {
		payload, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		value, decodeErr := audit.DecodeEvent(audit.Record{
			Kind:    event.Kind(),
			Header:  event.Header(),
			Payload: payload,
		})
		if decodeErr != nil || value.Kind() != event.Kind() {
			t.Fatalf("decode %s = %#v, %v", event.Kind(), value, decodeErr)
		}
	}

	opaque, err := audit.DecodeEvent(audit.Record{
		Kind:    "third-party",
		Header:  testHeader("third-party", 1),
		Payload: json.RawMessage(`{"custom":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := opaque.(audit.OpaqueEvent); !ok {
		t.Fatalf("unknown event = %T", opaque)
	}
}

func testButtonEvent(sequence uint64) teleop.ButtonEvent {
	return teleop.ButtonEvent{
		Meta:    testHeader("input", sequence),
		Button:  teleop.ButtonFaceSouth,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	}
}

func testHeader(stream string, sequence uint64) teleop.Header {
	var session teleop.SessionID
	session[0] = 1
	return teleop.Header{
		ID: teleop.EventID{
			Session:  session,
			Stream:   stream,
			Sequence: sequence,
		},
		DeviceID:   "test:0",
		ObservedAt: time.Unix(int64(sequence), 0),
	}
}

type failAfterWrites struct {
	allowed int
	calls   int
}

func (writer *failAfterWrites) Write(value []byte) (int, error) {
	writer.calls++
	if writer.calls > writer.allowed {
		return 0, io.ErrClosedPipe
	}
	return len(value), nil
}

type badEvent struct {
	Meta  teleop.Header `json:"header"`
	Value float64       `json:"value"`
}

func (event badEvent) Header() teleop.Header { return event.Meta.Clone() }
func (badEvent) Kind() teleop.EventKind      { return "bad.event" }
