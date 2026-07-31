package teleop

import (
	"context"
	"maps"
	"slices"
	"time"
)

// DeviceID is the provider-stable identity used to open a discovered device.
type DeviceID string

// AuditGrade states what the selected OS backend can honestly guarantee.
type AuditGrade string

const (
	// AuditExactBackendStream means the backend reports each exposed input
	// transition rather than reconstructing changes from sampled snapshots.
	AuditExactBackendStream AuditGrade = "exact-backend-stream"
	// AuditSampledState means the backend derives events from periodic state.
	AuditSampledState AuditGrade = "sampled-state"
	// AuditUnavailable means the backend makes no audit-delivery guarantee.
	AuditUnavailable AuditGrade = "unavailable"
)

// Valid reports whether grade is a guarantee understood by this format.
func (grade AuditGrade) Valid() bool {
	switch grade {
	case AuditExactBackendStream, AuditSampledState, AuditUnavailable:
		return true
	default:
		return false
	}
}

// ControlKind classifies a discovered physical control.
type ControlKind string

const (
	// ControlButton identifies a digital button.
	ControlButton ControlKind = "button"
	// ControlStick identifies a two-axis stick.
	ControlStick ControlKind = "stick"
	// ControlTrigger identifies a normalized analog trigger.
	ControlTrigger ControlKind = "trigger"
	// ControlDPad identifies one digital D-pad direction.
	ControlDPad ControlKind = "dpad"
	// ControlOther identifies a provider-specific control.
	ControlOther ControlKind = "other"
)

// ControlDescriptor identifies one control exposed by a device.
type ControlDescriptor struct {
	ID    ControlID   `json:"id"`
	Kind  ControlKind `json:"kind"`
	Label string      `json:"label,omitempty"`
}

// Capabilities describes the controls and optional output features a device
// exposes through its selected backend.
type Capabilities struct {
	Controls   []ControlDescriptor `json:"controls"`
	AuditGrade AuditGrade          `json:"audit_grade"`
	Rumble     bool                `json:"rumble"`
	Battery    bool                `json:"battery"`
	LEDs       bool                `json:"leds"`
}

// Clone returns an isolated copy with an explicit valid audit grade.
func (c Capabilities) Clone() Capabilities {
	if !c.AuditGrade.Valid() {
		c.AuditGrade = AuditUnavailable
	}
	c.Controls = slices.Clone(c.Controls)
	return c
}

// Supports reports whether id appears in the discovered control list.
func (c Capabilities) Supports(id ControlID) bool {
	return slices.ContainsFunc(c.Controls, func(control ControlDescriptor) bool {
		return control.ID == id
	})
}

// Descriptor contains the stable metadata and capabilities of one discovered
// controller.
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

// Clone returns an isolated copy of the device descriptor.
func (d Descriptor) Clone() Descriptor {
	d.Capability = d.Capability.Clone()
	d.Properties = maps.Clone(d.Properties)
	return d
}

// Provider discovers and opens one family of controllers.
type Provider interface {
	Type() ControllerType
	Discover(context.Context) ([]Descriptor, error)
	Open(context.Context, DeviceID, ...OpenOption) (GameController, error)
}

// DeviceEventKind identifies a hotplug discovery change.
type DeviceEventKind string

const (
	// DeviceAdded reports a newly discovered controller.
	DeviceAdded DeviceEventKind = "added"
	// DeviceRemoved reports a controller no longer present.
	DeviceRemoved DeviceEventKind = "removed"
	// DeviceUpdated reports changed metadata or capabilities.
	DeviceUpdated DeviceEventKind = "updated"
	// DeviceError reports a failed discovery poll.
	DeviceError DeviceEventKind = "error"
)

// DeviceEvent is one hotplug discovery update.
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
