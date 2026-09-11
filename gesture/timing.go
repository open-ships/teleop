package gesture

import (
	"time"

	"github.com/open-ships/teleop"
)

// Timer arithmetic uses an artificial epoch for monotonic sessions. It never
// escapes into wall-clock event metadata. Keeping both bases as time.Time
// lets the same press/tap state machine serve legacy wall-only callers.
var monotonicEpoch = time.Unix(0, 0).UTC()

func headerMonotonic(header teleop.Header) time.Duration {
	if header.ReceivedMonotonic != 0 || !header.ReceivedAt.IsZero() {
		return header.ReceivedMonotonic
	}
	// Standalone event producers may supply publication timing alone.
	return header.Monotonic
}

func (state *streamState) eventTime(header teleop.Header, processing teleop.ProcessingContext) time.Time {
	_, contextual := processing.(teleop.MonotonicProcessingContext)
	if !state.monotonic && (contextual || header.Monotonic != 0 || header.ReceivedMonotonic != 0) {
		state.useMonotonic()
	}
	if state.monotonic {
		return monotonicEpoch.Add(headerMonotonic(header))
	}
	return header.ObservedAt
}

func (state *streamState) useMonotonic() {
	if state.monotonic {
		return
	}
	state.monotonic = true
	// The first event can legitimately arrive exactly at session origin,
	// where both monotonic fields are zero. If later metadata or explicit
	// advancement establishes this basis, preserve that initial press/tap.
	for control, value := range state.pressed {
		value.at = monotonicEpoch.Add(headerMonotonic(value.header))
		state.pressed[control] = value
	}
	for control, value := range state.lastTap {
		value.at = monotonicEpoch.Add(value.monotonic)
		state.lastTap[control] = value
	}
}
