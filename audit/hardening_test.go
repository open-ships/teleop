package audit_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
)

func TestProvenanceConfigurationIsDeeplyFrozen(t *testing.T) {
	t.Parallel()

	type limits struct {
		Maximum int `json:"maximum"`
	}
	nestedMap := map[string]any{"dead_zone": 0.12}
	nestedSlice := []any{map[string]any{"action": "thrust"}}
	nestedPointer := &limits{Maximum: 7}
	provenance := audit.Provenance{Config: map[string]any{
		"mapping": nestedMap,
		"routes":  nestedSlice,
		"limits":  nestedPointer,
		"unset":   nil,
	}}

	var output bytes.Buffer
	recorder := audit.NewRecorder(&output, audit.WithProvenance(provenance))
	nestedMap["dead_zone"] = 0.99
	nestedSlice[0].(map[string]any)["action"] = "mutated"
	nestedPointer.Maximum = 99
	provenance.Config["new"] = true

	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	_, verification, err := audit.Read(
		bytes.NewReader(output.Bytes()),
		audit.VerifyOptions{RequireFooter: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	config := verification.Provenance.Config
	if config["new"] != nil {
		t.Fatalf("late top-level mutation was recorded: %#v", config)
	}
	mapping := config["mapping"].(map[string]any)
	if mapping["dead_zone"] != json.Number("0.12") {
		t.Fatalf("nested map = %#v", mapping)
	}
	routes := config["routes"].([]any)
	if routes[0].(map[string]any)["action"] != "thrust" {
		t.Fatalf("nested slice = %#v", routes)
	}
	recordedLimits := config["limits"].(map[string]any)
	if recordedLimits["maximum"] != json.Number("7") {
		t.Fatalf("nested pointer = %#v", recordedLimits)
	}
}

func TestRecorderFreezesCustomProvenanceJSONOnce(t *testing.T) {
	values := map[string]string{"mode": "harbor"}
	calls := 0
	provenance := audit.Provenance{Config: map[string]any{
		"custom": customProvenanceValue{values: values, calls: &calls},
	}}

	var output bytes.Buffer
	recorder := audit.NewRecorder(&output, audit.WithProvenance(provenance))
	values["mode"] = "mutated"
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("custom provenance marshaler called %d times, want exactly once", calls)
	}
	_, verification, err := audit.Read(
		bytes.NewReader(output.Bytes()),
		audit.VerifyOptions{RequireFooter: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	custom := verification.Provenance.Config["custom"].(map[string]any)
	if custom["mode"] != "harbor" {
		t.Fatalf("custom provenance changed after construction: %#v", custom)
	}
}

func TestRecorderMakesProvenanceNormalizationFailureSticky(t *testing.T) {
	var output bytes.Buffer
	recorder := audit.NewRecorder(
		&output,
		audit.WithProvenance(audit.Provenance{Config: map[string]any{
			"invalid": failingProvenanceValue{},
		}}),
	)
	if err := recorder.EvidenceStatus().Err; !errors.Is(err, errTestProvenanceMarshal) {
		t.Fatalf("initial EvidenceStatus error = %v, want provenance marshal error", err)
	}
	if err := recorder.Record(t.Context(), testButtonEvent(1)); !errors.Is(
		err,
		errTestProvenanceMarshal,
	) {
		t.Fatalf("Record error = %v, want provenance marshal error", err)
	}
	if output.Len() != 0 {
		t.Fatalf("invalid provenance wrote %d bytes", output.Len())
	}
	if err := recorder.Close(); !errors.Is(err, errTestProvenanceMarshal) {
		t.Fatalf("Close error = %v, want sticky provenance marshal error", err)
	}
}

func TestRecorderContainsProvenanceJSONPanic(t *testing.T) {
	var output bytes.Buffer
	recorder := audit.NewRecorder(
		&output,
		audit.WithProvenance(audit.Provenance{Config: map[string]any{
			"panics": panicProvenanceValue{},
		}}),
	)
	if err := recorder.Record(t.Context(), testButtonEvent(1)); !errors.Is(
		err,
		teleop.ErrCallbackPanic,
	) {
		t.Fatalf("Record error = %v, want callback panic", err)
	}
	if output.Len() != 0 {
		t.Fatalf("panicking provenance wrote %d bytes", output.Len())
	}
}

type customProvenanceValue struct {
	values map[string]string
	calls  *int
}

func (value customProvenanceValue) MarshalJSON() ([]byte, error) {
	*value.calls = *value.calls + 1
	return json.Marshal(value.values)
}

var errTestProvenanceMarshal = errors.New("test provenance marshal failure")

type failingProvenanceValue struct{}

func (failingProvenanceValue) MarshalJSON() ([]byte, error) {
	return nil, errTestProvenanceMarshal
}

type panicProvenanceValue struct{}

func (panicProvenanceValue) MarshalJSON() ([]byte, error) {
	panic("test provenance callback fault")
}

func TestRecordCanonicalPersistsFrozenThirdPartyPayload(t *testing.T) {
	t.Parallel()

	values := []int{1, 2, 3}
	event := mutableEvent{Meta: testHeader("third-party", 1), Values: values}
	frozen, err := teleop.FreezeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	values[0] = 99
	event.Values[1] = 88

	var output bytes.Buffer
	recorder := audit.NewRecorder(&output)
	if err := recorder.RecordCanonical(t.Context(), frozen); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := audit.ReadAll(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var recorded mutableEvent
	if err := json.Unmarshal(records[0].Payload, &recorded); err != nil {
		t.Fatal(err)
	}
	if got := recorded.Values; len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("recorded values = %v, want frozen [1 2 3]", got)
	}
}

func TestRecorderRejectsInvalidOrderBeforeAdmission(t *testing.T) {
	t.Parallel()

	t.Run("sequence gap", func(t *testing.T) {
		var output bytes.Buffer
		recorder := audit.NewRecorder(&output)
		event := testButtonEvent(2)
		if err := recorder.Record(t.Context(), event); !errors.Is(err, audit.ErrInvalidEvent) {
			t.Fatalf("Record error = %v, want ErrInvalidEvent", err)
		}
		if output.Len() != 0 {
			t.Fatalf("invalid first event wrote %d bytes", output.Len())
		}
		_ = recorder.Close()
	})

	t.Run("unknown cause", func(t *testing.T) {
		var output bytes.Buffer
		recorder := audit.NewRecorder(&output)
		first := testButtonEvent(1)
		if err := recorder.Record(t.Context(), first); err != nil {
			t.Fatal(err)
		}
		second := testButtonEvent(2)
		second.Meta.Causes = []teleop.EventID{{
			Session:  second.Meta.ID.Session,
			Stream:   "missing",
			Sequence: 1,
		}}
		if err := recorder.Record(t.Context(), second); !errors.Is(err, audit.ErrInvalidEvent) {
			t.Fatalf("Record error = %v, want ErrInvalidEvent", err)
		}
		_ = recorder.Close()
	})

	t.Run("ambiguous payload", func(t *testing.T) {
		var output bytes.Buffer
		recorder := audit.NewRecorder(&output)
		event := duplicateJSONEvent{Meta: testHeader("third-party", 1)}
		if err := recorder.Record(t.Context(), event); !errors.Is(err, audit.ErrInvalidEvent) {
			t.Fatalf("Record error = %v, want ErrInvalidEvent", err)
		}
		if output.Len() != 0 {
			t.Fatalf("ambiguous event wrote %d bytes", output.Len())
		}
		_ = recorder.Close()
	})
}

func TestIdleCheckpointIsPublishedWithoutLaterEvent(t *testing.T) {
	public, private, err := audit.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	recorder := audit.NewRecorder(
		&output,
		audit.WithSigner(private),
		audit.WithAnchor(audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
			return nil
		})),
		audit.WithCheckpoints(20*time.Millisecond, 0),
	)
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	receipt, err := recorder.WaitForWitness(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Checkpoint.RecordType != "checkpoint" || receipt.Checkpoint.EventCount != 1 {
		t.Fatalf("idle receipt = %+v", receipt)
	}
	time.Sleep(60 * time.Millisecond)
	if stats := recorder.AnchorStats(); stats.Published != 2 {
		t.Fatalf("idle timer published repeated empty checkpoints: %+v", stats)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, verification, err := audit.ReadTrusted(bytes.NewReader(output.Bytes()), public); err != nil {
		t.Fatal(err)
	} else if verification.Checkpoints == 0 {
		t.Fatal("idle timer did not persist a checkpoint")
	}
}

func TestIdleTimerRetriesRecoverableCheckpointOnce(t *testing.T) {
	const interval = 15 * time.Millisecond
	transient := errors.New("temporary witness outage")
	checkpoints := make(chan audit.Checkpoint, 4)
	var checkpointCalls atomic.Int64
	var output bytes.Buffer
	recorder := audit.NewRecorder(
		&output,
		audit.WithAnchor(audit.AnchorFunc(func(
			_ context.Context,
			checkpoint audit.Checkpoint,
		) error {
			if checkpoint.RecordType != "checkpoint" {
				return nil
			}
			call := checkpointCalls.Add(1)
			checkpoints <- checkpoint
			if call == 1 {
				return transient
			}
			return nil
		})),
		audit.WithRequiredWitness(true),
		audit.WithCheckpoints(interval, 1),
	)
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var first, retry audit.Checkpoint
	select {
	case first = <-checkpoints:
	case <-ctx.Done():
		t.Fatal("initial checkpoint did not reach witness")
	}
	select {
	case retry = <-checkpoints:
	case <-ctx.Done():
		t.Fatal("failed checkpoint was not retried while idle")
	}
	if first.EventCount != 1 || retry.EventCount != first.EventCount ||
		retry.ChainHead == first.ChainHead || retry.Size <= first.Size {
		t.Fatalf("retry does not supersede failed head: first=%+v retry=%+v", first, retry)
	}

	for {
		stats := recorder.AnchorStats()
		if stats.LastReceipt != nil && stats.LastReceipt.Checkpoint.ChainHead == retry.ChainHead {
			if stats.Failed != 1 || !errors.Is(stats.Err, transient) {
				t.Fatalf("retry cleared historical degradation: %+v", stats)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("retry was not acknowledged: %+v", stats)
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(4 * interval)
	if got := checkpointCalls.Load(); got != 2 {
		t.Fatalf("idle retry calls = %d, want exactly original plus one retry", got)
	}
	if status := recorder.EvidenceStatus(); !errors.Is(status.Err, audit.ErrWitnessRequired) ||
		!errors.Is(status.Err, transient) {
		t.Fatalf("successful retry hid sticky witness degradation: %+v", status)
	}

	if err := recorder.Close(); !errors.Is(err, audit.ErrWitnessRequired) ||
		!errors.Is(err, transient) {
		t.Fatalf("Close error = %v, want sticky required-witness degradation", err)
	}
	_, verification, err := audit.Read(
		bytes.NewReader(output.Bytes()),
		audit.VerifyOptions{RequireFooter: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if verification.Checkpoints != 2 {
		t.Fatalf("local retry checkpoints = %d, want 2", verification.Checkpoints)
	}
}

func TestIdleTimerBoundsPersistentCheckpointRetries(t *testing.T) {
	const interval = 10 * time.Millisecond
	persistent := errors.New("witness remains offline")
	var checkpointCalls atomic.Int64
	recorder := audit.NewRecorder(
		io.Discard,
		audit.WithAnchor(audit.AnchorFunc(func(
			_ context.Context,
			checkpoint audit.Checkpoint,
		) error {
			if checkpoint.RecordType == "checkpoint" {
				checkpointCalls.Add(1)
				return persistent
			}
			return nil
		})),
		audit.WithCheckpoints(interval, 1),
	)
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for checkpointCalls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := checkpointCalls.Load(); got != 2 {
		t.Fatalf("checkpoint attempts = %d, want original plus one retry", got)
	}
	time.Sleep(5 * interval)
	if got := checkpointCalls.Load(); got != 2 {
		t.Fatalf("persistent outage caused %d attempts, want bounded total 2", got)
	}
	if stats := recorder.AnchorStats(); stats.Failed != 2 || !errors.Is(stats.Err, persistent) {
		t.Fatalf("persistent retry stats = %+v", stats)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredWitnessMakesCloseFailClosed(t *testing.T) {
	t.Parallel()

	witnessErr := errors.New("witness offline")
	var output bytes.Buffer
	recorder := audit.NewRecorder(
		&output,
		audit.WithAnchor(audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
			return witnessErr
		})),
		audit.WithRequiredWitness(true),
		audit.WithCheckpoints(0, 1),
	)
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	first := recorder.Close()
	if !errors.Is(first, audit.ErrWitnessRequired) || !errors.Is(first, witnessErr) {
		t.Fatalf("Close error = %v, want required witness failure", first)
	}
	if second := recorder.Close(); second.Error() != first.Error() {
		t.Fatalf("repeated Close = %v, want %v", second, first)
	}
	if _, err := audit.ReadAll(bytes.NewReader(output.Bytes())); err != nil {
		t.Fatalf("local log should remain complete: %v", err)
	}
}

func TestPanickingAnchorBecomesStickyFailureAndReleasesWaiter(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	recorder := audit.NewRecorder(
		&output,
		audit.WithAnchor(audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
			panic("witness adapter fault")
		})),
		audit.WithRequiredWitness(true),
		audit.WithCheckpoints(0, 1),
	)
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := recorder.WaitForWitness(waitCtx, 1); !errors.Is(err, audit.ErrWitnessRequired) ||
		!errors.Is(err, teleop.ErrCallbackPanic) {
		t.Fatalf("WaitForWitness error = %v, want required-witness callback panic", err)
	}
	stats := recorder.AnchorStats()
	if stats.Failed == 0 || !errors.Is(stats.Err, teleop.ErrCallbackPanic) {
		t.Fatalf("anchor stats = %+v, want sticky callback-panic failure", stats)
	}
	if err := recorder.Close(); !errors.Is(err, audit.ErrWitnessRequired) ||
		!errors.Is(err, teleop.ErrCallbackPanic) {
		t.Fatalf("Close error = %v, want required-witness callback panic", err)
	}
}

func TestBlockingAnchorCannotHangRequiredWitnessClose(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, 8)
	var calls atomic.Int64
	recorder := audit.NewRecorder(
		&bytes.Buffer{},
		audit.WithAnchor(audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
			calls.Add(1)
			started <- struct{}{}
			<-release // Deliberately ignore context cancellation.
			return nil
		})),
		audit.WithAnchorTimeout(10*time.Millisecond),
		audit.WithRequiredWitness(true),
		audit.WithCheckpoints(5*time.Millisecond, 1),
	)
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("anchor publication did not start")
	}
	deadline := time.Now().Add(time.Second)
	for recorder.AnchorStats().Failed == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if recorder.AnchorStats().Failed == 0 {
		t.Fatal("blocking anchor did not reach its timeout")
	}
	for index := range 64 {
		if err := recorder.Checkpoint(fmt.Sprintf("after timeout %d", index)); err != nil {
			t.Fatal(err)
		}
	}

	closed := make(chan error, 1)
	go func() { closed <- recorder.Close() }()
	select {
	case err := <-closed:
		if !errors.Is(err, audit.ErrWitnessRequired) ||
			!errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close error = %v, want bounded required-witness timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close hung on an Anchor that ignored context cancellation")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("blocking Anchor invoked %d times after timeout, want one bounded worker", got)
	}
}

func TestFileAnchorRejectsNilWriter(t *testing.T) {
	t.Parallel()

	anchor := audit.NewFileAnchor(nil)
	if err := anchor.Publish(t.Context(), audit.Checkpoint{}); err == nil {
		t.Fatal("nil writer unexpectedly accepted")
	}
}

type shortCheckpointWriter struct{}

func (shortCheckpointWriter) Write(payload []byte) (int, error) {
	return max(len(payload)-1, 0), nil
}

func TestFileAnchorRejectsShortWrite(t *testing.T) {
	t.Parallel()

	anchor := audit.NewFileAnchor(shortCheckpointWriter{})
	if err := anchor.Publish(t.Context(), audit.Checkpoint{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Publish error = %v, want io.ErrShortWrite", err)
	}
}

func TestFileAnchorFlushesBufferedCheckpoint(t *testing.T) {
	t.Parallel()

	var destination bytes.Buffer
	buffered := bufio.NewWriter(&destination)
	anchor := audit.NewFileAnchor(buffered)
	if err := anchor.Publish(t.Context(), audit.Checkpoint{}); err != nil {
		t.Fatal(err)
	}
	if destination.Len() == 0 {
		t.Fatal("successful Publish left the checkpoint in the writer buffer")
	}
}

type orderedAnchorWriter struct {
	steps      []string
	flushErr   error
	panicFlush bool
}

func (writer *orderedAnchorWriter) Write(payload []byte) (int, error) {
	writer.steps = append(writer.steps, "write")
	return len(payload), nil
}

func (writer *orderedAnchorWriter) Flush() error {
	writer.steps = append(writer.steps, "flush")
	if writer.panicFlush {
		panic("flush callback fault")
	}
	return writer.flushErr
}

func (writer *orderedAnchorWriter) Sync() error {
	writer.steps = append(writer.steps, "sync")
	return nil
}

func TestFileAnchorFlushesBeforeSync(t *testing.T) {
	t.Parallel()

	writer := &orderedAnchorWriter{}
	anchor := audit.NewFileAnchor(writer)
	if err := anchor.Publish(t.Context(), audit.Checkpoint{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"write", "flush", "sync"}
	if !slices.Equal(writer.steps, want) {
		t.Fatalf("writer calls = %v, want %v", writer.steps, want)
	}
}

func TestFileAnchorReportsFlushFailureBeforeSync(t *testing.T) {
	t.Parallel()

	flushErr := errors.New("flush failed")
	writer := &orderedAnchorWriter{flushErr: flushErr}
	anchor := audit.NewFileAnchor(writer)
	if err := anchor.Publish(t.Context(), audit.Checkpoint{}); !errors.Is(err, flushErr) {
		t.Fatalf("Publish error = %v, want flush failure", err)
	}
	want := []string{"write", "flush"}
	if !slices.Equal(writer.steps, want) {
		t.Fatalf("writer calls = %v, want %v", writer.steps, want)
	}
}

func TestFileAnchorContainsFlushPanic(t *testing.T) {
	t.Parallel()

	writer := &orderedAnchorWriter{panicFlush: true}
	anchor := audit.NewFileAnchor(writer)
	if err := anchor.Publish(t.Context(), audit.Checkpoint{}); !errors.Is(
		err,
		teleop.ErrCallbackPanic,
	) {
		t.Fatalf("Publish error = %v, want callback panic", err)
	}
	want := []string{"write", "flush"}
	if !slices.Equal(writer.steps, want) {
		t.Fatalf("writer calls = %v, want %v", writer.steps, want)
	}
}

func TestRequiredWitnessAcceptsAcknowledgedFooter(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	recorder := audit.NewRecorder(
		&output,
		audit.WithAnchor(audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
			return nil
		})),
		audit.WithRequiredWitness(true),
	)
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	status := recorder.EvidenceStatus()
	if status.LastWitness == nil || status.LastWitness.Checkpoint.RecordType != "footer" {
		t.Fatalf("final witness = %+v", status.LastWitness)
	}
}

func TestWaitForWitnessReturnsFailureAndClosure(t *testing.T) {
	t.Parallel()

	t.Run("publication failure", func(t *testing.T) {
		witnessErr := errors.New("witness refused")
		recorder := audit.NewRecorder(
			&bytes.Buffer{},
			audit.WithAnchor(audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
				return witnessErr
			})),
			audit.WithCheckpoints(0, 1),
		)
		if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := recorder.WaitForWitness(ctx, 1); !errors.Is(err, audit.ErrWitnessRequired) ||
			!errors.Is(err, witnessErr) {
			t.Fatalf("WaitForWitness error = %v", err)
		}
		_ = recorder.Close()
	})

	t.Run("recorder closed below target", func(t *testing.T) {
		recorder := audit.NewRecorder(
			&bytes.Buffer{},
			audit.WithAnchor(audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
				return nil
			})),
			audit.WithCheckpoints(0, 0),
		)
		if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
			t.Fatal(err)
		}
		if err := recorder.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := recorder.WaitForWitness(t.Context(), 2); !errors.Is(
			err,
			audit.ErrWitnessRequired,
		) {
			t.Fatalf("WaitForWitness error = %v", err)
		}
	})
}

func TestEvidenceStatusSeparatesAcceptedDurableAndWitnessed(t *testing.T) {
	t.Parallel()

	output := &syncBuffer{}
	recorder := audit.NewRecorder(
		output,
		audit.WithAnchor(audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
			return nil
		})),
		audit.WithCheckpoints(0, 1),
	)
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := recorder.WaitForWitness(ctx, 1); err != nil {
		t.Fatal(err)
	}
	status := recorder.EvidenceStatus()
	if status.AcceptedEvents != 1 || status.LocallyDurableEvents != 1 ||
		status.WitnessedEvents != 1 || status.WitnessedTreeSize == 0 {
		t.Fatalf("evidence status = %+v", status)
	}
	status.LastWitness.Checkpoint.EventCount = 999
	if next := recorder.EvidenceStatus(); next.LastWitness.Checkpoint.EventCount != 1 {
		t.Fatalf("caller mutated internal receipt: %+v", next.LastWitness)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if status := recorder.EvidenceStatus(); !status.Closed || status.Err != nil {
		t.Fatalf("closed evidence status = %+v", status)
	}
}

type mutableEvent struct {
	Meta   teleop.Header `json:"header"`
	Values []int         `json:"values"`
}

func (e mutableEvent) Header() teleop.Header { return e.Meta.Clone() }
func (mutableEvent) Kind() teleop.EventKind  { return "third-party.mutable" }

type duplicateJSONEvent struct {
	Meta teleop.Header
}

func (e duplicateJSONEvent) Header() teleop.Header { return e.Meta.Clone() }
func (duplicateJSONEvent) Kind() teleop.EventKind  { return "third-party.duplicate" }
func (e duplicateJSONEvent) MarshalJSON() ([]byte, error) {
	header, err := json.Marshal(e.Meta)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`{"header":%s,"value":1,"value":2}`, header)), nil
}

type syncBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (buffer *syncBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.Write(value)
}

func (buffer *syncBuffer) Sync() error { return nil }
