package teleop

import "sort"

func diffEvents(previous, current State, header func() Header) []Event {
	var events []Event
	buttons := StandardButtonIDs()
	seen := make(map[ControlID]bool)
	for _, id := range buttons {
		seen[id] = true
	}
	var extensions []ControlID
	for id := range previous.Buttons.Extensions {
		if !seen[id] {
			extensions = append(extensions, id)
			seen[id] = true
		}
	}
	for id := range current.Buttons.Extensions {
		if !seen[id] {
			extensions = append(extensions, id)
			seen[id] = true
		}
	}
	sort.Slice(extensions, func(i, j int) bool { return extensions[i] < extensions[j] })
	buttons = append(buttons, extensions...)
	for _, id := range buttons {
		before := previous.Button(id)
		after := current.Button(id)
		if before == after {
			continue
		}
		phase := PhaseReleased
		if after {
			phase = PhasePressed
		}
		events = append(events, ButtonEvent{
			Meta:    header(),
			Button:  id,
			Phase:   phase,
			Pressed: after,
		})
	}

	if previous.LeftStick != current.LeftStick {
		events = append(events, StickEvent{
			Meta:     header(),
			Stick:    LeftStick,
			Position: current.LeftStick,
			Delta: Stick{
				X: current.LeftStick.X - previous.LeftStick.X,
				Y: current.LeftStick.Y - previous.LeftStick.Y,
			},
		})
	}
	if previous.RightStick != current.RightStick {
		events = append(events, StickEvent{
			Meta:     header(),
			Stick:    RightStick,
			Position: current.RightStick,
			Delta: Stick{
				X: current.RightStick.X - previous.RightStick.X,
				Y: current.RightStick.Y - previous.RightStick.Y,
			},
		})
	}
	if previous.LeftTrigger != current.LeftTrigger {
		events = append(events, TriggerEvent{
			Meta:     header(),
			Trigger:  LeftTrigger,
			Position: current.LeftTrigger,
			Delta:    current.LeftTrigger - previous.LeftTrigger,
		})
	}
	if previous.RightTrigger != current.RightTrigger {
		events = append(events, TriggerEvent{
			Meta:     header(),
			Trigger:  RightTrigger,
			Position: current.RightTrigger,
			Delta:    current.RightTrigger - previous.RightTrigger,
		})
	}
	return events
}
