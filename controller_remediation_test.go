package teleop_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/gesture"
	"github.com/open-ships/teleop/testkit"
)

func TestDisconnectNeutralizesStateAndClosesControlEdges(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "disconnect"}, 8)
	controller, err := teleop.NewController(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 64})
	if err != nil {
		t.Fatal(err)
	}

	active := teleop.State{
		LeftStick:    teleop.Stick{Y: 1},
		RightTrigger: 1,
	}
	active.SetButton(teleop.ButtonFaceSouth, true)
	if err := source.Push(context.Background(), active); err != nil {
		t.Fatal(err)
	}
	waitForSnapshot(t, controller, func(state teleop.State, _ teleop.StateMeta) bool {
		return state.LeftStick.Y == 1 &&
			state.RightTrigger == 1 &&
			state.Button(teleop.ButtonFaceSouth)
	})
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("controller did not terminate after disconnect")
	}

	state, meta := controller.SnapshotWithMeta()
	if !reflect.DeepEqual(state, teleop.State{}) {
		t.Fatalf("snapshot after disconnect = %#v, want neutral", state)
	}
	if meta.Connected || !meta.Stale || !meta.Synthetic {
		t.Fatalf("metadata after disconnect = %#v", meta)
	}
	if !errors.Is(controller.Err(), teleop.ErrDisconnected) {
		t.Fatalf("terminal error = %v, want ErrDisconnected", controller.Err())
	}

	var (
		releasedButton  bool
		centeredStick   bool
		releasedTrigger bool
		disconnected    bool
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		event, nextErr := subscription.Next(ctx)
		if nextErr != nil {
			if !errors.Is(nextErr, teleop.ErrDisconnected) {
				t.Fatalf("subscription error = %v", nextErr)
			}
			break
		}
		switch value := event.(type) {
		case teleop.ButtonEvent:
			releasedButton = releasedButton ||
				value.Button == teleop.ButtonFaceSouth &&
					value.Phase == teleop.PhaseReleased
		case teleop.StickEvent:
			centeredStick = centeredStick ||
				value.Stick == teleop.LeftStick &&
					value.Position == (teleop.Stick{})
		case teleop.TriggerEvent:
			releasedTrigger = releasedTrigger ||
				value.Trigger == teleop.RightTrigger &&
					value.Position == 0
		case teleop.ConnectionEvent:
			disconnected = disconnected || value.State == teleop.Disconnected
		}
	}
	if !releasedButton || !centeredStick || !releasedTrigger || !disconnected {
		t.Fatalf(
			"closing events: button=%t stick=%t trigger=%t disconnected=%t",
			releasedButton,
			centeredStick,
			releasedTrigger,
			disconnected,
		)
	}
}

func TestControllerContextOwnsSessionLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "context-lifetime"}, 4)
	controller, err := teleop.NewController(source, teleop.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	cancel()
	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("controller remained open after its context was canceled")
	}
	if !errors.Is(controller.Err(), teleop.ErrClosed) {
		t.Fatalf("terminal error = %v, want ErrClosed", controller.Err())
	}
}

func TestDeferredStartAttachesBeforeFiniteReplayBegins(t *testing.T) {
	source := testkit.NewReplaySource(
		teleop.Descriptor{ID: "replay-deferred"},
		[]teleop.Observation{
			{State: teleop.State{LeftTrigger: 0.25}, ObservedAt: time.Unix(1, 0)},
			{State: teleop.State{LeftTrigger: 0.75}, ObservedAt: time.Unix(2, 0)},
		},
	)
	controller, err := teleop.NewController(source, teleop.WithDeferredStart())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	select {
	case <-controller.Done():
		t.Fatal("deferred controller started before a consumer attached")
	case <-time.After(15 * time.Millisecond):
	}

	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 32})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	observations := 0
	for {
		event, err := subscription.Next(ctx)
		if err != nil {
			if !errors.Is(err, teleop.ErrDisconnected) {
				t.Fatal(err)
			}
			break
		}
		if observation, ok := event.(teleop.ObservationEvent); ok &&
			!observation.Header().Synthetic {
			observations++
		}
	}
	if observations != 2 {
		t.Fatalf("replayed observations = %d, want 2", observations)
	}
}

func TestDisconnectLetsHealthyProcessorsCloseActiveGestures(t *testing.T) {
	config := gesture.DefaultConfig()
	config.TapMaximum = 10 * time.Millisecond
	config.HoldMinimum = 10 * time.Millisecond
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "gesture-disconnect"}, 8)
	controller, err := teleop.NewController(
		source,
		teleop.WithProcessor(gesture.New(config)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 64})
	if err != nil {
		t.Fatal(err)
	}
	state := teleop.State{}
	state.SetButton(teleop.ButtonFaceSouth, true)
	if err := source.Push(context.Background(), state); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		recognized, ok := event.(gesture.Event)
		if ok && recognized.Type == gesture.Hold &&
			recognized.Phase == teleop.PhaseStarted {
			break
		}
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	foundEnded := false
	for {
		event, err := subscription.Next(ctx)
		if err != nil {
			if !errors.Is(err, teleop.ErrDisconnected) {
				t.Fatal(err)
			}
			break
		}
		recognized, ok := event.(gesture.Event)
		foundEnded = foundEnded || ok &&
			recognized.Type == gesture.Hold &&
			recognized.Phase == teleop.PhaseEnded
	}
	if !foundEnded {
		t.Fatal("active hold did not end during disconnect neutralization")
	}
}

func TestSubscriptionCloseBroadcastsToAllBlockedReaders(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "broadcast"}, 4)
	controller, err := teleop.NewController(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	waitForSnapshot(t, controller, func(_ teleop.State, meta teleop.StateMeta) bool {
		return meta.Connected
	})
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 16})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for range 2 {
		if _, err := subscription.Next(ctx); err != nil {
			t.Fatal(err)
		}
	}

	started := make(chan struct{}, 4)
	finished := make(chan error, 4)
	for range 4 {
		go func() {
			started <- struct{}{}
			_, err := subscription.Next(context.Background())
			finished <- err
		}()
	}
	for range 4 {
		<-started
	}
	time.Sleep(10 * time.Millisecond)
	if err := subscription.Close(); err != nil {
		t.Fatal(err)
	}
	for index := range 4 {
		select {
		case err := <-finished:
			if !errors.Is(err, teleop.ErrClosed) {
				t.Fatalf("reader error = %v, want ErrClosed", err)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("reader %d remained blocked after subscription close", index)
		}
	}
}

func TestLosslessOverflowPreservesBufferedPrefix(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "overflow-prefix"}, 4)
	controller, err := teleop.NewController(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	waitForSnapshot(t, controller, func(_ teleop.State, meta teleop.StateMeta) bool {
		return meta.Connected
	})
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{
		Delivery: teleop.DeliveryLossless,
		Buffer:   3,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := teleop.State{}
	state.SetButton(teleop.ButtonFaceSouth, true)
	if err := source.Push(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !subscription.Stats().Closed && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !subscription.Stats().Closed {
		t.Fatal("lossless subscription did not close on overflow")
	}
	select {
	case <-controller.Done():
		t.Fatalf("subscription overflow terminated controller: %v", controller.Err())
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	kinds := make([]teleop.EventKind, 0, 3)
	for {
		event, nextErr := subscription.Next(ctx)
		if nextErr != nil {
			if !errors.Is(nextErr, teleop.ErrSubscriptionOverflow) {
				t.Fatalf("subscription error = %v", nextErr)
			}
			break
		}
		kinds = append(kinds, event.Kind())
	}
	want := []teleop.EventKind{
		teleop.EventConnection,
		teleop.EventCapabilities,
		teleop.EventObservation,
	}
	if len(kinds) != len(want) {
		t.Fatalf("buffered kinds = %v, want %v", kinds, want)
	}
	for index := range want {
		if kinds[index] != want[index] {
			t.Fatalf("buffered kinds = %v, want %v", kinds, want)
		}
	}
}

func TestLatestOverflowRetainsRealLatestEventAndAuditsLoss(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "latest-overflow"}, 4)
	audited := make(chan teleop.Event, 32)
	controller, err := teleop.NewController(
		source,
		teleop.WithAuditSink(channelSink{events: audited}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	waitForSnapshot(t, controller, func(_ teleop.State, meta teleop.StateMeta) bool {
		return meta.Connected
	})
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{
		Delivery: teleop.DeliveryLatest,
		Buffer:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Push(context.Background(), teleop.State{LeftTrigger: 1}); err != nil {
		t.Fatal(err)
	}
	waitForSnapshot(t, controller, func(state teleop.State, _ teleop.StateMeta) bool {
		return state.LeftTrigger == 1
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := event.(teleop.GapEvent); ok {
			t.Fatalf("subscription diagnostic displaced the real latest event: %#v", event)
		}
		trigger, ok := event.(teleop.TriggerEvent)
		if ok && trigger.Trigger == teleop.LeftTrigger && trigger.Position == 1 {
			break
		}
	}
	if subscription.Stats().Dropped == 0 {
		t.Fatal("latest subscription did not account for coalesced events")
	}

	for {
		select {
		case event := <-audited:
			if gap, ok := event.(teleop.GapEvent); ok &&
				gap.Source == "subscription" &&
				gap.Dropped > 0 {
				return
			}
		case <-ctx.Done():
			t.Fatal("subscription loss was not recorded to the audit sink")
		}
	}
}

type panicOnObservation struct{}

func (panicOnObservation) Process(event teleop.Event) []teleop.Event {
	if event.Kind() == teleop.EventObservation {
		panic("processor bug")
	}
	return nil
}

type blockingObservationProcessor struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu               sync.Mutex
	observationCalls int
}

func (processor *blockingObservationProcessor) Process(event teleop.Event) []teleop.Event {
	if event.Kind() != teleop.EventObservation {
		return nil
	}
	processor.mu.Lock()
	processor.observationCalls++
	processor.mu.Unlock()
	processor.once.Do(func() { close(processor.started) })
	<-processor.release
	return nil
}

func (processor *blockingObservationProcessor) calls() int {
	processor.mu.Lock()
	defer processor.mu.Unlock()
	return processor.observationCalls
}

type panicSource struct{}

func (panicSource) Descriptor() teleop.Descriptor {
	return teleop.Descriptor{ID: "source-panic"}
}

func (panicSource) Read(context.Context) (teleop.Observation, error) {
	panic("backend bug")
}

func (panicSource) Close() error { return nil }

var errSourceClose = errors.New("source close failed")

type closeErrorSource struct{}

func (closeErrorSource) Descriptor() teleop.Descriptor {
	return teleop.Descriptor{ID: "source-close-error"}
}

func (closeErrorSource) Read(ctx context.Context) (teleop.Observation, error) {
	<-ctx.Done()
	return teleop.Observation{}, ctx.Err()
}

func (closeErrorSource) Close() error { return errSourceClose }

type readErrorSource struct{ err error }

func (readErrorSource) Descriptor() teleop.Descriptor {
	return teleop.Descriptor{ID: "source-read-error"}
}

func (source readErrorSource) Read(context.Context) (teleop.Observation, error) {
	return teleop.Observation{}, source.err
}

func (readErrorSource) Close() error { return nil }

func TestInputSourcePanicBecomesObservableTerminalError(t *testing.T) {
	controller, err := teleop.NewController(panicSource{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("controller did not contain input source panic")
	}
	if !errors.Is(controller.Err(), teleop.ErrCallbackPanic) {
		t.Fatalf("terminal error = %v, want ErrCallbackPanic", controller.Err())
	}
}

func TestCloseReportsInputSourceCloseError(t *testing.T) {
	controller, err := teleop.NewController(
		closeErrorSource{},
		teleop.WithShutdownTimeout(100*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(); !errors.Is(err, errSourceClose) {
		t.Fatalf("Close error = %v, want source close error", err)
	}
	if !errors.Is(controller.Err(), errSourceClose) {
		t.Fatalf("terminal error = %v, want source close error", controller.Err())
	}
}

func TestReadErrorPreservesTypedCause(t *testing.T) {
	controller, err := teleop.NewController(readErrorSource{err: teleop.ErrPermission})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("controller did not terminate after source error")
	}
	if !errors.Is(controller.Err(), teleop.ErrDisconnected) ||
		!errors.Is(controller.Err(), teleop.ErrPermission) {
		t.Fatalf(
			"terminal error = %v, want ErrDisconnected and ErrPermission",
			controller.Err(),
		)
	}
}

func TestProcessorPanicBecomesObservableTerminalError(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "processor-panic"}, 4)
	controller, err := teleop.NewController(
		source,
		teleop.WithProcessor(panicOnObservation{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Push(context.Background(), teleop.State{RightTrigger: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("controller did not contain processor panic")
	}
	if !errors.Is(controller.Err(), teleop.ErrCallbackPanic) {
		t.Fatalf("terminal error = %v, want ErrCallbackPanic", controller.Err())
	}

	var foundError, foundDisconnect, foundNeutralTrigger bool
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		event, nextErr := subscription.Next(ctx)
		if nextErr != nil {
			break
		}
		switch value := event.(type) {
		case teleop.ErrorEvent:
			foundError = foundError || errors.Is(value.Err, teleop.ErrCallbackPanic)
		case teleop.ConnectionEvent:
			foundDisconnect = foundDisconnect || value.State == teleop.Disconnected
		case teleop.TriggerEvent:
			foundNeutralTrigger = foundNeutralTrigger ||
				value.Trigger == teleop.RightTrigger && value.Position == 0
		}
	}
	if !foundError || !foundDisconnect || !foundNeutralTrigger {
		t.Fatalf(
			"terminal events: error=%t disconnect=%t neutral=%t",
			foundError,
			foundDisconnect,
			foundNeutralTrigger,
		)
	}
}

func TestTimedOutProcessorIsNotReenteredDuringTermination(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "processor-timeout"}, 4)
	processor := &blockingObservationProcessor{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	defer close(processor.release)
	controller, err := teleop.NewController(
		source,
		teleop.WithProcessor(processor),
		teleop.WithCallbackTimeout(20*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	if err := source.Push(context.Background(), teleop.State{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor callback did not start")
	}
	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("controller did not terminate after processor timeout")
	}
	if !errors.Is(controller.Err(), teleop.ErrCallbackTimeout) {
		t.Fatalf("terminal error = %v, want ErrCallbackTimeout", controller.Err())
	}
	if calls := processor.calls(); calls != 1 {
		t.Fatalf("timed-out processor observation calls = %d, want 1", calls)
	}
}

type delayedSink struct {
	delay time.Duration

	mu    sync.Mutex
	count int
}

func (sink *delayedSink) Record(context.Context, teleop.Event) error {
	time.Sleep(sink.delay)
	sink.mu.Lock()
	sink.count++
	sink.mu.Unlock()
	return nil
}

func TestSlowSinkDoesNotDelayCanonicalSnapshot(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "async-sink"}, 16)
	sink := &delayedSink{delay: 20 * time.Millisecond}
	controller, err := teleop.NewController(source, teleop.WithAuditSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	waitForSnapshot(t, controller, func(_ teleop.State, meta teleop.StateMeta) bool {
		return meta.Connected
	})
	for index := range 8 {
		if err := source.Push(context.Background(), teleop.State{
			LeftTrigger: float32(index) / 7,
		}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		if controller.Snapshot().LeftTrigger == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("snapshot remained at %f while a slow sink drained", controller.Snapshot().LeftTrigger)
}

type channelSink struct {
	events chan teleop.Event
}

func (sink channelSink) Record(ctx context.Context, event teleop.Event) error {
	select {
	case sink.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestControllerStartsAttachedSinkWithoutSnapshotOrSubscription(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "eager-audit"}, 4)
	events := make(chan teleop.Event, 8)
	controller, err := teleop.NewController(
		source,
		teleop.WithAuditSink(channelSink{events: events}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	for _, want := range []teleop.EventKind{
		teleop.EventConnection,
		teleop.EventCapabilities,
	} {
		select {
		case event := <-events:
			if event.Kind() != want {
				t.Fatalf("sink event kind = %s, want %s", event.Kind(), want)
			}
		case <-time.After(time.Second):
			t.Fatalf("sink did not receive %s without an explicit start operation", want)
		}
	}
}

func TestLivenessMarksAndNeutralizesStaleInput(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "liveness"}, 4)
	controller, err := teleop.NewController(
		source,
		teleop.WithLiveness(5*time.Millisecond, 20*time.Millisecond),
		teleop.WithNeutralizeOnStale(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 64})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		liveness, ok := event.(teleop.LivenessEvent)
		if ok && liveness.State == teleop.LivenessStale {
			break
		}
	}
	state, meta := controller.SnapshotWithMeta()
	if !reflect.DeepEqual(state, teleop.State{}) || !meta.Stale || !meta.Synthetic {
		t.Fatalf("stale snapshot = %#v, %#v", state, meta)
	}
	time.Sleep(15 * time.Millisecond)
	_, meta = controller.SnapshotWithMeta()
	if !meta.Stale {
		t.Fatal("synthetic neutralization refreshed physical input freshness")
	}
}

func TestObservationAgeDoesNotNeutralizeWithoutExplicitOptIn(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "held-input"}, 4)
	controller, err := teleop.NewController(
		source,
		teleop.WithLiveness(0, 20*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	state := teleop.State{}
	state.SetButton(teleop.ButtonFaceSouth, true)
	if err := source.Push(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	waitForSnapshot(t, controller, func(got teleop.State, _ teleop.StateMeta) bool {
		return got.Button(teleop.ButtonFaceSouth)
	})
	deadline := time.Now().Add(100 * time.Millisecond)
	var (
		got  teleop.State
		meta teleop.StateMeta
	)
	for time.Now().Before(deadline) {
		got, meta = controller.SnapshotWithMeta()
		if meta.Stale {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !got.Button(teleop.ButtonFaceSouth) {
		t.Fatal("observation age neutralized a healthy held control without opt-in")
	}
	if !meta.Stale || meta.Synthetic {
		t.Fatalf("aged snapshot metadata = %#v", meta)
	}
}

type failAfterSink struct {
	mu        sync.Mutex
	remaining int
}

func (sink *failAfterSink) Record(context.Context, teleop.Event) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.remaining == 0 {
		return errors.New("audit disk full")
	}
	sink.remaining--
	return nil
}

func TestSinkFailureStillDeliversTerminalSafetyEvents(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "sink-failure"}, 4)
	sink := &failAfterSink{remaining: 2}
	controller, err := teleop.NewController(source, teleop.WithAuditSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Push(context.Background(), teleop.State{LeftTrigger: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("controller did not terminate after sink failure")
	}

	var foundError, foundDisconnect, foundNeutralTrigger bool
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		event, nextErr := subscription.Next(ctx)
		if nextErr != nil {
			break
		}
		switch value := event.(type) {
		case teleop.ErrorEvent:
			foundError = foundError || value.Message != ""
		case teleop.ConnectionEvent:
			foundDisconnect = foundDisconnect || value.State == teleop.Disconnected
		case teleop.TriggerEvent:
			foundNeutralTrigger = foundNeutralTrigger ||
				value.Trigger == teleop.LeftTrigger && value.Position == 0
		}
	}
	if !foundError || !foundDisconnect || !foundNeutralTrigger {
		t.Fatalf(
			"sink failure terminal events: error=%t disconnect=%t neutral=%t terminal=%v",
			foundError,
			foundDisconnect,
			foundNeutralTrigger,
			controller.Err(),
		)
	}
}

func TestLateSubscriberReceivesLatchedSessionState(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "late"}, 4)
	controller, err := teleop.NewController(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	if err := source.Push(context.Background(), teleop.State{RightTrigger: 0.75}); err != nil {
		t.Fatal(err)
	}
	waitForSnapshot(t, controller, func(state teleop.State, _ teleop.StateMeta) bool {
		return state.RightTrigger == 0.75
	})
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, want := range []teleop.EventKind{
		teleop.EventConnection,
		teleop.EventCapabilities,
		teleop.EventObservation,
	} {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind() != want {
			t.Fatalf("latched event kind = %s, want %s", event.Kind(), want)
		}
	}
}

func waitForSnapshot(
	t *testing.T,
	controller *teleop.Controller,
	ready func(teleop.State, teleop.StateMeta) bool,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, meta := controller.SnapshotWithMeta()
		if ready(state, meta) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	state, meta := controller.SnapshotWithMeta()
	t.Fatalf("snapshot condition not reached: state=%#v meta=%#v", state, meta)
}
