package teleop

import (
	"testing"
	"time"
)

func TestClockMonitorDetectsWallClockDivergence(t *testing.T) {
	monitor := newClockMonitor(time.Now(), 100*time.Millisecond)
	monitor.observe(time.Now())

	// Simulate a wall-clock correction after the first sample. The saved
	// monotonic reading is deliberately left unchanged, as it would be by an
	// NTP step or a manual wall-clock adjustment.
	monitor.lastWall = monitor.lastWall.Add(-time.Second)
	sample := monitor.observe(time.Now())
	if !sample.Stepped || sample.Step < 900*time.Millisecond || monitor.stepCount() != 1 {
		t.Fatalf("clock sample = %#v, steps = %d", sample, monitor.stepCount())
	}
}

// TestClockMonitorDetectsBackwardStep covers the direction that corrupts
// forensic ordering: a wall clock moved backwards mid-session can make a
// release appear to precede the press that caused it.
func TestClockMonitorDetectsBackwardStep(t *testing.T) {
	monitor := newClockMonitor(time.Now(), 100*time.Millisecond)
	monitor.observe(time.Now())

	monitor.lastWall = monitor.lastWall.Add(time.Second)
	sample := monitor.observe(time.Now())
	if !sample.Stepped {
		t.Fatalf("a backward step must be detected: %#v", sample)
	}
	if sample.Step > -900*time.Millisecond {
		t.Fatalf("step = %s, want a large negative adjustment", sample.Step)
	}
}

func TestClockMonitorReportsNoStepUnderUniformAdvance(t *testing.T) {
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	monitor := newClockMonitor(base, 250*time.Millisecond)

	// A clock with no monotonic reading, such as a deterministic test clock,
	// must degrade safely: both deltas come from the same base, so no step is
	// ever reported.
	for tick := 1; tick <= 20; tick++ {
		sample := monitor.observe(base.Add(time.Duration(tick) * time.Hour))
		if sample.Stepped {
			t.Fatalf("tick %d reported a step under uniform advance", tick)
		}
	}
	if monitor.stepCount() != 0 {
		t.Fatalf("step count = %d, want 0", monitor.stepCount())
	}
}

func TestClockMonitorFirstObservationNeverSteps(t *testing.T) {
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	monitor := newClockMonitor(base, time.Millisecond)

	if sample := monitor.observe(base.Add(time.Hour)); sample.Stepped {
		t.Fatal("the first observation has no predecessor to compare against")
	}
}

// TestClockMonitorSinceIsSessionRelative checks the reading that every event
// header carries.
func TestClockMonitorSinceIsSessionRelative(t *testing.T) {
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	monitor := newClockMonitor(base, 0)

	if got := monitor.since(base); got != 0 {
		t.Fatalf("since at origin = %s, want 0", got)
	}
	if got := monitor.since(base.Add(1500 * time.Millisecond)); got != 1500*time.Millisecond {
		t.Fatalf("since = %s, want 1.5s", got)
	}
}

func TestClockMonitorDefaultsThreshold(t *testing.T) {
	for _, threshold := range []time.Duration{0, -time.Second} {
		monitor := newClockMonitor(time.Now(), threshold)
		if monitor.threshold != DefaultClockStepThreshold {
			t.Fatalf(
				"threshold %s produced %s, want %s",
				threshold,
				monitor.threshold,
				DefaultClockStepThreshold,
			)
		}
	}
}
