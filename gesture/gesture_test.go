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
	if len(result) != 2 || result[1].Type != gesture.DoubleTap {
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
