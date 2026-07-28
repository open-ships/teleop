package teleop

import "testing"

func TestDiffEventsReportsDPadAsButtons(t *testing.T) {
	t.Parallel()

	var sequence uint64
	header := func() Header {
		sequence++
		return Header{ID: EventID{Sequence: sequence}}
	}

	current := State{
		DPad: DPad{
			Up:   true,
			Left: true,
		},
	}
	events := diffEvents(State{}, current, header)
	if len(events) != 2 {
		t.Fatalf("press events = %#v, want two changed-direction events", events)
	}
	assertButtonEvent(t, events[0], DPadUp, PhasePressed, true)
	assertButtonEvent(t, events[1], DPadLeft, PhasePressed, true)

	events = diffEvents(current, State{}, header)
	if len(events) != 2 {
		t.Fatalf("release events = %#v, want two changed-direction events", events)
	}
	assertButtonEvent(t, events[0], DPadUp, PhaseReleased, false)
	assertButtonEvent(t, events[1], DPadLeft, PhaseReleased, false)
}

func TestDPadIDsUseButtonNamespace(t *testing.T) {
	t.Parallel()

	want := map[ControlID]bool{
		"button.dpad.up":    false,
		"button.dpad.down":  false,
		"button.dpad.left":  false,
		"button.dpad.right": false,
	}
	for _, id := range StandardButtonIDs() {
		if _, exists := want[id]; exists {
			want[id] = true
		}
	}
	for id, found := range want {
		if !found {
			t.Errorf("StandardButtonIDs does not contain %q", id)
		}
	}
}

func TestDiffEventsCoversEveryStateShapeInDeterministicOrder(t *testing.T) {
	var sequence uint64
	header := func() Header {
		sequence++
		return Header{ID: EventID{Sequence: sequence}}
	}
	current := State{
		LeftStick:    Stick{X: -0.5, Y: 0.75},
		RightStick:   Stick{X: 0.25, Y: -1},
		LeftTrigger:  0.4,
		RightTrigger: 1,
	}
	current.SetButton(ButtonFaceNorth, true)
	current.SetButton("button.extension.z", true)
	current.SetButton("button.extension.a", true)

	events := diffEvents(State{}, current, header)
	if len(events) != 7 {
		t.Fatalf("events = %#v, want 7 state changes", events)
	}
	assertButtonEvent(t, events[0], ButtonFaceNorth, PhasePressed, true)
	assertButtonEvent(t, events[1], "button.extension.a", PhasePressed, true)
	assertButtonEvent(t, events[2], "button.extension.z", PhasePressed, true)
	if event, ok := events[3].(StickEvent); !ok ||
		event.Stick != LeftStick || event.Position != current.LeftStick {
		t.Fatalf("left-stick event = %#v", events[3])
	}
	if event, ok := events[4].(StickEvent); !ok ||
		event.Stick != RightStick || event.Position != current.RightStick {
		t.Fatalf("right-stick event = %#v", events[4])
	}
	if event, ok := events[5].(TriggerEvent); !ok ||
		event.Trigger != LeftTrigger || event.Position != current.LeftTrigger {
		t.Fatalf("left-trigger event = %#v", events[5])
	}
	if event, ok := events[6].(TriggerEvent); !ok ||
		event.Trigger != RightTrigger || event.Position != current.RightTrigger {
		t.Fatalf("right-trigger event = %#v", events[6])
	}
}

func assertButtonEvent(
	t *testing.T,
	event Event,
	button ControlID,
	phase Phase,
	pressed bool,
) {
	t.Helper()

	value, ok := event.(ButtonEvent)
	if !ok {
		t.Fatalf("event type = %T, want ButtonEvent", event)
	}
	if value.Button != button || value.Phase != phase || value.Pressed != pressed {
		t.Fatalf("event = %#v, want button=%q phase=%q pressed=%t",
			value, button, phase, pressed)
	}
	if control, ok := ControlOf(value); !ok || control != button {
		t.Fatalf("ControlOf(event) = %q, %t", control, ok)
	}
}
