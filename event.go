package teleop

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// EventKind is the stable persisted discriminator for an event payload.
type EventKind string

const (
	// EventObservation records a complete canonical controller state.
	EventObservation EventKind = "input.observation"
	// EventButton records one digital-control edge.
	EventButton EventKind = "input.button"
	// EventStick records one normalized stick change.
	EventStick EventKind = "input.stick"
	// EventTrigger records one normalized trigger change.
	EventTrigger EventKind = "input.trigger"
	// EventConnection records a controller lifecycle transition.
	EventConnection EventKind = "device.connection"
	// EventCapabilities records the controls exposed by a controller.
	EventCapabilities EventKind = "device.capabilities"
	// EventLiveness records observation-age state.
	EventLiveness EventKind = "stream.liveness"
	// EventGap records known or suspected input loss.
	EventGap EventKind = "stream.gap"
	// EventError records a pipeline or source error.
	EventError EventKind = "stream.error"
	// EventClock records a detected host wall-clock adjustment.
	EventClock EventKind = "stream.clock"
	// EventCommand records an application command at the actuation boundary.
	EventCommand EventKind = "command.issued"
)

// SessionID is the random 128-bit identity of one open controller session.
type SessionID [16]byte

// String returns the lowercase hexadecimal session identity.
func (id SessionID) String() string {
	return hex.EncodeToString(id[:])
}

// MarshalText implements encoding.TextMarshaler.
func (id SessionID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (id *SessionID) UnmarshalText(value []byte) error {
	decoded, err := hex.DecodeString(string(value))
	if err != nil {
		return fmt.Errorf("decode session ID: %w", err)
	}
	if len(decoded) != len(id) {
		return fmt.Errorf("decode session ID: got %d bytes, want %d", len(decoded), len(id))
	}
	copy(id[:], decoded)
	return nil
}

// EventID uniquely identifies an event within a controller session.
type EventID struct {
	Session  SessionID `json:"session"`
	Stream   string    `json:"stream"`
	Sequence uint64    `json:"sequence"`
}

// Header carries identity, timing, device, and causal metadata shared by every
// event.
type Header struct {
	ID          EventID   `json:"id"`
	DeviceID    DeviceID  `json:"device_id"`
	ObservedAt  time.Time `json:"observed_at"`
	ReceivedAt  time.Time `json:"received_at,omitempty"`
	PublishedAt time.Time `json:"published_at,omitempty"`

	// Monotonic is the session-relative monotonic reading taken when the event
	// was published. Unlike the wall-clock fields it survives clock steps, so
	// it is the authoritative source for event ordering and for any duration
	// measured during forensic reconstruction.
	Monotonic time.Duration `json:"monotonic"`
	// ReceivedMonotonic is the session-relative monotonic reading taken when
	// the originating observation reached the controller.
	ReceivedMonotonic time.Duration `json:"received_monotonic,omitempty"`

	DeviceTimestamp int64     `json:"device_timestamp,omitempty"`
	Causes          []EventID `json:"causes,omitempty"`
	Synthetic       bool      `json:"synthetic,omitempty"`
}

// Clone returns an isolated copy of the header.
func (h Header) Clone() Header {
	h.Causes = append([]EventID(nil), h.Causes...)
	return h
}

// Event is intentionally open: gesture, action, and third-party packages can
// publish derived events while retaining the same identity and causality model.
type Event interface {
	Header() Header
	Kind() EventKind
}

// EventCloner lets derived and third-party events provide isolation when an
// event contains reference-backed mutable data.
type EventCloner interface {
	CloneEvent() Event
}

// NativeInput retains backend-specific source data for forensic replay.
type NativeInput struct {
	Format string           `json:"format"`
	Data   []byte           `json:"data,omitempty"`
	Fields map[string]int64 `json:"fields,omitempty"`
}

func (n NativeInput) clone() NativeInput {
	n.Data = append([]byte(nil), n.Data...)
	if n.Fields != nil {
		n.Fields = make(map[string]int64, len(n.Fields))
		for key, value := range n.Fields {
			n.Fields[key] = value
		}
	}
	return n
}

// ObservationEvent records one complete canonical state transition.
type ObservationEvent struct {
	Meta     Header      `json:"header"`
	Native   NativeInput `json:"native"`
	Previous State       `json:"previous"`
	Current  State       `json:"current"`
}

// Header implements Event.
func (e ObservationEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (ObservationEvent) Kind() EventKind { return EventObservation }

// CloneEvent implements EventCloner.
func (e ObservationEvent) CloneEvent() Event {
	e.Meta = e.Meta.Clone()
	e.Native = e.Native.clone()
	e.Previous = e.Previous.Clone()
	e.Current = e.Current.Clone()
	return e
}

// ButtonEvent records one digital button or D-pad transition.
type ButtonEvent struct {
	Meta    Header    `json:"header"`
	Button  ControlID `json:"button"`
	Phase   Phase     `json:"phase"`
	Pressed bool      `json:"pressed"`
}

// Header implements Event.
func (e ButtonEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (ButtonEvent) Kind() EventKind { return EventButton }

// StickEvent records a normalized stick position and delta.
type StickEvent struct {
	Meta     Header  `json:"header"`
	Stick    StickID `json:"stick"`
	Position Stick   `json:"position"`
	Delta    Stick   `json:"delta"`
}

// Header implements Event.
func (e StickEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (StickEvent) Kind() EventKind { return EventStick }

// TriggerEvent records a normalized trigger position and delta.
type TriggerEvent struct {
	Meta     Header    `json:"header"`
	Trigger  TriggerID `json:"trigger"`
	Position float32   `json:"position"`
	Delta    float32   `json:"delta"`
}

// Header implements Event.
func (e TriggerEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (TriggerEvent) Kind() EventKind { return EventTrigger }

// ConnectionState is the lifecycle state carried by ConnectionEvent.
type ConnectionState string

const (
	// Connected reports an opened controller session.
	Connected ConnectionState = "connected"
	// Disconnected reports a terminal device or session transition.
	Disconnected ConnectionState = "disconnected"
)

// ConnectionEvent records a controller lifecycle transition and descriptor.
type ConnectionEvent struct {
	Meta       Header          `json:"header"`
	State      ConnectionState `json:"state"`
	Reason     string          `json:"reason,omitempty"`
	Descriptor Descriptor      `json:"descriptor"`
}

// Header implements Event.
func (e ConnectionEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (ConnectionEvent) Kind() EventKind { return EventConnection }

// CapabilitiesEvent records the controls and output features exposed by the
// open controller.
type CapabilitiesEvent struct {
	Meta         Header       `json:"header"`
	Capabilities Capabilities `json:"capabilities"`
}

// Header implements Event.
func (e CapabilitiesEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (CapabilitiesEvent) Kind() EventKind { return EventCapabilities }

// LivenessState distinguishes recent input from input older than the
// configured observation-age threshold. It represents transport health only
// for backends known to emit periodic observations.
type LivenessState string

const (
	// LivenessHealthy reports input newer than the configured threshold.
	LivenessHealthy LivenessState = "healthy"
	// LivenessStale reports input older than the configured threshold.
	LivenessStale LivenessState = "stale"
)

// LivenessEvent periodically reports the age of the most recently received
// complete observation.
type LivenessEvent struct {
	Meta         Header        `json:"header"`
	State        LivenessState `json:"state"`
	LastObserved time.Time     `json:"last_observed,omitempty"`
	LastReceived time.Time     `json:"last_received,omitempty"`
	Age          time.Duration `json:"age"`
}

// Header implements Event.
func (e LivenessEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (LivenessEvent) Kind() EventKind { return EventLiveness }

// GapEvent records known or suspected loss in a source, pipeline, or
// subscription stream.
type GapEvent struct {
	Meta    Header `json:"header"`
	Source  string `json:"source"`
	Dropped uint64 `json:"dropped,omitempty"`
	Reason  string `json:"reason"`
}

// Header implements Event.
func (e GapEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (GapEvent) Kind() EventKind { return EventGap }

// ErrorEvent records a source or pipeline error while retaining its Go error
// for in-process consumers.
type ErrorEvent struct {
	Meta    Header `json:"header"`
	Message string `json:"message"`
	Err     error  `json:"-"`
}

// Header implements Event.
func (e ErrorEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (ErrorEvent) Kind() EventKind { return EventError }

// ClockEvent reports that the host wall clock moved relative to the monotonic
// clock by more than the configured step threshold. Wall-clock timestamps
// recorded before and after a step are not directly comparable; the Monotonic
// header field remains authoritative across one.
type ClockEvent struct {
	Meta Header `json:"header"`
	// Step is the signed wall-clock adjustment: positive when the wall clock
	// jumped forward relative to elapsed monotonic time.
	Step time.Duration `json:"step"`
	// Steps counts adjustments detected so far in this session.
	Steps  uint64 `json:"steps"`
	Reason string `json:"reason,omitempty"`
}

// Header implements Event.
func (e ClockEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (ClockEvent) Kind() EventKind { return EventClock }

// CommandEvent records a command an application issued to the system under
// control. Recording input alone leaves the causal chain incomplete: the
// liability question is usually what the machine was told to do, not what the
// operator's thumb did. Causes in the header link the command to the input
// events that produced it.
type CommandEvent struct {
	Meta    Header `json:"header"`
	Command string `json:"command"`
	// Payload is the application's own encoding of the command. It is written
	// to audit sinks verbatim.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Authorized reports whether a safety gate permitted the command at the
	// instant it was issued. A recorded unauthorized command means the
	// application computed one and the gate inhibited it.
	Authorized bool `json:"authorized"`
	// Reason explains an unauthorized or degraded command.
	Reason string `json:"reason,omitempty"`
}

// Header implements Event.
func (e CommandEvent) Header() Header { return e.Meta.Clone() }

// Kind implements Event.
func (CommandEvent) Kind() EventKind { return EventCommand }

// CloneEvent implements EventCloner.
func (e CommandEvent) CloneEvent() Event {
	e.Meta = e.Meta.Clone()
	e.Payload = append(json.RawMessage(nil), e.Payload...)
	return e
}

func cloneEvent(event Event) Event {
	if cloner, ok := event.(EventCloner); ok {
		return cloner.CloneEvent()
	}
	switch value := event.(type) {
	case ButtonEvent:
		value.Meta = value.Meta.Clone()
		return value
	case *ButtonEvent:
		cloned := *value
		cloned.Meta = cloned.Meta.Clone()
		return &cloned
	case StickEvent:
		value.Meta = value.Meta.Clone()
		return value
	case *StickEvent:
		cloned := *value
		cloned.Meta = cloned.Meta.Clone()
		return &cloned
	case TriggerEvent:
		value.Meta = value.Meta.Clone()
		return value
	case *TriggerEvent:
		cloned := *value
		cloned.Meta = cloned.Meta.Clone()
		return &cloned
	case ConnectionEvent:
		value.Meta = value.Meta.Clone()
		value.Descriptor = value.Descriptor.Clone()
		return value
	case *ConnectionEvent:
		cloned := *value
		cloned.Meta = cloned.Meta.Clone()
		cloned.Descriptor = cloned.Descriptor.Clone()
		return &cloned
	case CapabilitiesEvent:
		value.Meta = value.Meta.Clone()
		value.Capabilities = value.Capabilities.Clone()
		return value
	case *CapabilitiesEvent:
		cloned := *value
		cloned.Meta = cloned.Meta.Clone()
		cloned.Capabilities = cloned.Capabilities.Clone()
		return &cloned
	case LivenessEvent:
		value.Meta = value.Meta.Clone()
		return value
	case *LivenessEvent:
		cloned := *value
		cloned.Meta = cloned.Meta.Clone()
		return &cloned
	case GapEvent:
		value.Meta = value.Meta.Clone()
		return value
	case *GapEvent:
		cloned := *value
		cloned.Meta = cloned.Meta.Clone()
		return &cloned
	case ErrorEvent:
		value.Meta = value.Meta.Clone()
		return value
	case *ErrorEvent:
		cloned := *value
		cloned.Meta = cloned.Meta.Clone()
		return &cloned
	case ClockEvent:
		value.Meta = value.Meta.Clone()
		return value
	case *ClockEvent:
		cloned := *value
		cloned.Meta = cloned.Meta.Clone()
		return &cloned
	default:
		return event
	}
}

// ControlOf returns the physical control associated with a standard input
// event. Not every event has one.
func ControlOf(event Event) (ControlID, bool) {
	switch value := event.(type) {
	case ButtonEvent:
		return value.Button, true
	case *ButtonEvent:
		return value.Button, true
	case StickEvent:
		if value.Stick == LeftStick {
			return StickLeft, true
		}
		if value.Stick == RightStick {
			return StickRight, true
		}
		return "", false
	case *StickEvent:
		if value.Stick == LeftStick {
			return StickLeft, true
		}
		if value.Stick == RightStick {
			return StickRight, true
		}
		return "", false
	case TriggerEvent:
		if value.Trigger == LeftTrigger {
			return TriggerLeft, true
		}
		if value.Trigger == RightTrigger {
			return TriggerRight, true
		}
		return "", false
	case *TriggerEvent:
		if value.Trigger == LeftTrigger {
			return TriggerLeft, true
		}
		if value.Trigger == RightTrigger {
			return TriggerRight, true
		}
		return "", false
	default:
		return "", false
	}
}
