package teleop

import (
	"context"
	"errors"
	"time"
)

var (
	ErrClosed               = errors.New("teleop: controller closed")
	ErrDisconnected         = errors.New("teleop: controller disconnected")
	ErrUnsupported          = errors.New("teleop: unsupported")
	ErrPermission           = errors.New("teleop: permission denied")
	ErrUnavailable          = errors.New("teleop: unavailable")
	ErrSubscriptionOverflow = errors.New("teleop: subscription overflow")
)

type SourceGap struct {
	Dropped uint64
	Reason  string
}

// Observation is one complete state reading received from an OS backend.
// Native contains the closest practical representation of the source reading.
type Observation struct {
	State           State
	ObservedAt      time.Time
	DeviceTimestamp int64
	Native          NativeInput
	Gap             *SourceGap
}

// InputSource is the small boundary implemented by platform drivers and fakes.
type InputSource interface {
	Descriptor() Descriptor
	Read(context.Context) (Observation, error)
	Close() error
}

type EventSink interface {
	Record(context.Context, Event) error
}

// Processor derives events from events earlier in a controller pipeline.
// Processors are applied in option order; a later processor sees canonical
// events and output from all earlier processors.
type Processor interface {
	Process(Event) []Event
}

// AdvancingProcessor emits time-based events even when the controller is
// otherwise idle. Controller invokes it on a short internal ticker.
type AdvancingProcessor interface {
	Processor
	Advance(time.Time) []Event
}

type controllerOptions struct {
	sinks      []EventSink
	processors []Processor
}

type OpenOption func(*controllerOptions)

// WithAuditSink attaches an authoritative ingress sink. The controller records
// each event to all sinks before publishing it to subscriptions.
func WithAuditSink(sink EventSink) OpenOption {
	return func(options *controllerOptions) {
		if sink != nil {
			options.sinks = append(options.sinks, sink)
		}
	}
}

// WithProcessor adds an ordered derived-event stage. A typical pipeline adds a
// gesture recognizer followed by an action mapper.
func WithProcessor(processor Processor) OpenOption {
	return func(options *controllerOptions) {
		if processor != nil {
			options.processors = append(options.processors, processor)
		}
	}
}
