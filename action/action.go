// Package action maps physical input and gestures onto application-defined
// semantic actions.
package action

import (
	"sync"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/gesture"
)

type ID string

const EventKind teleop.EventKind = "action"

type Value struct {
	Pressed bool         `json:"pressed,omitempty"`
	Scalar  float32      `json:"scalar,omitempty"`
	Vector  teleop.Stick `json:"vector,omitempty"`
}

type Event struct {
	Meta    teleop.Header    `json:"header"`
	Action  ID               `json:"action"`
	Phase   teleop.Phase     `json:"phase"`
	Control teleop.ControlID `json:"control,omitempty"`
	Value   Value            `json:"value"`
}

func (e Event) Header() teleop.Header { return e.Meta.Clone() }
func (Event) Kind() teleop.EventKind  { return EventKind }

// Binding matches an event kind and optional control, phase, and gesture type.
// Empty match fields act as wildcards.
type Binding struct {
	Action      ID
	EventKind   teleop.EventKind
	Control     teleop.ControlID
	Phase       teleop.Phase
	GestureType gesture.Type
}

func OnButton(action ID, button teleop.ControlID, phase teleop.Phase) Binding {
	return Binding{
		Action:    action,
		EventKind: teleop.EventButton,
		Control:   button,
		Phase:     phase,
	}
}

func OnDPad(action ID, direction teleop.ControlID, phase teleop.Phase) Binding {
	return Binding{
		Action:    action,
		EventKind: teleop.EventButton,
		Control:   direction,
		Phase:     phase,
	}
}

func OnGesture(action ID, gestureType gesture.Type, control teleop.ControlID) Binding {
	return Binding{
		Action:      action,
		EventKind:   gesture.EventKind,
		Control:     control,
		GestureType: gestureType,
	}
}

func OnStick(action ID, stick teleop.StickID) Binding {
	control := teleop.StickRight
	if stick == teleop.LeftStick {
		control = teleop.StickLeft
	}
	return Binding{
		Action:    action,
		EventKind: teleop.EventStick,
		Control:   control,
	}
}

func OnTrigger(action ID, trigger teleop.TriggerID) Binding {
	control := teleop.TriggerRight
	if trigger == teleop.LeftTrigger {
		control = teleop.TriggerLeft
	}
	return Binding{
		Action:    action,
		EventKind: teleop.EventTrigger,
		Control:   control,
	}
}

type Mapper struct {
	mu       sync.Mutex
	bindings []Binding
	sequence uint64
}

func New(bindings ...Binding) *Mapper {
	return &Mapper{bindings: append([]Binding(nil), bindings...)}
}

// Process implements teleop.Processor.
func (m *Mapper) Process(input teleop.Event) []teleop.Event {
	mapped := m.Map(input)
	result := make([]teleop.Event, len(mapped))
	for index := range mapped {
		result[index] = mapped[index]
	}
	return result
}

// Map returns strongly typed application actions.
func (m *Mapper) Map(input teleop.Event) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []Event
	for _, binding := range m.bindings {
		control, phase, value, gestureType := eventValues(input)
		if binding.EventKind != "" && input.Kind() != binding.EventKind {
			continue
		}
		if binding.Control != "" && binding.Control != control {
			continue
		}
		if binding.Phase != "" && binding.Phase != phase {
			continue
		}
		if binding.GestureType != "" && binding.GestureType != gestureType {
			continue
		}
		source := input.Header()
		m.sequence++
		result = append(result, Event{
			Meta: teleop.Header{
				ID: teleop.EventID{
					Session:  source.ID.Session,
					Stream:   "action",
					Sequence: m.sequence,
				},
				DeviceID:   source.DeviceID,
				ObservedAt: source.ObservedAt,
				Causes:     []teleop.EventID{source.ID},
			},
			Action:  binding.Action,
			Phase:   phase,
			Control: control,
			Value:   value,
		})
	}
	return result
}

func eventValues(input teleop.Event) (teleop.ControlID, teleop.Phase, Value, gesture.Type) {
	switch event := input.(type) {
	case teleop.ButtonEvent:
		return event.Button, event.Phase, Value{Pressed: event.Pressed}, ""
	case *teleop.ButtonEvent:
		return event.Button, event.Phase, Value{Pressed: event.Pressed}, ""
	case teleop.StickEvent:
		control := teleop.StickRight
		if event.Stick == teleop.LeftStick {
			control = teleop.StickLeft
		}
		return control, teleop.PhaseChanged, Value{Vector: event.Position}, ""
	case *teleop.StickEvent:
		control := teleop.StickRight
		if event.Stick == teleop.LeftStick {
			control = teleop.StickLeft
		}
		return control, teleop.PhaseChanged, Value{Vector: event.Position}, ""
	case teleop.TriggerEvent:
		control := teleop.TriggerRight
		if event.Trigger == teleop.LeftTrigger {
			control = teleop.TriggerLeft
		}
		return control, teleop.PhaseChanged, Value{Scalar: event.Position}, ""
	case *teleop.TriggerEvent:
		control := teleop.TriggerRight
		if event.Trigger == teleop.LeftTrigger {
			control = teleop.TriggerLeft
		}
		return control, teleop.PhaseChanged, Value{Scalar: event.Position}, ""
	case gesture.Event:
		var control teleop.ControlID
		if len(event.Controls) > 0 {
			control = event.Controls[0]
		}
		return control, event.Phase, Value{Scalar: event.Value}, event.Type
	case *gesture.Event:
		var control teleop.ControlID
		if len(event.Controls) > 0 {
			control = event.Controls[0]
		}
		return control, event.Phase, Value{Scalar: event.Value}, event.Type
	default:
		return "", "", Value{}, ""
	}
}
