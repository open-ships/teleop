package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/action"
	"github.com/open-ships/teleop/gesture"
)

// OpaqueEvent retains an unknown third-party kind without pretending it was
// decoded.
type OpaqueEvent struct {
	Meta    teleop.Header
	Type    teleop.EventKind
	Payload json.RawMessage
}

func (event OpaqueEvent) Header() teleop.Header  { return event.Meta.Clone() }
func (event OpaqueEvent) Kind() teleop.EventKind { return event.Type }
func (event OpaqueEvent) CloneEvent() teleop.Event {
	event.Meta = event.Meta.Clone()
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	return event
}

// EncodingErrorEvent preserves the identity and original kind of an event
// whose concrete payload could not be represented as JSON. It prevents replay
// from silently manufacturing a zero-valued event of the original type.
type EncodingErrorEvent struct {
	Meta         teleop.Header
	OriginalKind teleop.EventKind
	Message      string
}

func (event EncodingErrorEvent) Header() teleop.Header  { return event.Meta.Clone() }
func (event EncodingErrorEvent) Kind() teleop.EventKind { return event.OriginalKind }
func (event EncodingErrorEvent) CloneEvent() teleop.Event {
	event.Meta = event.Meta.Clone()
	return event
}

// DecodeEvent reconstructs every event kind shipped by this module. Unknown
// kinds are returned as OpaqueEvent.
func DecodeEvent(record Record) (teleop.Event, error) {
	if record.EncodingError != "" {
		return EncodingErrorEvent{
			Meta:         record.Header.Clone(),
			OriginalKind: record.Kind,
			Message:      record.EncodingError,
		}, nil
	}
	var target teleop.Event
	switch record.Kind {
	case teleop.EventObservation:
		target = &teleop.ObservationEvent{}
	case teleop.EventButton:
		target = &teleop.ButtonEvent{}
	case teleop.EventStick:
		target = &teleop.StickEvent{}
	case teleop.EventTrigger:
		target = &teleop.TriggerEvent{}
	case teleop.EventConnection:
		target = &teleop.ConnectionEvent{}
	case teleop.EventCapabilities:
		target = &teleop.CapabilitiesEvent{}
	case teleop.EventLiveness:
		target = &teleop.LivenessEvent{}
	case teleop.EventGap:
		target = &teleop.GapEvent{}
	case teleop.EventError:
		target = &teleop.ErrorEvent{}
	case teleop.EventClock:
		target = &teleop.ClockEvent{}
	case teleop.EventCommand:
		target = &teleop.CommandEvent{}
	case gesture.EventKind:
		target = &gesture.Event{}
	case action.EventKind:
		target = &action.Event{}
	default:
		return OpaqueEvent{
			Meta:    record.Header.Clone(),
			Type:    record.Kind,
			Payload: append(json.RawMessage(nil), record.Payload...),
		}, nil
	}
	if err := json.Unmarshal(record.Payload, target); err != nil {
		return nil, fmt.Errorf("decode %s event: %w", record.Kind, err)
	}
	return target, nil
}

// Events decodes all verified records in order.
func Events(records []Record) ([]teleop.Event, error) {
	result := make([]teleop.Event, 0, len(records))
	for _, record := range records {
		event, err := DecodeEvent(record)
		if err != nil {
			return result, err
		}
		result = append(result, event)
	}
	return result, nil
}

// Descriptor returns the first controller descriptor recorded in a connection
// event.
func Descriptor(records []Record) (teleop.Descriptor, bool, error) {
	for _, record := range records {
		if record.Kind != teleop.EventConnection {
			continue
		}
		event, err := DecodeEvent(record)
		if err != nil {
			return teleop.Descriptor{}, false, err
		}
		connection := event.(*teleop.ConnectionEvent)
		if connection.Descriptor.ID != "" {
			return connection.Descriptor.Clone(), true, nil
		}
	}
	return teleop.Descriptor{}, false, nil
}

// ReplayFaultError reports that observations were recovered from a session
// which ended with loss or an unexpected disconnect.
type ReplayFaultError struct {
	Reason string
}

func (err *ReplayFaultError) Error() string { return "audit replay contains a fault: " + err.Reason }

// Observations extracts replayable source observations. It returns the intact
// prefix together with ReplayFaultError when a gap has no following
// observation or a session ended unexpectedly.
func Observations(records []Record) ([]teleop.Observation, error) {
	var (
		result     []teleop.Observation
		pendingGap *teleop.SourceGap
		faults     []string
	)
	for _, record := range records {
		switch record.Kind {
		case teleop.EventGap:
			var event teleop.GapEvent
			if err := json.Unmarshal(record.Payload, &event); err != nil {
				return result, fmt.Errorf("decode gap event: %w", err)
			}
			if event.Source == "subscription" {
				faults = append(faults, event.Reason)
				continue
			}
			if pendingGap == nil {
				pendingGap = &teleop.SourceGap{}
			}
			pendingGap.Dropped += event.Dropped
			if pendingGap.Reason == "" {
				pendingGap.Reason = event.Reason
			} else {
				pendingGap.Reason += "; " + event.Reason
			}
		case teleop.EventObservation:
			var event teleop.ObservationEvent
			if err := json.Unmarshal(record.Payload, &event); err != nil {
				return result, fmt.Errorf("decode observation event: %w", err)
			}
			result = append(result, teleop.Observation{
				State:           event.Current.Clone(),
				ObservedAt:      event.Meta.ObservedAt,
				DeviceTimestamp: event.Meta.DeviceTimestamp,
				Native:          event.Native,
				Gap:             pendingGap,
			})
			pendingGap = nil
		case teleop.EventError:
			var event teleop.ErrorEvent
			if err := json.Unmarshal(record.Payload, &event); err == nil {
				faults = append(faults, event.Message)
			}
		case teleop.EventConnection:
			var event teleop.ConnectionEvent
			if err := json.Unmarshal(record.Payload, &event); err == nil &&
				event.State == teleop.Disconnected &&
				event.Reason != teleop.ErrClosed.Error() &&
				!errors.Is(classifyReason(event.Reason), teleop.ErrClosed) {
				faults = append(faults, event.Reason)
			}
		}
	}
	if pendingGap != nil {
		faults = append(faults, "trailing input gap: "+pendingGap.Reason)
	}
	if len(faults) > 0 {
		return result, &ReplayFaultError{Reason: strings.Join(faults, "; ")}
	}
	return result, nil
}

func classifyReason(reason string) error {
	if strings.Contains(reason, teleop.ErrClosed.Error()) {
		return teleop.ErrClosed
	}
	return errors.New(reason)
}
