package teleop

import (
	"context"
	"time"
)

type DeviceID string

// AuditGrade states what the selected OS backend can honestly guarantee.
type AuditGrade string

const (
	AuditExactBackendStream AuditGrade = "exact-backend-stream"
	AuditSampledState       AuditGrade = "sampled-state"
	AuditUnavailable        AuditGrade = "unavailable"
)

type ControlKind string

const (
	ControlButton  ControlKind = "button"
	ControlStick   ControlKind = "stick"
	ControlTrigger ControlKind = "trigger"
	ControlDPad    ControlKind = "dpad"
	ControlOther   ControlKind = "other"
)

type ControlDescriptor struct {
	ID    ControlID   `json:"id"`
	Kind  ControlKind `json:"kind"`
	Label string      `json:"label,omitempty"`
}

type Capabilities struct {
	Controls   []ControlDescriptor `json:"controls"`
	AuditGrade AuditGrade          `json:"audit_grade"`
	Rumble     bool                `json:"rumble"`
	Battery    bool                `json:"battery"`
	LEDs       bool                `json:"leds"`
}

func (c Capabilities) Clone() Capabilities {
	c.Controls = append([]ControlDescriptor(nil), c.Controls...)
	return c
}

func (c Capabilities) Supports(id ControlID) bool {
	for _, control := range c.Controls {
		if control.ID == id {
			return true
		}
	}
	return false
}

type Descriptor struct {
	ID         DeviceID          `json:"id"`
	Type       ControllerType    `json:"type"`
	Name       string            `json:"name"`
	Transport  Transport         `json:"transport"`
	Backend    string            `json:"backend"`
	VendorID   uint16            `json:"vendor_id,omitempty"`
	ProductID  uint16            `json:"product_id,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
	Capability Capabilities      `json:"capabilities"`
}

func (d Descriptor) Clone() Descriptor {
	d.Capability = d.Capability.Clone()
	if d.Properties != nil {
		d.Properties = make(map[string]string, len(d.Properties))
		for key, value := range d.Properties {
			d.Properties[key] = value
		}
	}
	return d
}

// Provider discovers and opens one family of controllers.
type Provider interface {
	Type() ControllerType
	Discover(context.Context) ([]Descriptor, error)
	Open(context.Context, DeviceID, ...OpenOption) (GameController, error)
}

type DeviceEventKind string

const (
	DeviceAdded   DeviceEventKind = "added"
	DeviceRemoved DeviceEventKind = "removed"
	DeviceUpdated DeviceEventKind = "updated"
	DeviceError   DeviceEventKind = "error"
)

type DeviceEvent struct {
	Kind       DeviceEventKind `json:"kind"`
	Descriptor Descriptor      `json:"descriptor"`
	ObservedAt time.Time       `json:"observed_at"`
	Error      string          `json:"error,omitempty"`
}

// WatchingProvider is an optional provider capability for controller hotplug.
type WatchingProvider interface {
	Provider
	Watch(context.Context) <-chan DeviceEvent
}
