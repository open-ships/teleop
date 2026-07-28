package gesture_test

import (
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/gesture"
)

func TestTapDoubleTapAndHold(t *testing.T) {
	t.Parallel()

	recognizer := gesture.New(gesture.Config{
		TapMaximum:       200 * time.Millisecond,
		DoubleTapWindow:  300 * time.Millisecond,
		HoldMinimum:      500 * time.Millisecond,
		StickThreshold:   0.5,
		TriggerThreshold: 0.5,
	})
	start := time.Unix(100, 0)
	press := buttonEvent(1, start, true)
	release := buttonEvent(2, start.Add(100*time.Millisecond), false)
	if result := recognizer.Recognize(press); len(result) != 0 {
		t.Fatalf("press emitted %#v", result)
	}
	result := recognizer.Recognize(release)
	if len(result) != 1 || result[0].Type != gesture.Tap {
		t.Fatalf("first release emitted %#v", result)
	}

	recognizer.Recognize(buttonEvent(3, start.Add(200*time.Millisecond), true))
	result = recognizer.Recognize(buttonEvent(4, start.Add(275*time.Millisecond), false))
	if len(result) != 1 || result[0].Type != gesture.DoubleTap {
		t.Fatalf("second release emitted %#v", result)
	}

	holdStart := start.Add(time.Second)
	recognizer.Recognize(buttonEvent(5, holdStart, true))
	result = recognizer.AdvanceGestures(holdStart.Add(600 * time.Millisecond))
	if len(result) != 1 || result[0].Type != gesture.Hold || result[0].Phase != teleop.PhaseStarted {
		t.Fatalf("hold advance emitted %#v", result)
	}
	result = recognizer.Recognize(buttonEvent(6, holdStart.Add(700*time.Millisecond), false))
	if len(result) != 1 || result[0].Phase != teleop.PhaseEnded {
		t.Fatalf("hold release emitted %#v", result)
	}
}

func TestStickRegionAndTriggerThreshold(t *testing.T) {
	t.Parallel()

	recognizer := gesture.New(gesture.DefaultConfig())
	header := teleop.Header{
		ID:         teleop.EventID{Stream: "input", Sequence: 1},
		ObservedAt: time.Now(),
	}
	stick := recognizer.Recognize(teleop.StickEvent{
		Meta:     header,
		Stick:    teleop.LeftStick,
		Position: teleop.Stick{Y: 1},
	})
	if len(stick) != 1 || stick[0].Region != "north" {
		t.Fatalf("stick gestures = %#v", stick)
	}
	trigger := recognizer.Recognize(teleop.TriggerEvent{
		Meta:     header,
		Trigger:  teleop.RightTrigger,
		Position: 0.75,
	})
	if len(trigger) != 1 || trigger[0].Type != gesture.TriggerThreshold {
		t.Fatalf("trigger gestures = %#v", trigger)
	}
}

func TestRecognizerIsolatesControllerSessions(t *testing.T) {
	t.Parallel()

	recognizer := gesture.New(gesture.Config{
		Chords: []gesture.ChordSpec{{
			Name: "arm",
			Buttons: []teleop.ControlID{
				teleop.ButtonBumperLeft,
				teleop.ButtonFaceSouth,
			},
		}},
	})
	at := time.Unix(100, 0)
	if got := recognizer.Recognize(controllerButtonEvent(
		1, 1, at, teleop.ButtonBumperLeft, true,
	)); len(got) != 0 {
		t.Fatalf("first session press = %#v", got)
	}
	if got := recognizer.Recognize(controllerButtonEvent(
		2, 1, at, teleop.ButtonFaceSouth, true,
	)); len(got) != 0 {
		t.Fatalf("cross-session chord = %#v", got)
	}
	got := recognizer.Recognize(controllerButtonEvent(
		1, 2, at.Add(time.Millisecond), teleop.ButtonFaceSouth, true,
	))
	if len(got) != 1 || got[0].Type != gesture.Chord || got[0].Region != "arm" {
		t.Fatalf("same-session chord = %#v", got)
	}
}

func TestDisconnectResetsAndTerminatesActiveGestures(t *testing.T) {
	t.Parallel()

	recognizer := gesture.New(gesture.Config{
		Chords: []gesture.ChordSpec{{
			Name: "arm",
			Buttons: []teleop.ControlID{
				teleop.ButtonBumperLeft,
				teleop.ButtonFaceSouth,
			},
		}},
	})
	at := time.Unix(200, 0)
	recognizer.Recognize(controllerButtonEvent(
		1, 1, at, teleop.ButtonBumperLeft, true,
	))
	chord := recognizer.Recognize(controllerButtonEvent(
		1, 2, at.Add(time.Millisecond), teleop.ButtonFaceSouth, true,
	))
	if len(chord) != 1 || chord[0].Phase != teleop.PhaseStarted {
		t.Fatalf("chord start = %#v", chord)
	}
	recognizer.Recognize(teleop.StickEvent{
		Meta:     gestureHeader(1, 3, at.Add(2*time.Millisecond)),
		Stick:    teleop.LeftStick,
		Position: teleop.Stick{Y: 1},
	})
	recognizer.Recognize(teleop.TriggerEvent{
		Meta:     gestureHeader(1, 4, at.Add(3*time.Millisecond)),
		Trigger:  teleop.LeftTrigger,
		Position: 1,
	})
	ended := recognizer.Recognize(teleop.ConnectionEvent{
		Meta:  gestureHeader(1, 5, at.Add(time.Second)),
		State: teleop.Disconnected,
	})
	if len(ended) != 3 ||
		ended[0].Type != gesture.Chord ||
		ended[1].Type != gesture.StickRegion ||
		ended[2].Type != gesture.TriggerThreshold {
		t.Fatalf("disconnect endings = %#v", ended)
	}
	for _, event := range ended {
		if event.Phase != teleop.PhaseEnded {
			t.Fatalf("disconnect phase = %#v", event)
		}
	}
	if later := recognizer.AdvanceGestures(at.Add(24 * time.Hour)); len(later) != 0 {
		t.Fatalf("state remained after disconnect = %#v", later)
	}
}

func TestDisconnectEndsOnlyAnAlreadyStartedHold(t *testing.T) {
	t.Parallel()

	config := gesture.DefaultConfig()
	recognizer := gesture.New(config)
	at := time.Unix(300, 0)
	press := controllerButtonEvent(1, 1, at, teleop.ButtonFaceSouth, true)
	recognizer.Recognize(press)
	started := recognizer.AdvanceGestures(at.Add(config.HoldMinimum))
	if len(started) != 1 || started[0].Phase != teleop.PhaseStarted {
		t.Fatalf("hold start = %#v", started)
	}
	disconnect := teleop.ConnectionEvent{
		Meta:  gestureHeader(1, 2, at.Add(config.HoldMinimum+time.Second)),
		State: teleop.Disconnected,
	}
	result := recognizer.Recognize(disconnect)
	if len(result) != 1 ||
		result[0].Type != gesture.Hold ||
		result[0].Phase != teleop.PhaseEnded {
		t.Fatalf("disconnect hold = %#v", result)
	}
	if len(result[0].Meta.Causes) != 2 ||
		result[0].Meta.Causes[0] != started[0].Meta.ID ||
		result[0].Meta.Causes[1] != disconnect.Meta.ID {
		t.Fatalf("hold end causes = %#v", result[0].Meta.Causes)
	}
}

func TestHoldAdvanceUsesEventTimelineNotLogAge(t *testing.T) {
	t.Parallel()

	config := gesture.DefaultConfig()
	recognizer := gesture.New(config)
	recordedAt := time.Unix(400, 0)
	recognizer.Recognize(controllerButtonEvent(
		1, 1, recordedAt, teleop.ButtonFaceSouth, true,
	))
	replayedNow := recordedAt.Add(24 * time.Hour)
	result := recognizer.AdvanceGestures(replayedNow)
	if len(result) != 1 {
		t.Fatalf("replayed hold = %#v", result)
	}
	if result[0].Meta.ObservedAt != recordedAt.Add(config.HoldMinimum) ||
		result[0].Duration != config.HoldMinimum {
		t.Fatalf("replayed hold timeline = %#v", result[0])
	}
}

func TestGestureOrderingAndFallbackIDsAreDeterministicPerSession(t *testing.T) {
	t.Parallel()

	config := gesture.DefaultConfig()
	recognizer := gesture.New(config)
	at := time.Unix(500, 0)
	recognizer.Recognize(controllerButtonEvent(
		1, 1, at, teleop.ButtonFaceSouth, true,
	))
	recognizer.Recognize(controllerButtonEvent(
		1, 2, at, teleop.ButtonFaceEast, true,
	))
	result := recognizer.AdvanceGestures(at.Add(config.HoldMinimum))
	if len(result) != 2 {
		t.Fatalf("holds = %#v", result)
	}
	if result[0].Controls[0] >= result[1].Controls[0] ||
		result[0].Meta.ID.Sequence != 1 ||
		result[1].Meta.ID.Sequence != 2 {
		t.Fatalf("hold order/IDs = %#v", result)
	}

	other := gesture.New(config)
	other.Recognize(controllerButtonEvent(
		1, 1, at, teleop.ButtonFaceSouth, true,
	))
	first := other.Recognize(controllerButtonEvent(
		1, 2, at.Add(time.Millisecond), teleop.ButtonFaceSouth, false,
	))
	other.Recognize(controllerButtonEvent(
		2, 1, at, teleop.ButtonFaceSouth, true,
	))
	second := other.Recognize(controllerButtonEvent(
		2, 2, at.Add(time.Millisecond), teleop.ButtonFaceSouth, false,
	))
	if len(first) != 1 || len(second) != 1 ||
		first[0].Meta.ID.Sequence != 1 ||
		second[0].Meta.ID.Sequence != 1 {
		t.Fatalf("per-session gesture IDs = %#v, %#v", first, second)
	}
}

func TestOutOfOrderReleaseDoesNotCreateTap(t *testing.T) {
	t.Parallel()

	recognizer := gesture.New(gesture.DefaultConfig())
	at := time.Unix(600, 0)
	recognizer.Recognize(controllerButtonEvent(
		1, 1, at, teleop.ButtonFaceSouth, true,
	))
	if result := recognizer.Recognize(controllerButtonEvent(
		1, 2, at.Add(-time.Second), teleop.ButtonFaceSouth, false,
	)); len(result) != 0 {
		t.Fatalf("out-of-order release = %#v", result)
	}
}

func TestRecognizerCopiesChordConfiguration(t *testing.T) {
	t.Parallel()

	buttons := []teleop.ControlID{
		teleop.ButtonBumperLeft,
		teleop.ButtonFaceSouth,
	}
	chords := []gesture.ChordSpec{{Name: "arm", Buttons: buttons}}
	recognizer := gesture.New(gesture.Config{Chords: chords})
	buttons[0] = teleop.ButtonFaceEast
	chords[0].Name = "mutated"

	at := time.Unix(700, 0)
	recognizer.Recognize(controllerButtonEvent(
		1, 1, at, teleop.ButtonBumperLeft, true,
	))
	result := recognizer.Recognize(controllerButtonEvent(
		1, 2, at.Add(time.Millisecond), teleop.ButtonFaceSouth, true,
	))
	if len(result) != 1 || result[0].Region != "arm" {
		t.Fatalf("copied chord config = %#v", result)
	}
}

func buttonEvent(sequence uint64, at time.Time, pressed bool) teleop.ButtonEvent {
	phase := teleop.PhaseReleased
	if pressed {
		phase = teleop.PhasePressed
	}
	return teleop.ButtonEvent{
		Meta: teleop.Header{
			ID:         teleop.EventID{Stream: "input", Sequence: sequence},
			ObservedAt: at,
		},
		Button:  teleop.ButtonFaceSouth,
		Phase:   phase,
		Pressed: pressed,
	}
}

func controllerButtonEvent(
	sessionByte byte,
	sequence uint64,
	at time.Time,
	button teleop.ControlID,
	pressed bool,
) teleop.ButtonEvent {
	phase := teleop.PhaseReleased
	if pressed {
		phase = teleop.PhasePressed
	}
	return teleop.ButtonEvent{
		Meta:    gestureHeader(sessionByte, sequence, at),
		Button:  button,
		Phase:   phase,
		Pressed: pressed,
	}
}

func gestureHeader(sessionByte byte, sequence uint64, at time.Time) teleop.Header {
	var session teleop.SessionID
	session[0] = sessionByte
	return teleop.Header{
		ID: teleop.EventID{
			Session:  session,
			Stream:   "input",
			Sequence: sequence,
		},
		DeviceID:   "controller",
		ObservedAt: at,
	}
}
