package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/open-ships/teleop"
)

type stubController struct {
	descriptor teleop.Descriptor
	state      teleop.State
	done       chan struct{}
	rumble     []teleop.Rumble
	rumbleErr  error
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

func (c *stubController) SetRumble(
	ctx context.Context,
	rumble teleop.Rumble,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.rumble = append(c.rumble, rumble)
	return c.rumbleErr
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
		"HAPTIC FEEDBACK",
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

func TestMonitorModelPlacesHapticsBeforePrimaryPanels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newMonitorModel(
		ctx,
		cancel,
		&stubController{
			descriptor: teleop.Descriptor{
				Name: "Rumble Controller",
				Capability: teleop.Capabilities{
					Rumble: true,
				},
			},
		},
		&stubSubscription{},
		"",
	)

	for _, width := range []int{60, 140} {
		model.width = width
		rendered := model.render()
		haptics := strings.Index(rendered, "HAPTIC FEEDBACK")
		input := strings.Index(rendered, "INPUT STATE")
		events := strings.Index(rendered, "EVENT STREAM")
		if haptics < 0 || input < 0 || events < 0 {
			t.Fatalf("width %d omitted a primary section", width)
		}
		if !(haptics < input && haptics < events) {
			t.Fatalf(
				"width %d section order does not place haptics before both primary panels",
				width,
			)
		}
	}
	if strings.Contains(model.renderFooter(140), "RUMBLE") {
		t.Fatal("global footer still contains the rumble control")
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

func TestMonitorModelTogglesRumble(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := &stubController{
		descriptor: teleop.Descriptor{
			Name: "Rumble Controller",
			Capability: teleop.Capabilities{
				Rumble: true,
			},
		},
	}
	model := newMonitorModel(
		ctx,
		cancel,
		controller,
		&stubSubscription{},
		"",
	)

	for _, expected := range []string{
		"HAPTIC FEEDBACK",
		"○ OFF",
		"turn on",
		"INTENSITY",
		"100%",
		"adjust",
	} {
		if !strings.Contains(model.render(), expected) {
			t.Fatalf("available haptic section does not contain %q", expected)
		}
	}
	if strings.Contains(model.renderFooter(96), "rumble") {
		t.Fatal("rumble control still appears in the global footer")
	}
	_, command := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
	if command != nil || model.rumbleLevel != 9 {
		t.Fatal("offline left key did not lower intensity without a backend call")
	}
	if len(controller.rumble) != 0 || !strings.Contains(model.render(), "90%") {
		t.Fatal("offline intensity adjustment was not retained in the slider")
	}
	_, command = model.Update(tea.KeyPressMsg(tea.Key{
		Code: 'r',
		Text: "r",
	}))
	if command == nil || !model.rumblePending || !model.rumbleTarget {
		t.Fatal("rumble-on key did not start an enable request")
	}
	_, repeated := model.Update(tea.KeyPressMsg(tea.Key{
		Code:     'r',
		Text:     "r",
		IsRepeat: true,
	}))
	if repeated != nil {
		t.Fatal("repeated rumble key started a second request")
	}
	for line := range strings.SplitSeq(model.renderHaptics(30), "\n") {
		if lipgloss.Width(line) > 30 {
			t.Fatalf(
				"compact haptic line is %d cells wide, want at most 30",
				lipgloss.Width(line),
			)
		}
	}
	message, ok := command().(rumbleSetMsg)
	if !ok {
		t.Fatal("rumble command returned an unexpected message")
	}
	_, _ = model.Update(message)
	if !model.rumbleOn || model.rumblePending {
		t.Fatal("successful rumble-on request did not update the model")
	}
	if len(controller.rumble) != 1 ||
		controller.rumble[0] != (teleop.Rumble{
			LowFrequency:  0.9,
			HighFrequency: 0.9,
		}) {
		t.Fatalf("rumble-on calls = %#v, want both components at 0.9", controller.rumble)
	}
	for _, expected := range []string{"● ON", "turn off"} {
		if !strings.Contains(model.render(), expected) {
			t.Fatalf("enabled view does not contain %q", expected)
		}
	}

	_, command = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
	if command == nil || model.rumbleLevel != 8 || !model.rumblePending {
		t.Fatal("live left key did not start an intensity update")
	}
	if !strings.Contains(model.renderHaptics(96), "APPLYING") {
		t.Fatal("live intensity update does not show its pending state")
	}
	_, queued := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
	if queued != nil || model.rumbleLevel != 7 {
		t.Fatal("pending intensity adjustment was not coalesced")
	}
	message, ok = command().(rumbleSetMsg)
	if !ok {
		t.Fatal("intensity command returned an unexpected message")
	}
	_, catchUp := model.Update(message)
	if catchUp == nil {
		t.Fatal("coalesced intensity did not schedule a catch-up request")
	}
	message, ok = catchUp().(rumbleSetMsg)
	if !ok {
		t.Fatal("catch-up intensity command returned an unexpected message")
	}
	_, _ = model.Update(message)
	if model.rumblePending || model.rumbleLevel != 7 {
		t.Fatal("catch-up intensity request did not settle at 70%")
	}
	if len(controller.rumble) != 3 ||
		controller.rumble[1] != (teleop.Rumble{
			LowFrequency:  0.8,
			HighFrequency: 0.8,
		}) ||
		controller.rumble[2] != (teleop.Rumble{
			LowFrequency:  0.7,
			HighFrequency: 0.7,
		}) {
		t.Fatalf("live intensity calls = %#v, want 0.8 followed by 0.7", controller.rumble)
	}
	if !strings.Contains(model.renderHaptics(96), "70%") {
		t.Fatal("settled slider does not show 70%")
	}

	_, command = model.Update(tea.KeyPressMsg(tea.Key{
		Code: 'R',
		Text: "R",
	}))
	if command == nil || !model.rumblePending || model.rumbleTarget {
		t.Fatal("rumble-off key did not start a disable request")
	}
	message, ok = command().(rumbleSetMsg)
	if !ok {
		t.Fatal("rumble command returned an unexpected message")
	}
	_, _ = model.Update(message)
	if model.rumbleOn || model.rumblePending {
		t.Fatal("successful rumble-off request did not update the model")
	}
	if len(controller.rumble) != 4 ||
		controller.rumble[3] != (teleop.Rumble{}) {
		t.Fatalf("rumble-off calls = %#v, want a zero rumble", controller.rumble)
	}
}

func TestMonitorModelClampsRumbleIntensity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := &stubController{
		descriptor: teleop.Descriptor{
			Capability: teleop.Capabilities{Rumble: true},
		},
	}
	model := newMonitorModel(
		ctx,
		cancel,
		controller,
		&stubSubscription{},
		"",
	)

	_, command := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
	if command != nil || model.rumbleLevel != rumbleIntensitySteps {
		t.Fatal("right key moved intensity above 100%")
	}
	for range rumbleIntensitySteps + 2 {
		_, command = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
		if command != nil {
			t.Fatal("offline intensity adjustment called the backend")
		}
	}
	if model.rumbleLevel != 0 || !strings.Contains(model.renderHaptics(96), "  0%") {
		t.Fatal("left key did not clamp intensity at 0%")
	}
	for range rumbleIntensitySteps + 2 {
		_, command = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
		if command != nil {
			t.Fatal("offline intensity adjustment called the backend")
		}
	}
	if model.rumbleLevel != rumbleIntensitySteps ||
		!strings.Contains(model.renderHaptics(96), "100%") {
		t.Fatal("right key did not clamp intensity at 100%")
	}
	if len(controller.rumble) != 0 {
		t.Fatal("offline slider changes reached the backend")
	}
}

func TestMonitorModelAlternatesRumbleAtConfiguredInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := &stubController{
		descriptor: teleop.Descriptor{
			Capability: teleop.Capabilities{Rumble: true},
		},
	}
	model := newMonitorModel(
		ctx,
		cancel,
		controller,
		&stubSubscription{},
		"",
	)

	_, command := model.Update(tea.KeyPressMsg(tea.Key{
		Code: 'm',
		Text: "m",
	}))
	if command != nil || model.rumbleMode != rumbleModeAlternate {
		t.Fatal("mode key did not select alternating rumble")
	}
	for _, expected := range []string{
		"ALTERNATE L/R",
		"INTERVAL",
		"5s",
		"select slider",
	} {
		if !strings.Contains(model.renderHaptics(96), expected) {
			t.Fatalf("alternating haptic controls do not contain %q", expected)
		}
	}
	for line := range strings.SplitSeq(model.renderHaptics(30), "\n") {
		if lipgloss.Width(line) > 30 {
			t.Fatalf(
				"compact alternating haptic line is %d cells wide, want at most 30",
				lipgloss.Width(line),
			)
		}
	}

	_, command = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	if command != nil || model.hapticSlider != hapticSliderInterval {
		t.Fatal("tab did not select the interval slider")
	}
	_, command = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
	if command != nil ||
		model.rumbleInterval() != 6*time.Second ||
		!strings.Contains(model.renderHaptics(96), "6s") {
		t.Fatal("interval slider did not advance to 6s while rumble was off")
	}

	_, command = model.Update(tea.KeyPressMsg(tea.Key{
		Code: 'r',
		Text: "r",
	}))
	if command == nil {
		t.Fatal("alternating rumble did not start")
	}
	message, ok := command().(rumbleSetMsg)
	if !ok {
		t.Fatal("alternating start returned an unexpected message")
	}
	if message.mode != rumbleModeAlternate ||
		message.side != rumbleSideLeft {
		t.Fatalf(
			"alternating start = mode %d side %d, want alternate left",
			message.mode,
			message.side,
		)
	}
	_, timer := model.Update(message)
	if timer == nil || !model.rumbleOn || model.rumbleSide != rumbleSideLeft {
		t.Fatal("successful alternating start did not schedule a side switch")
	}
	if !strings.Contains(model.renderHaptics(96), "● ON · LEFT") {
		t.Fatal("alternating status does not show the active left side")
	}
	if len(controller.rumble) != 1 ||
		controller.rumble[0] != (teleop.Rumble{
			LowFrequency: 1,
		}) {
		t.Fatalf("alternating left call = %#v, want low-frequency only", controller.rumble)
	}

	alternatingRevision := model.rumbleRevision
	_, command = model.Update(rumbleTickMsg{
		revision: alternatingRevision,
	})
	if command == nil {
		t.Fatal("alternating tick did not request the right side")
	}
	message, ok = command().(rumbleSetMsg)
	if !ok || message.side != rumbleSideRight {
		t.Fatal("alternating tick returned an unexpected side request")
	}
	_, timer = model.Update(message)
	if timer == nil || model.rumbleSide != rumbleSideRight {
		t.Fatal("successful right-side request did not continue oscillation")
	}
	if !strings.Contains(model.renderHaptics(96), "● ON · RIGHT") {
		t.Fatal("alternating status does not show the active right side")
	}
	if len(controller.rumble) != 2 ||
		controller.rumble[1] != (teleop.Rumble{
			HighFrequency: 1,
		}) {
		t.Fatalf("alternating right call = %#v, want high-frequency only", controller.rumble)
	}

	_, command = model.Update(tea.KeyPressMsg(tea.Key{
		Code: 'm',
		Text: "m",
	}))
	if command == nil || model.rumbleMode != rumbleModeBoth {
		t.Fatal("mode key did not switch active rumble back to both")
	}
	message, ok = command().(rumbleSetMsg)
	if !ok || message.mode != rumbleModeBoth {
		t.Fatal("both-mode change returned an unexpected request")
	}
	_, timer = model.Update(message)
	if timer != nil {
		t.Fatal("both mode continued scheduling alternating ticks")
	}
	if len(controller.rumble) != 3 ||
		controller.rumble[2] != (teleop.Rumble{
			LowFrequency:  1,
			HighFrequency: 1,
		}) {
		t.Fatalf("both-mode call = %#v, want both components", controller.rumble)
	}
	_, stale := model.Update(rumbleTickMsg{
		revision: alternatingRevision,
	})
	if stale != nil || len(controller.rumble) != 3 {
		t.Fatal("stale alternating tick survived the mode change")
	}
}

func TestMonitorModelClampsRumbleInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := &stubController{
		descriptor: teleop.Descriptor{
			Capability: teleop.Capabilities{Rumble: true},
		},
	}
	model := newMonitorModel(
		ctx,
		cancel,
		controller,
		&stubSubscription{},
		"",
	)
	model.rumbleMode = rumbleModeAlternate
	model.hapticSlider = hapticSliderInterval

	for range rumbleIntervalSteps + 2 {
		_, command := model.Update(tea.KeyPressMsg(tea.Key{
			Code: tea.KeyLeft,
		}))
		if command != nil {
			t.Fatal("offline interval adjustment called the backend")
		}
	}
	if model.rumbleInterval() != time.Second ||
		!strings.Contains(model.renderHaptics(96), "1s") {
		t.Fatal("interval slider did not clamp at 1s")
	}
	for range rumbleIntervalSteps + 2 {
		_, command := model.Update(tea.KeyPressMsg(tea.Key{
			Code: tea.KeyRight,
		}))
		if command != nil {
			t.Fatal("offline interval adjustment called the backend")
		}
	}
	if model.rumbleInterval() != 10*time.Second ||
		!strings.Contains(model.renderHaptics(96), "10s") {
		t.Fatal("interval slider did not clamp at 10s")
	}
	if len(controller.rumble) != 0 {
		t.Fatal("offline interval slider changes reached the backend")
	}
}

func TestMonitorModelReportsUnavailableAndFailedRumble(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	unsupported := &stubController{
		descriptor: teleop.Descriptor{Name: "Plain Controller"},
	}
	model := newMonitorModel(
		ctx,
		cancel,
		unsupported,
		&stubSubscription{},
		"",
	)
	for _, expected := range []string{"HAPTIC FEEDBACK", "RUMBLE", "UNAVAILABLE"} {
		if !strings.Contains(model.render(), expected) {
			t.Fatalf("unavailable haptic section does not contain %q", expected)
		}
	}
	if strings.Contains(model.renderHaptics(96), "turn on") {
		t.Fatal("unavailable rumble exposes an enable action")
	}
	_, command := model.Update(tea.KeyPressMsg(tea.Key{
		Code: 'r',
		Text: "r",
	}))
	if command != nil || len(unsupported.rumble) != 0 {
		t.Fatal("unavailable rumble attempted a backend call")
	}
	_, command = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
	if command != nil || model.rumbleLevel != rumbleIntensitySteps {
		t.Fatal("unavailable rumble accepted an intensity adjustment")
	}

	failed := &stubController{
		descriptor: teleop.Descriptor{
			Name: "Broken Rumble Controller",
			Capability: teleop.Capabilities{
				Rumble: true,
			},
		},
		rumbleErr: errors.New("motor failed\x1b[2J"),
	}
	model = newMonitorModel(
		ctx,
		cancel,
		failed,
		&stubSubscription{},
		"",
	)
	_, command = model.Update(tea.KeyPressMsg(tea.Key{
		Code: 'r',
		Text: "r",
	}))
	if command == nil {
		t.Fatal("available rumble did not start a request")
	}
	message, ok := command().(rumbleSetMsg)
	if !ok {
		t.Fatal("rumble command returned an unexpected message")
	}
	_, _ = model.Update(message)
	if model.rumbleOn || model.rumblePending {
		t.Fatal("failed rumble request changed the enabled state")
	}
	for _, expected := range []string{
		"HAPTIC FEEDBACK",
		"○ OFF",
		"ERROR",
		"motor failed[2J",
		"retry",
		"INTENSITY",
		"100%",
	} {
		if !strings.Contains(model.render(), expected) {
			t.Fatalf("failed rumble view does not contain %q", expected)
		}
	}
	for line := range strings.SplitSeq(model.renderHaptics(30), "\n") {
		if lipgloss.Width(line) > 30 {
			t.Fatalf(
				"compact haptic error line is %d cells wide, want at most 30",
				lipgloss.Width(line),
			)
		}
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
