package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/open-ships/teleop"
)

func TestRecorderCausalityStateScalesWithStreams(t *testing.T) {
	const events = 25_000
	recorder := NewRecorder(io.Discard, WithHashChain(false))
	for sequence := uint64(1); sequence <= events; sequence++ {
		if err := recorder.Record(t.Context(), testButtonEvent(sequence)); err != nil {
			t.Fatalf("record input event %d: %v", sequence, err)
		}
	}
	derived := testButtonEvent(1)
	derived.Meta.ID.Stream = "derived"
	derived.Meta.Causes = []teleop.EventID{{
		Session:  derived.Meta.ID.Session,
		Stream:   "input",
		Sequence: 1,
	}}
	if err := recorder.Record(t.Context(), derived); err != nil {
		t.Fatalf("record derived event: %v", err)
	}

	recorder.mu.Lock()
	streams := recorder.published.StreamCount()
	inputThrough := recorder.published.Through("input")
	recorder.mu.Unlock()
	if streams != 2 || inputThrough != events {
		t.Fatalf(
			"causality state = %d streams through input %d, want 2 through %d",
			streams,
			inputThrough,
			events,
		)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderCauseRequiresSameSessionAndPriorPublishedSequence(t *testing.T) {
	tests := map[string]teleop.EventID{
		"future sequence": {
			Session: teleop.SessionID{1}, Stream: "input", Sequence: 2,
		},
		"other session": {
			Session: teleop.SessionID{2}, Stream: "input", Sequence: 1,
		},
		"missing stream": {
			Session: teleop.SessionID{1}, Stream: "missing", Sequence: 1,
		},
	}
	for name, cause := range tests {
		t.Run(name, func(t *testing.T) {
			recorder := NewRecorder(io.Discard)
			if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
				t.Fatal(err)
			}
			derived := testButtonEvent(1)
			derived.Meta.ID.Stream = "derived"
			derived.Meta.Causes = []teleop.EventID{cause}
			if err := recorder.Record(t.Context(), derived); err == nil {
				t.Fatal("invalid cause was accepted")
			}
			_ = recorder.Close()
		})
	}
}

func TestRecorderRejectsStreamPerEventGrowthAtSessionLimit(t *testing.T) {
	recorder := NewRecorder(io.Discard, WithHashChain(false))
	for index := 0; index < teleop.MaxEventStreamsPerSession; index++ {
		event := testButtonEvent(1)
		event.Meta.ID.Stream = fmt.Sprintf("application-%d", index)
		if err := recorder.Record(t.Context(), event); err != nil {
			t.Fatalf("record stream %d: %v", index, err)
		}
	}
	overflow := testButtonEvent(1)
	overflow.Meta.ID.Stream = "one-stream-too-many"
	if err := recorder.Record(t.Context(), overflow); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("overflow record error = %v, want ErrInvalidEvent", err)
	}
	recorder.mu.Lock()
	streams := recorder.published.StreamCount()
	recorder.mu.Unlock()
	if streams != teleop.MaxEventStreamsPerSession {
		t.Fatalf("retained streams = %d, want cap %d", streams, teleop.MaxEventStreamsPerSession)
	}
	_ = recorder.Close()
}

func TestVerifyRejectsStreamPerEventGrowthAtSessionLimit(t *testing.T) {
	recordedAt := time.Unix(1, 0).UTC()
	records := make([]diskRecord, 0, teleop.MaxEventStreamsPerSession+1)
	for index := 0; index <= teleop.MaxEventStreamsPerSession; index++ {
		event := testButtonEvent(1)
		event.Meta.ID.Stream = fmt.Sprintf("application-%d", index)
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		header := event.Meta
		records = append(records, diskRecord{
			Version:    1,
			RecordType: "event",
			RecordedAt: recordedAt,
			Kind:       event.Kind(),
			Header:     &header,
			Payload:    payload,
		})
	}
	reader := encodeLegacy(t, records, recordHashV1)
	_, err := Verify(reader, VerifyOptions{AllowUnverified: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "event stream limit") {
		t.Fatalf("Verify error = %v, want event stream limit rejection", err)
	}

	// The encoded adversarial stream is finite and modest; this guards against
	// accidentally turning the regression itself into an unbounded fixture.
	if reader.Size() > int64(16<<20) {
		t.Fatalf("stream-limit fixture = %d bytes, want at most 16 MiB", reader.Size())
	}
}
