package teleop

import "slices"

func diffEvents(previous, current State, header func() Header) []Event {
	var events []Event
	appendButton := func(id ControlID) {
		before := previous.Button(id)
		after := current.Button(id)
		if before == after {
			return
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
	for _, id := range standardButtons {
		appendButton(id)
	}

	var extensions []ControlID
	for _, set := range []map[ControlID]bool{previous.Buttons.Extensions, current.Buttons.Extensions} {
		for id := range set {
			if !isStandardButton(id) {
				extensions = append(extensions, id)
			}
		}
	}
	slices.Sort(extensions)
	for _, id := range slices.Compact(extensions) {
		appendButton(id)
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

func isStandardButton(id ControlID) bool {
	return slices.Contains(standardButtons, id)
}
