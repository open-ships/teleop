package teleop

import "time"

// DefaultClockStepThreshold is the wall-versus-monotonic divergence a
// controller treats as a clock step rather than ordinary drift. NTP slewing
// stays well below it; a step adjustment or manual clock change exceeds it.
const DefaultClockStepThreshold = 250 * time.Millisecond

// clockSample is one reading of both time bases taken by the controller run
// loop. Monotonic is relative to the controller session origin, so it is
// meaningful only within a single session.
type clockSample struct {
	Wall      time.Time
	Monotonic time.Duration
	Step      time.Duration
	Stepped   bool
}

// clockMonitor detects wall-clock steps by comparing wall-clock movement
// against monotonic movement between consecutive readings.
//
// A Clock implementation whose times carry no monotonic reading, such as a
// deterministic test clock, degrades safely: both deltas are then computed
// from the same base and no step is ever reported.
type clockMonitor struct {
	base      time.Time
	lastWall  time.Time
	lastMono  time.Duration
	threshold time.Duration
	steps     uint64
	started   bool
}

func newClockMonitor(base time.Time, threshold time.Duration) *clockMonitor {
	if threshold <= 0 {
		threshold = DefaultClockStepThreshold
	}
	return &clockMonitor{base: base, threshold: threshold}
}

// since returns the session-relative monotonic reading for now. Go subtracts
// monotonic readings whenever both operands carry one, so the result is
// unaffected by wall-clock adjustments. It reads only immutable state and is
// safe to call from any goroutine.
func (m *clockMonitor) since(now time.Time) time.Duration {
	return now.Sub(m.base)
}

// observe records a reading and reports whether the wall clock stepped
// relative to the monotonic clock since the previous reading. It mutates
// monitor state and must be called only from the controller run loop.
func (m *clockMonitor) observe(now time.Time) clockSample {
	monotonic := m.since(now)
	sample := clockSample{Wall: now, Monotonic: monotonic}
	if !m.started {
		m.started = true
		m.lastWall = now
		m.lastMono = monotonic
		return sample
	}

	// Round(0) strips the monotonic reading, so this difference reflects the
	// wall clock alone. The unrounded difference uses the monotonic reading.
	wallDelta := now.Round(0).Sub(m.lastWall.Round(0))
	monoDelta := monotonic - m.lastMono
	divergence := wallDelta - monoDelta
	magnitude := divergence
	if magnitude < 0 {
		magnitude = -magnitude
	}
	if magnitude >= m.threshold {
		m.steps++
		sample.Step = divergence
		sample.Stepped = true
	}

	m.lastWall = now
	m.lastMono = monotonic
	return sample
}

func (m *clockMonitor) stepCount() uint64 { return m.steps }
