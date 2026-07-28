package teleop_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/action"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/gesture"
	"github.com/open-ships/teleop/testkit"
)

func TestControllerPublishesCanonicalEventsAndAudit(t *testing.T) {
	t.Parallel()

	descriptor := teleop.Descriptor{
		ID:        "test:xbox",
		Type:      teleop.ControllerXbox,
		Name:      "Test Xbox controller",
		Transport: teleop.TransportVirtual,
		Backend:   "test",
		Capability: teleop.Capabilities{
			AuditGrade: teleop.AuditExactBackendStream,
			Controls: []teleop.ControlDescriptor{
				{ID: teleop.ButtonFaceSouth, Kind: teleop.ControlButton},
				{ID: teleop.StickLeft, Kind: teleop.ControlStick},
			},
		},
	}
	source := testkit.NewFakeSource(descriptor, 8)
	var log bytes.Buffer
	recorder := audit.NewRecorder(&log, audit.WithFlushEveryEvent(true))
	controller, err := teleop.NewController(source, teleop.WithAuditSink(recorder))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = controller.Close()
		_ = recorder.Close()
	})

	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{
		Delivery: teleop.DeliveryLossless,
		Buffer:   32,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subscription.Close() })

	state := teleop.State{
		LeftStick: teleop.Stick{X: 0.25, Y: -0.5},
	}
	state.SetButton(teleop.ButtonFaceSouth, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := source.Push(ctx, state); err != nil {
		t.Fatal(err)
	}

	var (
		observation teleop.ObservationEvent
		button      teleop.ButtonEvent
		stick       teleop.StickEvent
	)
	for button.Button == "" || stick.Stick == "" {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.(type) {
		case teleop.ObservationEvent:
			observation = value
		case teleop.ButtonEvent:
			button = value
		case teleop.StickEvent:
			stick = value
		}
	}

	if button.Button != teleop.ButtonFaceSouth || button.Phase != teleop.PhasePressed {
		t.Fatalf("unexpected button event: %#v", button)
	}
	if stick.Stick != teleop.LeftStick || stick.Position != state.LeftStick {
		t.Fatalf("unexpected stick event: %#v", stick)
	}
	if len(button.Meta.Causes) != 1 || button.Meta.Causes[0] != observation.Meta.ID {
		t.Fatalf("button cause = %#v, observation = %#v", button.Meta.Causes, observation.Meta.ID)
	}
	if got := controller.Snapshot(); got.LeftStick != state.LeftStick || !got.Button(teleop.ButtonFaceSouth) {
		t.Fatalf("snapshot = %#v", got)
	}

	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := audit.ReadAll(bytes.NewReader(log.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < 5 {
		t.Fatalf("audit records = %d, want at least 5", len(records))
	}
}

func TestLosslessSubscriptionFailsExplicitlyOnOverflow(t *testing.T) {
	t.Parallel()

	source := testkit.NewFakeSource(teleop.Descriptor{ID: "overflow"}, 1)
	controller, err := teleop.NewController(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })

	ready, err := controller.Subscribe(teleop.SubscriptionOptions{
		Delivery: teleop.DeliveryLossless,
		Buffer:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		event, nextErr := ready.Next(ctx)
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if event.Kind() == teleop.EventCapabilities {
			break
		}
	}
	if err := ready.Close(); err != nil {
		t.Fatal(err)
	}

	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{
		Delivery: teleop.DeliveryLossless,
		Buffer:   1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Connection and capabilities are published immediately, overflowing a
	// deliberately undersized subscriber before it consumes either event.
	if _, err = subscription.Next(ctx); err != nil {
		t.Fatalf("buffered event error = %v", err)
	}
	_, err = subscription.Next(ctx)
	if !errors.Is(err, teleop.ErrSubscriptionOverflow) {
		t.Fatalf("Next error = %v, want %v", err, teleop.ErrSubscriptionOverflow)
	}
}

func TestProcessorsPublishAndAuditDerivedEvents(t *testing.T) {
	t.Parallel()

	source := testkit.NewFakeSource(teleop.Descriptor{
		ID:      "processor",
		Backend: "test",
	}, 4)
	recognizer := gesture.New(gesture.DefaultConfig())
	mapper := action.New(
		action.OnGesture("confirm", gesture.Tap, teleop.ButtonFaceSouth),
	)
	var log bytes.Buffer
	recorder := audit.NewRecorder(&log)
	controller, err := teleop.NewController(
		source,
		teleop.WithAuditSink(recorder),
		teleop.WithProcessor(recognizer),
		teleop.WithProcessor(mapper),
	)
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 64})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pressed := teleop.State{}
	pressed.SetButton(teleop.ButtonFaceSouth, true)
	if err := source.PushObservation(ctx, teleop.Observation{
		State:      pressed,
		ObservedAt: time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.PushObservation(ctx, teleop.Observation{
		State:      teleop.State{},
		ObservedAt: time.Unix(10, int64(100*time.Millisecond)),
	}); err != nil {
		t.Fatal(err)
	}

	var mapped action.Event
	for mapped.Action == "" {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := event.(action.Event); ok {
			mapped = value
		}
	}
	if mapped.Action != "confirm" || len(mapped.Meta.Causes) != 1 {
		t.Fatalf("mapped action = %#v", mapped)
	}

	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := audit.ReadAll(bytes.NewReader(log.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	foundGesture, foundAction := false, false
	for _, record := range records {
		foundGesture = foundGesture || record.Kind == gesture.EventKind
		foundAction = foundAction || record.Kind == action.EventKind
	}
	if !foundGesture || !foundAction {
		t.Fatalf("audit kinds missing: gesture=%v action=%v", foundGesture, foundAction)
	}
}

func TestAdvancingProcessorPublishesHoldWhileControllerIsIdle(t *testing.T) {
	t.Parallel()

	source := testkit.NewFakeSource(teleop.Descriptor{ID: "hold"}, 2)
	recognizer := gesture.New(gesture.Config{
		TapMaximum:       20 * time.Millisecond,
		DoubleTapWindow:  50 * time.Millisecond,
		HoldMinimum:      40 * time.Millisecond,
		StickThreshold:   0.5,
		TriggerThreshold: 0.5,
	})
	mapper := action.New(
		action.OnGesture("arm", gesture.Hold, teleop.ButtonBumperLeft),
	)
	controller, err := teleop.NewController(
		source,
		teleop.WithProcessor(recognizer),
		teleop.WithProcessor(mapper),
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
	state.SetButton(teleop.ButtonBumperLeft, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := source.Push(ctx, state); err != nil {
		t.Fatal(err)
	}
	for {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := event.(action.Event); ok &&
			value.Action == "arm" &&
			value.Phase == teleop.PhaseStarted {
			return
		}
	}
}

func TestNormalization(t *testing.T) {
	t.Parallel()

	if got := teleop.NormalizeAxis(0, -32768, 32767); got != 0 {
		t.Fatalf("centered axis = %f, want exact neutral", got)
	}
	if got := teleop.NormalizeAxis(-32768, -32768, 32767); got != -1 {
		t.Fatalf("minimum axis = %f", got)
	}
	if got := teleop.NormalizeTrigger(255, 0, 255); got != 1 {
		t.Fatalf("maximum trigger = %f", got)
	}
	if got := teleop.NormalizeAxis(32767, -32768, 32767); got != 1 {
		t.Fatalf("maximum signed axis = %f", got)
	}
	if got := teleop.NormalizeTrigger(2_147_483_647, -2_147_483_648, 2_147_483_647); got != 1 {
		t.Fatalf("wide trigger maximum = %f", got)
	}
	if got := teleop.ApplyRadialDeadZone(teleop.Stick{X: 0.05}, 0.1); got != (teleop.Stick{}) {
		t.Fatalf("dead-zone result = %#v", got)
	}
}
