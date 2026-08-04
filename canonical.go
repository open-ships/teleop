package teleop

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// CanonicalEvent is an immutable event snapshot captured before asynchronous
// sink handoff. Its JSON bytes, identity, and kind cannot be changed by the
// event producer after admission.
//
// CanonicalEvent is primarily the value passed to a CanonicalEventSink. The
// zero value is invalid.
type CanonicalEvent struct {
	header  Header
	kind    EventKind
	payload string
}

// Header implements Event.
func (e CanonicalEvent) Header() Header { return e.header.Clone() }

// Kind implements Event.
func (e CanonicalEvent) Kind() EventKind { return e.kind }

// CloneEvent implements EventCloner. CanonicalEvent stores its payload as an
// immutable string and Header returns an isolated copy, so the value itself is
// safe to copy.
func (e CanonicalEvent) CloneEvent() Event { return e }

// JSON returns an isolated copy of the exact admitted event encoding.
func (e CanonicalEvent) JSON() []byte { return []byte(e.payload) }

// MarshalJSON preserves the exact admitted encoding rather than observing the
// source event again.
func (e CanonicalEvent) MarshalJSON() ([]byte, error) {
	if e.payload == "" {
		return nil, fmt.Errorf("%w: empty canonical event", ErrInvalidState)
	}
	return []byte(e.payload), nil
}

// CanonicalEventSink opts an authoritative sink into immutable admission.
// Controller serializes and validates each event before placing it in this
// sink's asynchronous queue, then calls RecordCanonical with those captured
// bytes. EventSink remains embedded for backward compatibility and for callers
// that record outside a Controller.
type CanonicalEventSink interface {
	EventSink
	RecordCanonical(context.Context, CanonicalEvent) error
}

// FreezeEvent captures event as immutable canonical JSON. It calls the Event
// interface only during this operation; later mutation of a third-party event
// cannot change the returned value.
func FreezeEvent(event Event) (frozen CanonicalEvent, err error) {
	if event == nil {
		return CanonicalEvent{}, fmt.Errorf("%w: nil event", ErrInvalidState)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			frozen = CanonicalEvent{}
			err = fmt.Errorf("%w: freeze event: %v", ErrCallbackPanic, recovered)
		}
	}()

	if canonical, ok := event.(CanonicalEvent); ok {
		if canonical.payload == "" {
			return CanonicalEvent{}, fmt.Errorf("%w: empty canonical event", ErrInvalidState)
		}
		return canonical, nil
	}
	if canonical, ok := event.(*CanonicalEvent); ok {
		if canonical == nil || canonical.payload == "" {
			return CanonicalEvent{}, fmt.Errorf("%w: empty canonical event", ErrInvalidState)
		}
		return *canonical, nil
	}

	header := event.Header()
	kind := event.Kind()
	if kind == "" {
		return CanonicalEvent{}, fmt.Errorf("%w: event kind is empty", ErrInvalidState)
	}
	payload, marshalErr := json.Marshal(event)
	if marshalErr != nil {
		return CanonicalEvent{}, fmt.Errorf("marshal event %q: %w", kind, marshalErr)
	}
	if !json.Valid(payload) {
		return CanonicalEvent{}, fmt.Errorf("%w: event %q produced invalid JSON", ErrInvalidState, kind)
	}
	var envelope struct {
		Header *Header `json:"header"`
	}
	if unmarshalErr := json.Unmarshal(payload, &envelope); unmarshalErr != nil {
		return CanonicalEvent{}, fmt.Errorf("decode event %q header: %w", kind, unmarshalErr)
	}
	if envelope.Header == nil {
		return CanonicalEvent{}, fmt.Errorf("%w: event %q JSON has no header", ErrInvalidState, kind)
	}
	if !equalHeaders(header, *envelope.Header) {
		return CanonicalEvent{}, fmt.Errorf(
			"%w: event %q JSON header differs from Event.Header",
			ErrInvalidState,
			kind,
		)
	}

	return CanonicalEvent{
		header:  header.Clone(),
		kind:    kind,
		payload: string(payload),
	}, nil
}

func equalHeaders(left, right Header) bool {
	return left.ID == right.ID &&
		left.DeviceID == right.DeviceID &&
		left.ObservedAt.Equal(right.ObservedAt) &&
		left.ReceivedAt.Equal(right.ReceivedAt) &&
		left.PublishedAt.Equal(right.PublishedAt) &&
		left.Monotonic == right.Monotonic &&
		left.ReceivedMonotonic == right.ReceivedMonotonic &&
		left.DeviceTimestamp == right.DeviceTimestamp &&
		slices.Equal(left.Causes, right.Causes) &&
		left.Synthetic == right.Synthetic
}
