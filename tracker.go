package teleop

import "sort"

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
	for id := range previous.Buttons.Extensions {
		if !isStandardButton(id) {
			extensions = append(extensions, id)
		}
	}
	for id := range current.Buttons.Extensions {
		if !isStandardButton(id) {
			extensions = append(extensions, id)
		}
	}
	sort.Slice(extensions, func(i, j int) bool { return extensions[i] < extensions[j] })
	var previousID ControlID
	for index, id := range extensions {
		if index > 0 && id == previousID {
			continue
		}
		appendButton(id)
		previousID = id
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
	for _, standard := range standardButtons {
		if id == standard {
			return true
		}
	}
	return false
}
