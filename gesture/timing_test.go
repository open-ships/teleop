package gesture_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/gesture"
)

func monotonicButton(sequence uint64, wall time.Time, elapsed time.Duration, pressed bool) teleop.ButtonEvent {
	event := controllerButtonEvent(1, sequence, wall, teleop.ButtonFaceSouth, pressed)
	event.Meta.ReceivedAt = wall
	event.Meta.Monotonic, event.Meta.ReceivedMonotonic = elapsed, elapsed
	return event
}

func TestGestureDurationsSurviveWallClockStepsAndJSON(t *testing.T) {
	for _, step := range []time.Duration{-time.Hour, time.Hour} {
		for _, decoded := range []bool{false, true} {
			r := gesture.New(gesture.Config{TapMaximum: 200 * time.Millisecond, HoldMinimum: time.Second})
			start := time.Unix(100, 0)
			// Include a press exactly at monotonic origin (both fields zero).
			events := []teleop.ButtonEvent{
				monotonicButton(1, start, 0, true),
				monotonicButton(2, start.Add(step+100*time.Millisecond), 100*time.Millisecond, false),
				monotonicButton(3, start.Add(step+200*time.Millisecond), 200*time.Millisecond, true),
				monotonicButton(4, start.Add(2*step+300*time.Millisecond), 300*time.Millisecond, false),
			}
			if decoded {
				data, err := json.Marshal(events)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &events); err != nil {
					t.Fatal(err)
				}
			}
			var got []gesture.Event
			for _, event := range events {
				got = append(got, r.Recognize(event)...)
			}
			if len(got) != 2 || got[0].Type != gesture.Tap || got[0].Duration != 100*time.Millisecond || got[1].Type != gesture.DoubleTap || got[1].Duration != 200*time.Millisecond {
				t.Fatalf("step=%s decoded=%t: %+v", step, decoded, got)
			}
			if got[1].Meta.Monotonic != 300*time.Millisecond {
				t.Fatal("derived event lost monotonic metadata")
			}
		}
	}
}

func TestMonotonicHoldAdvancementAndDisconnect(t *testing.T) {
	r := gesture.New(gesture.DefaultConfig())
	wall := time.Unix(100, 0)
	press := monotonicButton(1, wall, time.Second, true)
	r.Recognize(press)
	other := press
	other.Meta.ID.Session = teleop.SessionID{2}
	r.Recognize(other)
	if got := r.AdvanceGestures(wall.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("wall time advanced monotonic hold: %+v", got)
	}
	if got := r.AdvanceMonotonic(press.Meta.ID.Session, -1); len(got) != 0 {
		t.Fatal("negative time advanced hold")
	}
	if got := r.AdvanceMonotonic(press.Meta.ID.Session, 1599*time.Millisecond); len(got) != 0 {
		t.Fatal("hold started early")
	}
	got := r.AdvanceMonotonic(press.Meta.ID.Session, 1600*time.Millisecond)
	if len(got) != 1 || got[0].Type != gesture.Hold || got[0].Phase != teleop.PhaseStarted || got[0].Meta.ID.Session != press.Meta.ID.Session {
		t.Fatalf("hold start: %+v", got)
	}
	if got[0].Meta.ObservedAt != wall.Add(600*time.Millisecond) {
		t.Fatal("calculation epoch leaked into wall timestamp")
	}
	if got := r.AdvanceMonotonic(press.Meta.ID.Session, 2*time.Second); len(got) != 0 {
		t.Fatal("hold repeated")
	}
	header := monotonicButton(2, wall.Add(-time.Hour), 2*time.Second, false).Meta
	ended := r.Recognize(teleop.ConnectionEvent{Meta: header, State: teleop.Disconnected})
	if len(ended) != 1 || ended[0].Duration != time.Second || ended[0].Phase != teleop.PhaseEnded {
		t.Fatalf("disconnect duration: %+v", ended)
	}
	if got := r.AdvanceMonotonic(other.Meta.ID.Session, 1600*time.Millisecond); len(got) != 1 {
		t.Fatalf("other session was advanced or lost: %+v", got)
	}
}

func TestStandaloneMonotonicOriginAndPublicationOnlyTiming(t *testing.T) {
	r := gesture.New(gesture.DefaultConfig())
	wall := time.Unix(100, 0)
	press := monotonicButton(1, wall, 0, true)
	r.Recognize(press)
	got := r.AdvanceMonotonic(press.Meta.ID.Session, 600*time.Millisecond)
	if len(got) != 1 || got[0].Type != gesture.Hold {
		t.Fatalf("explicit origin advancement: %+v", got)
	}
	r = gesture.New(gesture.DefaultConfig())
	press = monotonicButton(1, wall, time.Second, true)
	release := monotonicButton(2, wall.Add(time.Hour), 1100*time.Millisecond, false)
	press.Meta.ReceivedAt, release.Meta.ReceivedAt = time.Time{}, time.Time{}
	press.Meta.ReceivedMonotonic, release.Meta.ReceivedMonotonic = 0, 0
	r.Recognize(press)
	got = r.Recognize(release)
	if len(got) != 1 || got[0].Type != gesture.Tap || got[0].Duration != 100*time.Millisecond {
		t.Fatalf("publication-only timing: %+v", got)
	}
}

func TestMonotonicReleaseHoldAndDoubleTapExpiry(t *testing.T) {
	for _, step := range []time.Duration{-time.Hour, time.Hour} {
		wall := time.Unix(100, 0)
		r := gesture.New(gesture.DefaultConfig())
		r.Recognize(monotonicButton(1, wall, time.Second, true))
		got := r.Recognize(monotonicButton(2, wall.Add(step), 2*time.Second, false))
		if len(got) != 2 || got[0].Type != gesture.Hold || got[1].Type != gesture.Hold || got[1].Duration != time.Second {
			t.Fatalf("hold release across %s: %+v", step, got)
		}
		if got[0].Meta.Monotonic != 2*time.Second || got[1].Meta.Monotonic != 2*time.Second {
			t.Fatal("release-derived hold publication predates its recognition")
		}
		for _, interval := range []time.Duration{300 * time.Millisecond, 301 * time.Millisecond} {
			r = gesture.New(gesture.Config{DoubleTapWindow: 300 * time.Millisecond})
			r.Recognize(monotonicButton(1, wall, time.Second, true))
			r.Recognize(monotonicButton(2, wall.Add(time.Millisecond), 1100*time.Millisecond, false))
			r.Recognize(monotonicButton(3, wall.Add(step), time.Second+interval, true))
			got = r.Recognize(monotonicButton(4, wall.Add(step+time.Millisecond), 1100*time.Millisecond+interval, false))
			want := gesture.DoubleTap
			if interval > 300*time.Millisecond {
				want = gesture.Tap
			}
			if len(got) != 1 || got[0].Type != want {
				t.Fatalf("double-tap expiry across %s, interval %s: %+v", step, interval, got)
			}
		}
	}
}

type gestureMonotonicContext struct {
	session  teleop.SessionID
	elapsed  time.Duration
	wall     time.Time
	sequence uint64
}

func (clock *gestureMonotonicContext) Session() teleop.SessionID { return clock.session }
func (clock *gestureMonotonicContext) Monotonic() time.Duration  { return clock.elapsed }
func (clock *gestureMonotonicContext) Now() time.Time            { return clock.wall }
func (clock *gestureMonotonicContext) NewHeader(stream string, observed time.Time, deviceTimestamp int64, causes ...teleop.EventID) teleop.Header {
	clock.sequence++
	return teleop.Header{ID: teleop.EventID{Session: clock.session, Stream: stream, Sequence: clock.sequence}, ObservedAt: observed, Monotonic: clock.elapsed, Causes: causes}
}

func TestContextHoldUsesControllerMonotonicClock(t *testing.T) {
	r := gesture.New(gesture.DefaultConfig())
	clock := &gestureMonotonicContext{session: teleop.SessionID{1}, wall: time.Unix(100, 0)}
	press := monotonicButton(1, clock.wall, 0, true)
	if _, err := r.ProcessContext(t.Context(), clock, press); err != nil {
		t.Fatal(err)
	}
	clock.elapsed, clock.wall = 100*time.Millisecond, clock.wall.Add(time.Hour)
	if got, err := r.AdvanceContext(t.Context(), clock, clock.wall); err != nil || len(got) != 0 {
		t.Fatalf("wall step started hold: %+v %v", got, err)
	}
	clock.elapsed = 600 * time.Millisecond
	got, err := r.AdvanceContext(t.Context(), clock, clock.wall)
	if err != nil || len(got) != 1 || got[0].(gesture.Event).Type != gesture.Hold {
		t.Fatalf("monotonic hold: %+v %v", got, err)
	}
}

func TestTapMaximumAndDurationGap(t *testing.T) {
	for _, duration := range []time.Duration{99 * time.Millisecond, 100 * time.Millisecond, 101 * time.Millisecond, 500 * time.Millisecond, time.Second} {
		r := gesture.New(gesture.Config{TapMaximum: 100 * time.Millisecond, HoldMinimum: time.Second})
		start := time.Unix(100, 0)
		r.Recognize(buttonEvent(1, start, true))
		got := r.Recognize(buttonEvent(2, start.Add(duration), false))
		switch {
		case duration <= 100*time.Millisecond:
			if len(got) != 1 || got[0].Type != gesture.Tap {
				t.Fatalf("tap at %s: %+v", duration, got)
			}
		case duration < time.Second:
			if len(got) != 0 {
				t.Fatalf("duration gap at %s: %+v", duration, got)
			}
		default:
			if len(got) != 2 || got[0].Type != gesture.Hold {
				t.Fatalf("hold boundary: %+v", got)
			}
		}
	}
	// An overlong press interrupts a double-tap sequence even while the
	// overall double-tap window would allow pairing the surrounding taps.
	r := gesture.New(gesture.Config{TapMaximum: 100 * time.Millisecond, HoldMinimum: time.Second, DoubleTapWindow: 2 * time.Second})
	start := time.Unix(100, 0)
	for i, stamp := range []time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond, 600 * time.Millisecond, 650 * time.Millisecond, 700 * time.Millisecond} {
		got := r.Recognize(buttonEvent(uint64(i+1), start.Add(stamp), i%2 == 0))
		if i == 5 && (len(got) != 1 || got[0].Type != gesture.Tap) {
			t.Fatalf("overlong press failed to clear prior tap: %+v", got)
		}
	}
}

func TestGestureRejectsNonFiniteConfiguration(t *testing.T) {
	fields := map[string]func(*gesture.Config, float32){
		"stick threshold":    func(c *gesture.Config, v float32) { c.StickThreshold = v },
		"stick hysteresis":   func(c *gesture.Config, v float32) { c.StickHysteresis = v },
		"trigger threshold":  func(c *gesture.Config, v float32) { c.TriggerThreshold = v },
		"trigger hysteresis": func(c *gesture.Config, v float32) { c.TriggerHysteresis = v },
	}
	for name, set := range fields {
		for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			t.Run(name, func(t *testing.T) {
				config := gesture.DefaultConfig()
				set(&config, float32(value))
				defer func() {
					if recover() == nil {
						t.Fatal("accepted non-finite configuration")
					}
				}()
				gesture.New(config)
			})
		}
	}
}
