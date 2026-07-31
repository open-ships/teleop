// Package action maps physical input and gestures onto application-defined
// semantic actions.
package action

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/gesture"
)

// ID identifies an application-defined action.
type ID string

// EventKind is the persisted kind for action events.
const EventKind teleop.EventKind = "action"

// Value carries the input value that caused an action.
type Value struct {
	Pressed bool          `json:"pressed,omitempty"`
	Scalar  float32       `json:"scalar,omitempty"`
	Vector  *teleop.Stick `json:"vector,omitempty"`
}

// Event is one mapped semantic action.
type Event struct {
	Meta     teleop.Header      `json:"header"`
	Action   ID                 `json:"action"`
	Phase    teleop.Phase       `json:"phase"`
	Control  teleop.ControlID   `json:"control,omitempty"`
	Controls []teleop.ControlID `json:"controls,omitempty"`
	Value    Value              `json:"value"`
}

// Header implements teleop.Event.
func (e Event) Header() teleop.Header { return e.Meta.Clone() }

// Kind implements teleop.Event.
func (Event) Kind() teleop.EventKind { return EventKind }

// CloneEvent implements teleop.EventCloner.
func (e Event) CloneEvent() teleop.Event {
	e.Meta = e.Meta.Clone()
	e.Controls = slices.Clone(e.Controls)
	if e.Value.Vector != nil {
		vector := *e.Value.Vector
		e.Value.Vector = &vector
	}
	return e
}

// Binding matches an event kind and optional exact controls, phase, gesture
// type, and connection state. Empty match fields are wildcards.
type Binding struct {
	Action          ID
	EventKind       teleop.EventKind
	Control         teleop.ControlID
	Controls        []teleop.ControlID
	Phase           teleop.Phase
	GestureType     gesture.Type
	Region          string
	ConnectionState teleop.ConnectionState
}

// OnButton binds a button transition to action.
func OnButton(action ID, button teleop.ControlID, phase teleop.Phase) Binding {
	return Binding{Action: action, EventKind: teleop.EventButton, Control: button, Phase: phase}
}

// OnDPad binds a D-pad direction transition to action.
func OnDPad(action ID, direction teleop.ControlID, phase teleop.Phase) Binding {
	return OnButton(action, direction, phase)
}

// OnGesture binds the single meaningful phase: ended for taps and started for
// stateful gestures. Use OnGesturePhase when both edges are meaningful.
func OnGesture(action ID, gestureType gesture.Type, control teleop.ControlID) Binding {
	phase := teleop.PhaseStarted
	if gestureType == gesture.Tap || gestureType == gesture.DoubleTap {
		phase = teleop.PhaseEnded
	}
	return OnGesturePhase(action, gestureType, control, phase)
}

// OnGesturePhase binds one phase of a recognized gesture to action.
func OnGesturePhase(
	action ID,
	gestureType gesture.Type,
	control teleop.ControlID,
	phase teleop.Phase,
) Binding {
	return Binding{
		Action:      action,
		EventKind:   gesture.EventKind,
		Control:     control,
		Phase:       phase,
		GestureType: gestureType,
	}
}

// OnChord binds an exact chord by its configured name and controls.
func OnChord(action ID, name string, controls ...teleop.ControlID) Binding {
	return Binding{
		Action:      action,
		EventKind:   gesture.EventKind,
		Controls:    canonicalControls(controls),
		Phase:       teleop.PhaseStarted,
		GestureType: gesture.Chord,
		Region:      name,
	}
}

// OnStick binds changes from one normalized stick to action.
func OnStick(action ID, stick teleop.StickID) Binding {
	control, _ := stickControl(stick)
	return Binding{Action: action, EventKind: teleop.EventStick, Control: control}
}

// OnTrigger binds changes from one normalized trigger to action.
func OnTrigger(action ID, trigger teleop.TriggerID) Binding {
	control, _ := triggerControl(trigger)
	return Binding{Action: action, EventKind: teleop.EventTrigger, Control: control}
}

// OnConnection binds a lifecycle transition such as a disconnect failsafe.
func OnConnection(action ID, state teleop.ConnectionState) Binding {
	return Binding{
		Action:          action,
		EventKind:       teleop.EventConnection,
		ConnectionState: state,
	}
}

var mapperInstances atomic.Uint64

// Mapper is immutable after construction and safe for concurrent Process calls.
type Mapper struct {
	mu             sync.Mutex
	bindings       []Binding
	fallbackStream string
	fallbackSeq    map[teleop.SessionID]uint64
}

// New validates and copies bindings. Invalid bindings panic instead of silently
// matching a different control.
func New(bindings ...Binding) *Mapper {
	copied := slices.Clone(bindings)
	for index := range copied {
		copied[index].Controls = canonicalControls(copied[index].Controls)
		if err := validateBinding(copied[index]); err != nil {
			panic(err)
		}
	}
	instance := mapperInstances.Add(1)
	return &Mapper{
		bindings:       copied,
		fallbackStream: fmt.Sprintf("action/%d", instance),
		fallbackSeq:    make(map[teleop.SessionID]uint64),
	}
}

func validateBinding(binding Binding) error {
	if binding.Action == "" {
		return fmt.Errorf("action: binding has an empty action")
	}
	if binding.EventKind == teleop.EventStick &&
		binding.Control != teleop.StickLeft &&
		binding.Control != teleop.StickRight {
		return fmt.Errorf("action: invalid stick control %q", binding.Control)
	}
	if binding.EventKind == teleop.EventTrigger &&
		binding.Control != teleop.TriggerLeft &&
		binding.Control != teleop.TriggerRight {
		return fmt.Errorf("action: invalid trigger control %q", binding.Control)
	}
	if binding.GestureType == gesture.Chord && len(binding.Controls) < 2 {
		return fmt.Errorf("action: chord binding needs at least two controls")
	}
	if binding.GestureType == gesture.Chord && binding.Region == "" {
		return fmt.Errorf("action: chord binding needs its configured name")
	}
	if binding.ConnectionState != "" &&
		binding.ConnectionState != teleop.Connected &&
		binding.ConnectionState != teleop.Disconnected {
		return fmt.Errorf("action: invalid connection state %q", binding.ConnectionState)
	}
	if binding.ConnectionState != "" && binding.EventKind != teleop.EventConnection {
		return fmt.Errorf("action: connection state requires a connection event binding")
	}
	if binding.Region != "" && binding.EventKind != gesture.EventKind {
		return fmt.Errorf("action: gesture region requires a gesture event binding")
	}
	if binding.Control != "" && len(binding.Controls) > 0 {
		return fmt.Errorf("action: binding cannot match both one control and an exact control set")
	}
	return nil
}

// Process implements teleop.Processor.
func (m *Mapper) Process(input teleop.Event) []teleop.Event {
	mapped := m.Map(input)
	return asEvents(mapped)
}

// ProcessContext implements teleop.ContextProcessor.
func (m *Mapper) ProcessContext(
	_ context.Context,
	processing teleop.ProcessingContext,
	input teleop.Event,
) ([]teleop.Event, error) {
	mapped := m.mapInput(input, processing)
	return asEvents(mapped), nil
}

// Map returns strongly typed application actions using an instance-unique
// standalone event stream.
func (m *Mapper) Map(input teleop.Event) []Event {
	return m.mapInput(input, nil)
}

func (m *Mapper) mapInput(
	input teleop.Event,
	processing teleop.ProcessingContext,
) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()

	values, handled := eventValues(input)
	if !handled {
		return nil
	}
	source := input.Header()
	result := make([]Event, 0, len(m.bindings))
	for _, binding := range m.bindings {
		if binding.EventKind != "" && input.Kind() != binding.EventKind ||
			binding.Control != "" && binding.Control != values.control ||
			len(binding.Controls) > 0 && !equalControls(binding.Controls, values.controls) ||
			binding.Phase != "" && binding.Phase != values.phase ||
			binding.GestureType != "" && binding.GestureType != values.gestureType ||
			binding.Region != "" && binding.Region != values.region ||
			binding.ConnectionState != "" &&
				binding.ConnectionState != values.connectionState {
			continue
		}
		var header teleop.Header
		if processing != nil {
			header = processing.NewHeader(
				"action",
				source.ObservedAt,
				source.DeviceTimestamp,
				source.ID,
			)
			header.Synthetic = source.Synthetic
		} else {
			m.fallbackSeq[source.ID.Session]++
			header = teleop.Header{
				ID: teleop.EventID{
					Session:  source.ID.Session,
					Stream:   m.fallbackStream,
					Sequence: m.fallbackSeq[source.ID.Session],
				},
				DeviceID:        source.DeviceID,
				ObservedAt:      source.ObservedAt,
				ReceivedAt:      source.ReceivedAt,
				PublishedAt:     source.PublishedAt,
				DeviceTimestamp: source.DeviceTimestamp,
				Causes:          []teleop.EventID{source.ID},
				Synthetic:       source.Synthetic,
			}
		}
		result = append(result, Event{
			Meta:     header,
			Action:   binding.Action,
			Phase:    values.phase,
			Control:  values.control,
			Controls: slices.Clone(values.controls),
			Value:    values.value,
		})
	}
	return result
}

type extractedValues struct {
	control         teleop.ControlID
	controls        []teleop.ControlID
	phase           teleop.Phase
	value           Value
	gestureType     gesture.Type
	region          string
	connectionState teleop.ConnectionState
}

func eventValues(input teleop.Event) (extractedValues, bool) {
	switch event := input.(type) {
	case teleop.ButtonEvent:
		return extractedValues{
			control: event.Button, controls: []teleop.ControlID{event.Button},
			phase: event.Phase, value: Value{Pressed: event.Pressed},
		}, true
	case *teleop.ButtonEvent:
		return eventValues(*event)
	case teleop.StickEvent:
		control, ok := stickControl(event.Stick)
		if !ok {
			return extractedValues{}, false
		}
		vector := event.Position
		return extractedValues{
			control: control, controls: []teleop.ControlID{control},
			phase: teleop.PhaseChanged, value: Value{Vector: &vector},
		}, true
	case *teleop.StickEvent:
		return eventValues(*event)
	case teleop.TriggerEvent:
		control, ok := triggerControl(event.Trigger)
		if !ok {
			return extractedValues{}, false
		}
		return extractedValues{
			control: control, controls: []teleop.ControlID{control},
			phase: teleop.PhaseChanged, value: Value{Scalar: event.Position},
		}, true
	case *teleop.TriggerEvent:
		return eventValues(*event)
	case gesture.Event:
		controls := canonicalControls(event.Controls)
		var control teleop.ControlID
		if len(controls) == 1 {
			control = controls[0]
		}
		return extractedValues{
			control: control, controls: controls, phase: event.Phase,
			value: Value{Scalar: event.Value}, gestureType: event.Type,
			region: event.Region,
		}, true
	case *gesture.Event:
		return eventValues(*event)
	case teleop.ConnectionEvent:
		return extractedValues{
			phase: connectionPhase(event.State), connectionState: event.State,
		}, true
	case *teleop.ConnectionEvent:
		return eventValues(*event)
	default:
		return extractedValues{}, false
	}
}

func connectionPhase(state teleop.ConnectionState) teleop.Phase {
	if state == teleop.Connected {
		return teleop.PhaseStarted
	}
	return teleop.PhaseEnded
}

func stickControl(stick teleop.StickID) (teleop.ControlID, bool) {
	switch stick {
	case teleop.LeftStick:
		return teleop.StickLeft, true
	case teleop.RightStick:
		return teleop.StickRight, true
	default:
		return "", false
	}
}

func triggerControl(trigger teleop.TriggerID) (teleop.ControlID, bool) {
	switch trigger {
	case teleop.LeftTrigger:
		return teleop.TriggerLeft, true
	case teleop.RightTrigger:
		return teleop.TriggerRight, true
	default:
		return "", false
	}
}

// asEvents widens mapped actions to the open teleop.Event interface.
func asEvents[T teleop.Event](values []T) []teleop.Event {
	result := make([]teleop.Event, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}

func canonicalControls(values []teleop.ControlID) []teleop.ControlID {
	sorted := slices.Compact(slices.Sorted(slices.Values(values)))
	return slices.DeleteFunc(sorted, func(value teleop.ControlID) bool { return value == "" })
}

func equalControls(left, right []teleop.ControlID) bool {
	return slices.Equal(left, right)
}
