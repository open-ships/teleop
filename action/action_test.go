package action_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/action"
	"github.com/open-ships/teleop/gesture"
)

func TestMapperPreservesCausality(t *testing.T) {
	t.Parallel()

	mapper := action.New(
		action.OnButton("confirm", teleop.ButtonFaceSouth, teleop.PhasePressed),
		action.OnGesture("arm", gesture.Hold, teleop.ButtonBumperLeft),
	)
	source := teleop.ButtonEvent{
		Meta: teleop.Header{
			ID:         teleop.EventID{Stream: "input", Sequence: 42},
			ObservedAt: time.Now(),
		},
		Button:  teleop.ButtonFaceSouth,
		Phase:   teleop.PhasePressed,
		Pressed: true,
	}
	result := mapper.Map(source)
	if len(result) != 1 || result[0].Action != "confirm" {
		t.Fatalf("actions = %#v", result)
	}
	if len(result[0].Meta.Causes) != 1 || result[0].Meta.Causes[0] != source.Meta.ID {
		t.Fatalf("causes = %#v", result[0].Meta.Causes)
	}
}

func TestMapperMapsDPadDirectionPhase(t *testing.T) {
	t.Parallel()

	mapper := action.New(
		action.OnDPad("move-left", teleop.DPadLeft, teleop.PhasePressed),
	)
	source := teleop.ButtonEvent{
		Meta:    teleop.Header{ID: teleop.EventID{Stream: "input", Sequence: 7}},
		Button:  teleop.DPadLeft,
		Phase:   teleop.PhasePressed,
		Pressed: true,
	}
	result := mapper.Map(source)
	if len(result) != 1 || result[0].Action != "move-left" {
		t.Fatalf("actions = %#v", result)
	}
	if result[0].Control != teleop.DPadLeft ||
		result[0].Phase != teleop.PhasePressed ||
		!result[0].Value.Pressed {
		t.Fatalf("D-pad action = %#v", result[0])
	}
}

func TestOnGestureChoosesOneMeaningfulPhase(t *testing.T) {
	t.Parallel()

	holdMapper := action.New(
		action.OnGesture("arm", gesture.Hold, teleop.ButtonFaceSouth),
	)
	started := gesture.Event{
		Meta:     actionHeader(1, 1),
		Type:     gesture.Hold,
		Phase:    teleop.PhaseStarted,
		Controls: []teleop.ControlID{teleop.ButtonFaceSouth},
	}
	ended := started
	ended.Meta = actionHeader(1, 2)
	ended.Phase = teleop.PhaseEnded
	if got := holdMapper.Map(started); len(got) != 1 || got[0].Phase != teleop.PhaseStarted {
		t.Fatalf("hold start = %#v", got)
	}
	if got := holdMapper.Map(ended); len(got) != 0 {
		t.Fatalf("hold end unexpectedly mapped = %#v", got)
	}

	tapMapper := action.New(
		action.OnGesture("select", gesture.Tap, teleop.ButtonFaceSouth),
	)
	tapped := ended
	tapped.Type = gesture.Tap
	if got := tapMapper.Map(tapped); len(got) != 1 || got[0].Phase != teleop.PhaseEnded {
		t.Fatalf("tap = %#v", got)
	}
}

func TestOnChordMatchesNameAndExactControlSet(t *testing.T) {
	t.Parallel()

	mapper := action.New(action.OnChord(
		"arm",
		"arm-chord",
		teleop.ButtonBumperLeft,
		teleop.ButtonFaceSouth,
	))
	base := gesture.Event{
		Meta:  actionHeader(1, 1),
		Type:  gesture.Chord,
		Phase: teleop.PhaseStarted,
		Controls: []teleop.ControlID{
			teleop.ButtonFaceSouth,
			teleop.ButtonBumperLeft,
		},
		Region: "arm-chord",
	}
	if got := mapper.Map(base); len(got) != 1 || got[0].Action != "arm" {
		t.Fatalf("exact chord = %#v", got)
	}
	wrongName := base
	wrongName.Region = "eject-chord"
	if got := mapper.Map(wrongName); len(got) != 0 {
		t.Fatalf("wrong chord name mapped = %#v", got)
	}
	wrongControls := base
	wrongControls.Controls = []teleop.ControlID{
		teleop.ButtonBumperLeft,
		teleop.ButtonFaceEast,
	}
	if got := mapper.Map(wrongControls); len(got) != 0 {
		t.Fatalf("wrong chord controls mapped = %#v", got)
	}
}

func TestMapperCopiesBindingsAndSequencesEachSession(t *testing.T) {
	t.Parallel()

	controls := []teleop.ControlID{
		teleop.ButtonFaceSouth,
		teleop.ButtonBumperLeft,
	}
	mapper := action.New(action.Binding{
		Action:      "arm",
		EventKind:   gesture.EventKind,
		Controls:    controls,
		Phase:       teleop.PhaseStarted,
		GestureType: gesture.Chord,
		Region:      "arm",
	})
	controls[0] = teleop.ButtonFaceEast
	chord := gesture.Event{
		Meta:     actionHeader(1, 1),
		Type:     gesture.Chord,
		Phase:    teleop.PhaseStarted,
		Controls: []teleop.ControlID{teleop.ButtonBumperLeft, teleop.ButtonFaceSouth},
		Region:   "arm",
	}
	first := mapper.Map(chord)
	if len(first) != 1 || first[0].Meta.ID.Sequence != 1 {
		t.Fatalf("first session action = %#v", first)
	}
	chord.Meta = actionHeader(2, 1)
	second := mapper.Map(chord)
	if len(second) != 1 || second[0].Meta.ID.Sequence != 1 {
		t.Fatalf("second session action = %#v", second)
	}
	chord.Meta = actionHeader(1, 2)
	third := mapper.Map(chord)
	if len(third) != 1 || third[0].Meta.ID.Sequence != 2 {
		t.Fatalf("continued first session action = %#v", third)
	}
}

func TestContextProcessingAllocatesUniqueSharedStreamIDs(t *testing.T) {
	t.Parallel()

	processing := &testProcessingContext{sequences: make(map[string]uint64)}
	left := action.New(
		action.OnButton("left", teleop.ButtonFaceSouth, teleop.PhasePressed),
	)
	right := action.New(
		action.OnButton("right", teleop.ButtonFaceSouth, teleop.PhasePressed),
	)
	source := teleop.ButtonEvent{
		Meta:    actionHeader(1, 1),
		Button:  teleop.ButtonFaceSouth,
		Phase:   teleop.PhasePressed,
		Pressed: true,
	}
	leftEvents, err := left.ProcessContext(context.Background(), processing, source)
	if err != nil {
		t.Fatal(err)
	}
	rightEvents, err := right.ProcessContext(context.Background(), processing, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftEvents) != 1 || len(rightEvents) != 1 {
		t.Fatalf("context actions = %#v, %#v", leftEvents, rightEvents)
	}
	if leftEvents[0].Header().ID.Sequence != 1 ||
		rightEvents[0].Header().ID.Sequence != 2 ||
		leftEvents[0].Header().ID.Stream != "action" ||
		rightEvents[0].Header().ID.Stream != "action" {
		t.Fatalf("context IDs = %#v, %#v", leftEvents[0].Header().ID, rightEvents[0].Header().ID)
	}
}

func TestOnConnectionMapsDisconnectAsEnded(t *testing.T) {
	t.Parallel()

	mapper := action.New(action.OnConnection("safe-state", teleop.Disconnected))
	result := mapper.Map(teleop.ConnectionEvent{
		Meta:  actionHeader(1, 1),
		State: teleop.Disconnected,
	})
	if len(result) != 1 || result[0].Action != "safe-state" ||
		result[0].Phase != teleop.PhaseEnded {
		t.Fatalf("disconnect action = %#v", result)
	}
}

func actionHeader(sessionByte byte, sequence uint64) teleop.Header {
	var session teleop.SessionID
	session[0] = sessionByte
	return teleop.Header{
		ID: teleop.EventID{
			Session:  session,
			Stream:   "input",
			Sequence: sequence,
		},
		DeviceID:   teleop.DeviceID("controller"),
		ObservedAt: time.Unix(int64(sequence), 0),
	}
}

type testProcessingContext struct {
	sequences map[string]uint64
}

func (processing *testProcessingContext) NewHeader(
	stream string,
	observedAt time.Time,
	deviceTimestamp int64,
	causes ...teleop.EventID,
) teleop.Header {
	processing.sequences[stream]++
	var session teleop.SessionID
	if len(causes) > 0 {
		session = causes[0].Session
	}
	return teleop.Header{
		ID: teleop.EventID{
			Session:  session,
			Stream:   stream,
			Sequence: processing.sequences[stream],
		},
		ObservedAt:      observedAt,
		DeviceTimestamp: deviceTimestamp,
		Causes:          append([]teleop.EventID(nil), causes...),
	}
}

func (*testProcessingContext) Now() time.Time { return time.Unix(100, 0) }
