package teleop_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/testkit"
)

func TestRecordCommandSyncWaitsForEverySinkAndPriorFIFOEvents(t *testing.T) {
	first := newSyncOrderingSink("prior", "current")
	second := newSyncOrderingSink("prior", "current")
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "command-sync-order"}, 4)
	controller, err := teleop.NewController(
		source,
		teleop.WithAuditSink(first),
		teleop.WithAuditSink(second),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		first.releaseAll()
		second.releaseAll()
		_ = controller.Close()
	})
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 32})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := controller.RecordCommand(ctx, teleop.Command{Name: "prior"}); err != nil {
		t.Fatal(err)
	}
	priorFirst := waitForSyncGate(t, first.gate("prior").entered)
	priorSecond := waitForSyncGate(t, second.gate("prior").entered)
	if priorFirst.Meta.ID != priorSecond.Meta.ID {
		t.Fatalf("prior command IDs differ: %v and %v", priorFirst.Meta.ID, priorSecond.Meta.ID)
	}

	result := make(chan syncCommandResult, 1)
	go func() {
		id, recordErr := controller.RecordCommandSync(ctx, teleop.Command{Name: "current"})
		result <- syncCommandResult{id: id, err: recordErr}
	}()
	assertSyncCommandPending(t, result, "prior sink callbacks are blocked")

	first.gate("prior").release()
	second.gate("prior").release()
	waitForSyncExit(t, first.gate("prior").exited)
	waitForSyncExit(t, second.gate("prior").exited)
	currentFirst := waitForSyncGate(t, first.gate("current").entered)
	currentSecond := waitForSyncGate(t, second.gate("current").entered)

	first.gate("current").release()
	waitForSyncExit(t, first.gate("current").exited)
	assertSyncCommandPending(t, result, "second sink callback is blocked")
	second.gate("current").release()
	waitForSyncExit(t, second.gate("current").exited)

	var got syncCommandResult
	select {
	case got = <-result:
	case <-ctx.Done():
		t.Fatalf("RecordCommandSync did not return: %v", ctx.Err())
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.id == (teleop.EventID{}) || got.id.Stream != "command" || got.id.Sequence != 2 {
		t.Fatalf("returned command ID = %v, want command sequence 2", got.id)
	}
	if got.id != currentFirst.Meta.ID || got.id != currentSecond.Meta.ID {
		t.Fatalf(
			"returned ID %v differs from sink events %v and %v",
			got.id,
			currentFirst.Meta.ID,
			currentSecond.Meta.ID,
		)
	}
	if names := first.commandNames(); !equalSyncNames(names, []string{"prior", "current"}) {
		t.Fatalf("first sink command order = %v", names)
	}
	if names := second.commandNames(); !equalSyncNames(names, []string{"prior", "current"}) {
		t.Fatalf("second sink command order = %v", names)
	}

	for {
		event, nextErr := subscription.Next(ctx)
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		command, ok := event.(teleop.CommandEvent)
		if !ok || command.Command != "current" {
			continue
		}
		if command.Meta.ID != got.id {
			t.Fatalf("subscriber command ID = %v, want %v", command.Meta.ID, got.id)
		}
		break
	}
}

func TestRecordCommandSyncPropagatesSinkFailureWithoutSubscriberExposure(t *testing.T) {
	wantErr := errors.New("witness store unavailable")
	sink := &syncFailingCommandSink{command: "unsafe.thrust", err: wantErr}
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "command-sync-failure"}, 4)
	controller, err := teleop.NewController(source, teleop.WithAuditSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 32})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	id, recordErr := controller.RecordCommandSync(ctx, teleop.Command{Name: "unsafe.thrust"})
	if !errors.Is(recordErr, wantErr) {
		t.Fatalf("RecordCommandSync error = %v, want %v", recordErr, wantErr)
	}
	recorded := sink.failedCommand()
	if id == (teleop.EventID{}) || recorded.Meta.ID != id {
		t.Fatalf("returned ID = %v, failed sink command ID = %v", id, recorded.Meta.ID)
	}

	select {
	case <-controller.Done():
	case <-ctx.Done():
		t.Fatalf("controller did not terminate after sink failure: %v", ctx.Err())
	}
	for {
		event, nextErr := subscription.Next(ctx)
		if nextErr != nil {
			break
		}
		if command, ok := event.(teleop.CommandEvent); ok && command.Command == "unsafe.thrust" {
			t.Fatalf("failed command was exposed to subscriber: %#v", command)
		}
	}
}

func TestControllerHandsCanonicalSinkImmutableThirdPartyBytes(t *testing.T) {
	const derivedKind teleop.EventKind = "test.command.mutable"
	processor := &syncMutableCommandProcessor{
		kind:     derivedKind,
		produced: make(chan *syncMutableCommandEvent, 1),
	}
	sink := &syncCanonicalCaptureSink{
		kind:     derivedKind,
		entered:  make(chan teleop.CanonicalEvent, 1),
		release:  make(chan struct{}),
		captured: make(chan []byte, 1),
	}
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "canonical-command"}, 4)
	controller, err := teleop.NewController(
		source,
		teleop.WithAuditSink(sink),
		teleop.WithProcessor(processor),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sink.unblock()
		_ = controller.Close()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := controller.RecordCommandSync(ctx, teleop.Command{Name: "derive.mutable"}); err != nil {
		t.Fatal(err)
	}
	var original *syncMutableCommandEvent
	select {
	case original = <-processor.produced:
	case <-ctx.Done():
		t.Fatalf("processor did not produce mutable event: %v", ctx.Err())
	}
	var admitted teleop.CanonicalEvent
	select {
	case admitted = <-sink.entered:
	case <-ctx.Done():
		t.Fatalf("canonical sink did not receive mutable event: %v", ctx.Err())
	}
	originalID := original.Meta.ID
	original.Values[0] = "mutated-after-admission"
	original.Meta.ID.Sequence = 999
	sink.unblock()

	var payload []byte
	select {
	case payload = <-sink.captured:
	case <-ctx.Done():
		t.Fatalf("canonical sink did not finish capture: %v", ctx.Err())
	}
	if admitted.Header().ID != originalID {
		t.Fatalf("canonical header ID = %v, want %v", admitted.Header().ID, originalID)
	}
	var decoded syncMutableCommandEvent
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Meta.ID != originalID || len(decoded.Values) != 1 || decoded.Values[0] != "original" {
		t.Fatalf("canonical payload changed after admission: %#v", decoded)
	}
}

func TestRecordCommandSyncReturnsAfterAuditLocalDurability(t *testing.T) {
	writer := &syncDurabilityWriter{}
	recorder := audit.NewRecorder(writer)
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "command-sync-durable"}, 4)
	controller, err := teleop.NewController(source, teleop.WithAuditSink(recorder))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = controller.Close()
		_ = recorder.Close()
	})

	baseline := waitForDurableEvents(t, recorder, 2)
	gate := writer.blockNextSync()
	t.Cleanup(gate.release)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan syncCommandResult, 1)
	go func() {
		id, recordErr := controller.RecordCommandSync(ctx, teleop.Command{Name: "durable.thrust"})
		result <- syncCommandResult{id: id, err: recordErr}
	}()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatalf("audit writer did not reach Sync: %v", ctx.Err())
	}
	assertSyncCommandPending(t, result, "audit Sync is blocked")
	gate.release()

	var got syncCommandResult
	select {
	case got = <-result:
	case <-ctx.Done():
		t.Fatalf("RecordCommandSync did not return after Sync: %v", ctx.Err())
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	status := recorder.EvidenceStatus()
	if status.AcceptedEvents < baseline+1 ||
		status.LocallyDurableEvents < baseline+1 ||
		status.LocallyDurableEvents != status.AcceptedEvents {
		t.Fatalf("evidence status after synchronous command = %+v, baseline %d", status, baseline)
	}
	if status.Durability != "fsync-every-record" {
		t.Fatalf("durability = %q, want fsync-every-record", status.Durability)
	}

	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordCommandSyncWaitsForCommandQueueSpace(t *testing.T) {
	sink := newSyncOrderingSink("running", "queued", "waiting")
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "command-sync-space"}, 4)
	controller, err := teleop.NewController(
		source,
		teleop.WithAuditSink(sink),
		teleop.WithSynchronousAudit(),
		teleop.WithPipelineBuffers(1, 8),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sink.releaseAll()
		_ = controller.Close()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := controller.RecordCommand(ctx, teleop.Command{Name: "running"}); err != nil {
		t.Fatal(err)
	}
	waitForSyncGate(t, sink.gate("running").entered)
	if err := controller.RecordCommand(ctx, teleop.Command{Name: "queued"}); err != nil {
		t.Fatal(err)
	}

	result := make(chan syncCommandResult, 1)
	go func() {
		id, recordErr := controller.RecordCommandSync(
			ctx,
			teleop.Command{Name: "waiting"},
		)
		result <- syncCommandResult{id: id, err: recordErr}
	}()
	assertSyncCommandPending(t, result, "the command queue is full")

	sink.gate("running").release()
	waitForSyncExit(t, sink.gate("running").exited)
	waitForSyncGate(t, sink.gate("queued").entered)
	assertSyncCommandPending(t, result, "the preceding queued command is blocked")
	sink.gate("queued").release()
	waitForSyncExit(t, sink.gate("queued").exited)
	waiting := waitForSyncGate(t, sink.gate("waiting").entered)
	sink.gate("waiting").release()
	waitForSyncExit(t, sink.gate("waiting").exited)

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.id != waiting.Meta.ID {
			t.Fatalf("result ID = %v, sink ID = %v", got.id, waiting.Meta.ID)
		}
	case <-ctx.Done():
		t.Fatalf("RecordCommandSync did not resume after queue space: %v", ctx.Err())
	}
}

func TestRecordCommandSyncCancellationAfterAdmissionIsExplicitlyUncertain(t *testing.T) {
	sink := newSyncOrderingSink("cancelled.wait")
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "command-sync-cancel"}, 4)
	controller, err := teleop.NewController(source, teleop.WithAuditSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sink.releaseAll()
		_ = controller.Close()
	})
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 32})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan syncCommandResult, 1)
	go func() {
		id, recordErr := controller.RecordCommandSync(ctx, teleop.Command{Name: "cancelled.wait"})
		result <- syncCommandResult{id: id, err: recordErr}
	}()
	admitted := waitForSyncGate(t, sink.gate("cancelled.wait").entered)
	cancel()
	got := <-result
	if got.id != (teleop.EventID{}) ||
		!errors.Is(got.err, context.Canceled) ||
		!errors.Is(got.err, teleop.ErrCommandPublicationUncertain) {
		t.Fatalf("cancelled result = %+v", got)
	}

	// Cancellation bounds the caller's wait; it cannot retract work already
	// admitted to the controller. Releasing the sink proves the same command can
	// still become visible, which is why the explicit uncertainty marker matters.
	sink.gate("cancelled.wait").release()
	waitForSyncExit(t, sink.gate("cancelled.wait").exited)
	nextCtx, cancelNext := context.WithTimeout(t.Context(), time.Second)
	defer cancelNext()
	for {
		event, nextErr := subscription.Next(nextCtx)
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		command, ok := event.(teleop.CommandEvent)
		if ok && command.Command == "cancelled.wait" {
			if command.Meta.ID != admitted.Meta.ID {
				t.Fatalf("published ID = %v, admitted ID = %v", command.Meta.ID, admitted.Meta.ID)
			}
			break
		}
	}
}

func TestAuditRecorderRejectsNilRecordContexts(t *testing.T) {
	event := teleop.ButtonEvent{Meta: teleop.Header{ID: teleop.EventID{
		Session:  teleop.SessionID{1},
		Stream:   "input",
		Sequence: 1,
	}}}
	canonical, err := teleop.FreezeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	for name, record := range map[string]func(*audit.Recorder) error{
		"event": func(recorder *audit.Recorder) error {
			return recorder.Record(nil, event)
		},
		"canonical event": func(recorder *audit.Recorder) error {
			return recorder.RecordCanonical(nil, canonical)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			recorder := audit.NewRecorder(&output)
			if err := record(recorder); !errors.Is(err, teleop.ErrInvalidState) {
				t.Fatalf("nil-context error = %v, want ErrInvalidState", err)
			}
			if err := recorder.Record(t.Context(), event); err != nil {
				t.Fatalf("nil-context rejection poisoned recorder: %v", err)
			}
			if err := recorder.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type syncCommandResult struct {
	id  teleop.EventID
	err error
}

type syncCommandGate struct {
	entered     chan teleop.CommandEvent
	releaseCh   chan struct{}
	exited      chan struct{}
	releaseOnce sync.Once
}

func newSyncCommandGate() *syncCommandGate {
	return &syncCommandGate{
		entered:   make(chan teleop.CommandEvent, 1),
		releaseCh: make(chan struct{}),
		exited:    make(chan struct{}),
	}
}

func (gate *syncCommandGate) release() {
	gate.releaseOnce.Do(func() { close(gate.releaseCh) })
}

type syncOrderingSink struct {
	mu       sync.Mutex
	gates    map[string]*syncCommandGate
	commands []string
}

func newSyncOrderingSink(commands ...string) *syncOrderingSink {
	sink := &syncOrderingSink{gates: make(map[string]*syncCommandGate, len(commands))}
	for _, command := range commands {
		sink.gates[command] = newSyncCommandGate()
	}
	return sink
}

func (sink *syncOrderingSink) Record(ctx context.Context, event teleop.Event) error {
	command, ok := event.(teleop.CommandEvent)
	if !ok {
		return nil
	}
	sink.mu.Lock()
	sink.commands = append(sink.commands, command.Command)
	gate := sink.gates[command.Command]
	sink.mu.Unlock()
	if gate == nil {
		return nil
	}
	gate.entered <- command
	defer close(gate.exited)
	select {
	case <-gate.releaseCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sink *syncOrderingSink) gate(command string) *syncCommandGate {
	return sink.gates[command]
}

func (sink *syncOrderingSink) commandNames() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]string(nil), sink.commands...)
}

func (sink *syncOrderingSink) releaseAll() {
	for _, gate := range sink.gates {
		gate.release()
	}
}

type syncFailingCommandSink struct {
	mu      sync.Mutex
	command string
	err     error
	failed  teleop.CommandEvent
}

func (sink *syncFailingCommandSink) Record(_ context.Context, event teleop.Event) error {
	command, ok := event.(teleop.CommandEvent)
	if !ok || command.Command != sink.command {
		return nil
	}
	sink.mu.Lock()
	sink.failed = command
	sink.mu.Unlock()
	return sink.err
}

func (sink *syncFailingCommandSink) failedCommand() teleop.CommandEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.failed
}

type syncMutableCommandEvent struct {
	Meta   teleop.Header `json:"header"`
	Values []string      `json:"values"`
}

func (event *syncMutableCommandEvent) Header() teleop.Header { return event.Meta.Clone() }
func (*syncMutableCommandEvent) Kind() teleop.EventKind      { return "test.command.mutable" }

type syncMutableCommandProcessor struct {
	kind     teleop.EventKind
	produced chan *syncMutableCommandEvent
}

func (*syncMutableCommandProcessor) Process(teleop.Event) []teleop.Event { return nil }

func (processor *syncMutableCommandProcessor) ProcessContext(
	_ context.Context,
	processing teleop.ProcessingContext,
	event teleop.Event,
) ([]teleop.Event, error) {
	command, ok := event.(teleop.CommandEvent)
	if !ok || command.Command != "derive.mutable" {
		return nil, nil
	}
	header := command.Header()
	derived := &syncMutableCommandEvent{
		Meta: processing.NewHeader(
			"test-derived",
			header.ObservedAt,
			0,
			header.ID,
		),
		Values: []string{"original"},
	}
	if derived.Kind() != processor.kind {
		return nil, fmt.Errorf("derived kind = %q, want %q", derived.Kind(), processor.kind)
	}
	processor.produced <- derived
	return []teleop.Event{derived}, nil
}

type syncCanonicalCaptureSink struct {
	kind        teleop.EventKind
	entered     chan teleop.CanonicalEvent
	release     chan struct{}
	captured    chan []byte
	releaseOnce sync.Once
}

func (*syncCanonicalCaptureSink) Record(context.Context, teleop.Event) error {
	return errors.New("non-canonical Record called for CanonicalEventSink")
}

func (sink *syncCanonicalCaptureSink) RecordCanonical(
	ctx context.Context,
	event teleop.CanonicalEvent,
) error {
	if event.Kind() != sink.kind {
		return nil
	}
	select {
	case sink.entered <- event:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-sink.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	payload := event.JSON()
	select {
	case sink.captured <- payload:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sink *syncCanonicalCaptureSink) unblock() {
	sink.releaseOnce.Do(func() { close(sink.release) })
}

type syncDurabilityGate struct {
	entered     chan struct{}
	releaseCh   chan struct{}
	releaseOnce sync.Once
}

func (gate *syncDurabilityGate) release() {
	gate.releaseOnce.Do(func() { close(gate.releaseCh) })
}

type syncDurabilityWriter struct {
	mu   sync.Mutex
	data bytes.Buffer
	next *syncDurabilityGate
}

func (writer *syncDurabilityWriter) Write(payload []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.data.Write(payload)
}

func (writer *syncDurabilityWriter) Sync() error {
	writer.mu.Lock()
	gate := writer.next
	writer.next = nil
	writer.mu.Unlock()
	if gate == nil {
		return nil
	}
	close(gate.entered)
	<-gate.releaseCh
	return nil
}

func (writer *syncDurabilityWriter) blockNextSync() *syncDurabilityGate {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.next != nil {
		panic("sync gate already armed")
	}
	gate := &syncDurabilityGate{
		entered:   make(chan struct{}),
		releaseCh: make(chan struct{}),
	}
	writer.next = gate
	return gate
}

func waitForSyncGate(t *testing.T, entered <-chan teleop.CommandEvent) teleop.CommandEvent {
	t.Helper()
	select {
	case command := <-entered:
		return command
	case <-time.After(time.Second):
		t.Fatal("sink callback did not reach command gate")
		return teleop.CommandEvent{}
	}
}

func waitForSyncExit(t *testing.T, exited <-chan struct{}) {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("sink callback did not exit")
	}
}

func assertSyncCommandPending(t *testing.T, result <-chan syncCommandResult, reason string) {
	t.Helper()
	select {
	case got := <-result:
		t.Fatalf("RecordCommandSync returned while %s: id=%v err=%v", reason, got.id, got.err)
	case <-time.After(25 * time.Millisecond):
	}
}

func equalSyncNames(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func waitForDurableEvents(t *testing.T, recorder *audit.Recorder, want uint64) uint64 {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := recorder.EvidenceStatus()
		if status.AcceptedEvents >= want && status.LocallyDurableEvents == status.AcceptedEvents {
			return status.AcceptedEvents
		}
		time.Sleep(time.Millisecond)
	}
	status := recorder.EvidenceStatus()
	t.Fatalf("durable event count did not reach %d: %+v", want, status)
	return 0
}
