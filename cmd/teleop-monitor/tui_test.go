package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/open-ships/teleop"
)

type stubController struct {
	descriptor teleop.Descriptor
	state      teleop.State
	done       chan struct{}
}

func (c *stubController) Descriptor() teleop.Descriptor {
	return c.descriptor
}

func (c *stubController) Capabilities() teleop.Capabilities {
	return c.descriptor.Capability
}

func (c *stubController) Snapshot() teleop.State {
	return c.state
}

func (c *stubController) SnapshotWithMeta() (teleop.State, teleop.StateMeta) {
	return c.state, teleop.StateMeta{Connected: true}
}

func (*stubController) Session() teleop.SessionID {
	return teleop.SessionID{}
}

func (c *stubController) Done() <-chan struct{} {
	if c.done == nil {
		c.done = make(chan struct{})
	}
	return c.done
}

func (*stubController) Err() error {
	return nil
}

func (*stubController) RecordCommand(context.Context, teleop.Command) error {
	return nil
}

func (*stubController) Subscribe(
	teleop.SubscriptionOptions,
) (teleop.Subscription, error) {
	return nil, errors.New("not implemented")
}

func (*stubController) Close() error {
	return nil
}

type stubSubscription struct {
	events []teleop.Event
	err    error
}

func (s *stubSubscription) Next(context.Context) (teleop.Event, error) {
	if len(s.events) == 0 {
		return nil, s.err
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (*stubSubscription) Stats() teleop.SubscriptionStats {
	return teleop.SubscriptionStats{}
}

func (*stubSubscription) Close() error {
	return nil
}

func TestMonitorModelConsumesAndRendersControllerEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := teleop.State{}
	state.SetButton(teleop.ButtonFaceSouth, true)
	controller := &stubController{
		descriptor: teleop.Descriptor{
			Type:      teleop.ControllerXbox,
			Name:      "Test Xbox Controller",
			Backend:   "test",
			Transport: teleop.TransportBluetooth,
			Capability: teleop.Capabilities{
				AuditGrade: teleop.AuditExactBackendStream,
			},
		},
		state: state,
	}
	subscription := &stubSubscription{
		events: []teleop.Event{
			teleop.ButtonEvent{
				Button: teleop.ButtonFaceSouth,
				Phase:  teleop.PhasePressed,
			},
		},
	}
	model := newMonitorModel(ctx, cancel, controller, subscription, "input.jsonl")

	message := model.Init()()
	updated, next := model.Update(message)
	if updated != model {
		t.Fatal("Update returned a different model")
	}
	if next == nil {
		t.Fatal("Update did not schedule the next lossless event read")
	}
	if model.eventCount != 1 {
		t.Fatalf("event count = %d, want 1", model.eventCount)
	}
	if !model.state.Button(teleop.ButtonFaceSouth) {
		t.Fatal("button state was not updated")
	}
	if len(model.recent) != 1 {
		t.Fatalf("recent event count = %d, want 1", len(model.recent))
	}

	view := model.View()
	if !view.AltScreen {
		t.Fatal("monitor does not request the alternate screen")
	}
	for _, expected := range []string{
		"TELEOP MONITOR",
		"Test Xbox Controller",
		"INPUT STATE",
		"EVENT STREAM",
		"button.face.south",
		"REC",
	} {
		if !strings.Contains(view.Content, expected) {
			t.Errorf("view does not contain %q", expected)
		}
	}
}

func TestMonitorModelTracksObservationsAndGaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	controller := &stubController{
		descriptor: teleop.Descriptor{Name: "Test Controller"},
	}
	model := newMonitorModel(
		ctx,
		cancel,
		controller,
		&stubSubscription{},
		"",
	)

	observed := teleop.State{LeftTrigger: 0.75}
	model.addEvent(teleop.ObservationEvent{Current: observed}, teleop.State{})
	model.addEvent(
		teleop.GapEvent{Reason: "backend queue overflow"},
		observed,
	)

	if model.eventCount != 2 {
		t.Fatalf("event count = %d, want 2", model.eventCount)
	}
	if model.observations != 1 {
		t.Fatalf("observation count = %d, want 1", model.observations)
	}
	if model.state.LeftTrigger != observed.LeftTrigger {
		t.Fatalf("left trigger = %v, want %v", model.state.LeftTrigger, observed.LeftTrigger)
	}
	if model.lastGap != "backend queue overflow" {
		t.Fatalf("last gap = %q", model.lastGap)
	}
	if !strings.Contains(model.render(), "INPUT GAP") {
		t.Fatal("gap warning is not visible")
	}
}

func TestMonitorModelClearsGapAlertOnNextObservation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newMonitorModel(
		ctx,
		cancel,
		&stubController{descriptor: teleop.Descriptor{Name: "Test Controller"}},
		&stubSubscription{},
		"",
	)
	model.addEvent(teleop.GapEvent{Reason: "kernel overflow\x1b[2J"}, teleop.State{})
	if model.lastGap != "kernel overflow[2J" {
		t.Fatalf("sanitized gap = %q", model.lastGap)
	}
	model.addEvent(teleop.ObservationEvent{Current: teleop.State{}}, teleop.State{})
	if model.lastGap != "" {
		t.Fatalf("gap alert remained after a complete observation: %q", model.lastGap)
	}
}

func TestMonitorModelRespondsToResizeAndQuit(t *testing.T) {
	eventContext, cancelEvents := context.WithCancel(context.Background())
	controller := &stubController{
		descriptor: teleop.Descriptor{Name: "Test Controller"},
	}
	model := newMonitorModel(
		eventContext,
		cancelEvents,
		controller,
		&stubSubscription{},
		"",
	)

	_, _ = model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	if model.width != 60 || model.height != 20 {
		t.Fatalf("size = %dx%d, want 60x20", model.width, model.height)
	}
	if !strings.Contains(model.render(), "EVENT STREAM") {
		t.Fatal("compact layout does not include the event stream")
	}

	_, command := model.Update(tea.KeyPressMsg(tea.Key{
		Code: 'q',
		Text: "q",
	}))
	if command == nil {
		t.Fatal("quit key returned no command")
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatal("quit key did not return tea.Quit")
	}
	if !errors.Is(eventContext.Err(), context.Canceled) {
		t.Fatal("quit key did not cancel the pending event read")
	}
}

func TestMonitorModelReturnsStreamErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamErr := errors.New("controller read failed")
	model := newMonitorModel(
		ctx,
		cancel,
		&stubController{
			descriptor: teleop.Descriptor{Name: "Test Controller"},
		},
		&stubSubscription{err: streamErr},
		"",
	)

	message := model.Init()()
	_, command := model.Update(message)
	if command == nil {
		t.Fatal("stream error did not stop the program")
	}
	if !errors.Is(model.streamErr, streamErr) {
		t.Fatalf("stream error = %v, want %v", model.streamErr, streamErr)
	}
}

func TestDPadButtonSummaryIncludesDirectionPhase(t *testing.T) {
	t.Parallel()

	event := teleop.ButtonEvent{
		Button:  teleop.DPadRight,
		Phase:   teleop.PhaseReleased,
		Pressed: false,
	}
	if got := eventSummary(event); got != "button.dpad.right released" {
		t.Fatalf("summary = %q", got)
	}
}
