package audit_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
)

func TestHashChainDetectsTampering(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	recorder := audit.NewRecorder(&output)
	for sequence := uint64(1); sequence <= 2; sequence++ {
		err := recorder.Record(context.Background(), teleop.ButtonEvent{
			Meta: teleop.Header{
				ID:         teleop.EventID{Stream: "input", Sequence: sequence},
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
	event := teleop.ObservationEvent{
		Meta: teleop.Header{
			ID:         teleop.EventID{Stream: "input", Sequence: 1},
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
