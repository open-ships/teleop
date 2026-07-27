package teleop

import (
	"encoding/hex"
	"fmt"
	"time"
)

type EventKind string

const (
	EventObservation  EventKind = "input.observation"
	EventButton       EventKind = "input.button"
	EventStick        EventKind = "input.stick"
	EventTrigger      EventKind = "input.trigger"
	EventDPad         EventKind = "input.dpad"
	EventConnection   EventKind = "device.connection"
	EventCapabilities EventKind = "device.capabilities"
	EventGap          EventKind = "stream.gap"
	EventError        EventKind = "stream.error"
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
	ID              EventID   `json:"id"`
	DeviceID        DeviceID  `json:"device_id"`
	ObservedAt      time.Time `json:"observed_at"`
	DeviceTimestamp int64     `json:"device_timestamp,omitempty"`
	Causes          []EventID `json:"causes,omitempty"`
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

type DPadEvent struct {
	Meta     Header `json:"header"`
	Previous DPad   `json:"previous"`
	Current  DPad   `json:"current"`
}

func (e DPadEvent) Header() Header { return e.Meta.Clone() }
func (DPadEvent) Kind() EventKind  { return EventDPad }

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
		return StickRight, true
	case *StickEvent:
		if value.Stick == LeftStick {
			return StickLeft, true
		}
		return StickRight, true
	case TriggerEvent:
		if value.Trigger == LeftTrigger {
			return TriggerLeft, true
		}
		return TriggerRight, true
	case *TriggerEvent:
		if value.Trigger == LeftTrigger {
			return TriggerLeft, true
		}
		return TriggerRight, true
	default:
		return "", false
	}
}
