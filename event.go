package teleop

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type EventKind string

const (
	EventObservation  EventKind = "input.observation"
	EventButton       EventKind = "input.button"
	EventStick        EventKind = "input.stick"
	EventTrigger      EventKind = "input.trigger"
	EventConnection   EventKind = "device.connection"
	EventCapabilities EventKind = "device.capabilities"
	EventLiveness     EventKind = "stream.liveness"
	EventGap          EventKind = "stream.gap"
	EventError        EventKind = "stream.error"
	EventClock        EventKind = "stream.clock"
	EventCommand      EventKind = "command.issued"
)

type SessionID [16]byte

func (id SessionID) String() string {
	return hex.EncodeToString(id[:])
}

func (id SessionID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}

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

type EventID struct {
	Session  SessionID `json:"session"`
	Stream   string    `json:"stream"`
	Sequence uint64    `json:"sequence"`
}

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

type ObservationEvent struct {
	Meta     Header      `json:"header"`
	Native   NativeInput `json:"native"`
	Previous State       `json:"previous"`
	Current  State       `json:"current"`
}

func (e ObservationEvent) Header() Header { return e.Meta.Clone() }
func (ObservationEvent) Kind() EventKind  { return EventObservation }
func (e ObservationEvent) CloneEvent() Event {
	e.Meta = e.Meta.Clone()
	e.Native = e.Native.clone()
	e.Previous = e.Previous.Clone()
	e.Current = e.Current.Clone()
	return e
}

type ButtonEvent struct {
	Meta    Header    `json:"header"`
	Button  ControlID `json:"button"`
	Phase   Phase     `json:"phase"`
	Pressed bool      `json:"pressed"`
}

func (e ButtonEvent) Header() Header { return e.Meta.Clone() }
func (ButtonEvent) Kind() EventKind  { return EventButton }

type StickEvent struct {
	Meta     Header  `json:"header"`
	Stick    StickID `json:"stick"`
	Position Stick   `json:"position"`
	Delta    Stick   `json:"delta"`
}

func (e StickEvent) Header() Header { return e.Meta.Clone() }
func (StickEvent) Kind() EventKind  { return EventStick }

type TriggerEvent struct {
	Meta     Header    `json:"header"`
	Trigger  TriggerID `json:"trigger"`
	Position float32   `json:"position"`
	Delta    float32   `json:"delta"`
}

func (e TriggerEvent) Header() Header { return e.Meta.Clone() }
func (TriggerEvent) Kind() EventKind  { return EventTrigger }

type ConnectionState string

const (
	Connected    ConnectionState = "connected"
	Disconnected ConnectionState = "disconnected"
)

type ConnectionEvent struct {
	Meta       Header          `json:"header"`
	State      ConnectionState `json:"state"`
	Reason     string          `json:"reason,omitempty"`
	Descriptor Descriptor      `json:"descriptor"`
}

func (e ConnectionEvent) Header() Header { return e.Meta.Clone() }
func (ConnectionEvent) Kind() EventKind  { return EventConnection }

type CapabilitiesEvent struct {
	Meta         Header       `json:"header"`
	Capabilities Capabilities `json:"capabilities"`
}

func (e CapabilitiesEvent) Header() Header { return e.Meta.Clone() }
func (CapabilitiesEvent) Kind() EventKind  { return EventCapabilities }

// LivenessState distinguishes recent input from input older than the
// configured observation-age threshold. It represents transport health only
// for backends known to emit periodic observations.
type LivenessState string

const (
	LivenessHealthy LivenessState = "healthy"
	LivenessStale   LivenessState = "stale"
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

func (e LivenessEvent) Header() Header { return e.Meta.Clone() }
func (LivenessEvent) Kind() EventKind  { return EventLiveness }

type GapEvent struct {
	Meta    Header `json:"header"`
	Source  string `json:"source"`
	Dropped uint64 `json:"dropped,omitempty"`
	Reason  string `json:"reason"`
}

func (e GapEvent) Header() Header { return e.Meta.Clone() }
func (GapEvent) Kind() EventKind  { return EventGap }

type ErrorEvent struct {
	Meta    Header `json:"header"`
	Message string `json:"message"`
	Err     error  `json:"-"`
}

func (e ErrorEvent) Header() Header { return e.Meta.Clone() }
func (ErrorEvent) Kind() EventKind  { return EventError }

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

func (e ClockEvent) Header() Header { return e.Meta.Clone() }
func (ClockEvent) Kind() EventKind  { return EventClock }

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

func (e CommandEvent) Header() Header { return e.Meta.Clone() }
func (CommandEvent) Kind() EventKind  { return EventCommand }
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
