package audit

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-ships/teleop"
)

// Descriptor returns the first controller descriptor recorded in a connection
// event.
func Descriptor(records []Record) (teleop.Descriptor, bool, error) {
	for _, record := range records {
		if record.Kind != teleop.EventConnection {
			continue
		}
		var event teleop.ConnectionEvent
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			return teleop.Descriptor{}, false, fmt.Errorf("decode connection event: %w", err)
		}
		if event.Descriptor.ID != "" {
			return event.Descriptor.Clone(), true, nil
		}
	}
	return teleop.Descriptor{}, false, nil
}

// Observations extracts replayable source observations from an audit log.
// Known gaps are attached to the next observation.
func Observations(records []Record) ([]teleop.Observation, error) {
	var (
		result     []teleop.Observation
		pendingGap *teleop.SourceGap
	)
	for _, record := range records {
		switch record.Kind {
		case teleop.EventGap:
			var event teleop.GapEvent
			if err := json.Unmarshal(record.Payload, &event); err != nil {
				return nil, fmt.Errorf("decode gap event: %w", err)
			}
			if pendingGap == nil {
				pendingGap = &teleop.SourceGap{}
			}
			pendingGap.Dropped += event.Dropped
			if pendingGap.Reason == "" {
				pendingGap.Reason = event.Reason
			} else {
				pendingGap.Reason = strings.Join([]string{pendingGap.Reason, event.Reason}, "; ")
			}
		case teleop.EventObservation:
			var event teleop.ObservationEvent
			if err := json.Unmarshal(record.Payload, &event); err != nil {
				return nil, fmt.Errorf("decode observation event: %w", err)
			}
			result = append(result, teleop.Observation{
				State:           event.Current.Clone(),
				ObservedAt:      event.Meta.ObservedAt,
				DeviceTimestamp: event.Meta.DeviceTimestamp,
				Native:          event.Native,
				Gap:             pendingGap,
			})
			pendingGap = nil
		}
	}
	return result, nil
}
