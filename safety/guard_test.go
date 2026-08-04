package safety

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/open-ships/teleop"
)

// fakeSource drives a Guard from an explicit monotonic clock, so timeout
// behavior is tested deterministically rather than by sleeping.
type fakeSource struct {
	mu    sync.Mutex
	state teleop.State
	meta  teleop.StateMeta
	mono  time.Duration
	done  chan struct{}
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		meta: teleop.StateMeta{Connected: true, Sequence: 1},
		done: make(chan struct{}),
	}
}

func (f *fakeSource) SnapshotWithMeta() (teleop.State, teleop.StateMeta) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.meta
}

func (f *fakeSource) Monotonic() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mono
}

func (f *fakeSource) Done() <-chan struct{} { return f.done }

func (*fakeSource) Capabilities() teleop.Capabilities {
	return teleop.Capabilities{Controls: []teleop.ControlDescriptor{
		{ID: deadMan, Kind: teleop.ControlButton},
	}}
}

// advance moves the monotonic clock without delivering input, which is how a
// command timeout is provoked.
func (f *fakeSource) advance(delta time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mono += delta
}

// observe delivers a fresh observation at the current monotonic reading.
func (f *fakeSource) observe(state teleop.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
	f.meta.ReceivedMonotonic = f.mono
	f.meta.Sequence++
	f.meta.Connected = true
	f.meta.Synthetic = false
}

func (f *fakeSource) setMeta(mutate func(*teleop.StateMeta)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(&f.meta)
}

const deadMan = teleop.ButtonBumperRight

func heldState() teleop.State {
	var state teleop.State
	state.SetButton(deadMan, true)
	return state
}

func inputHeader(source *fakeSource) teleop.Header {
	monotonic := source.Monotonic()
	return teleop.Header{
		Monotonic:         monotonic,
		ReceivedMonotonic: monotonic,
	}
}

// liveGuard returns an armed, engaged Guard permitting output, which is the
// baseline each inhibiting condition is tested against.
func liveGuard(t *testing.T, opts ...Option) (*Guard, *fakeSource) {
	t.Helper()
	source := newFakeSource()
	guard := New(append([]Option{
		WithCommandTimeout(100 * time.Millisecond),
		WithDeadMan(deadMan),
	}, opts...)...)
	if err := guard.Bind(source); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := guard.Arm(); err != nil {
		t.Fatalf("arm released baseline: %v", err)
	}

	// The Guard must witness a post-arm press that begins the hold.
	source.advance(time.Nanosecond)
	source.observe(heldState())
	guard.Process(teleop.ButtonEvent{
		Meta:    inputHeader(source),
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})
	decision := guard.Evaluate()
	if !decision.Permit {
		t.Fatalf("baseline must permit, got %v", decision.Reasons)
	}
	return guard, source
}

func rearmAndEngage(t *testing.T, guard *Guard, source *fakeSource) {
	t.Helper()
	source.observe(teleop.State{})
	guard.Process(teleop.ButtonEvent{
		Meta:   inputHeader(source),
		Button: deadMan,
		Phase:  teleop.PhaseReleased,
	})
	guard.Heartbeat()
	if err := guard.Arm(); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	source.advance(time.Nanosecond)
	source.observe(heldState())
	guard.Process(teleop.ButtonEvent{
		Meta:    inputHeader(source),
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})
}

func TestUnboundGuardDeniesOutput(t *testing.T) {
	guard := New()
	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("an unbound guard must never permit output")
	}
	if !decision.Has(ReasonUnbound) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
	if guard.Arm() == nil {
		t.Fatal("arming an unbound guard must fail")
	}
}

func TestUnarmedGuardDeniesOutput(t *testing.T) {
	source := newFakeSource()
	guard := New(WithDeadMan(deadMan))
	guard.Bind(source)
	source.observe(heldState())

	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("an unarmed guard must not permit output")
	}
	if !decision.Has(ReasonNotArmed) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
}

func TestDeadManPressMustBeReceivedStrictlyAfterArm(t *testing.T) {
	source := newFakeSource()
	source.advance(10 * time.Millisecond)
	source.observe(teleop.State{})
	guard := New(
		WithCommandTimeout(time.Second),
		WithDeadMan(deadMan),
	)
	if err := guard.Bind(source); err != nil {
		t.Fatal(err)
	}
	queuedPress := teleop.ButtonEvent{
		Meta: teleop.Header{
			ReceivedMonotonic: source.Monotonic(),
		},
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	}
	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}

	// Publication was delayed until after Arm, but receipt was not. The queued
	// edge cannot establish post-Arm operator engagement.
	source.advance(10 * time.Millisecond)
	source.observe(heldState())
	queuedPress.Meta.Monotonic = source.Monotonic()
	guard.Process(queuedPress)
	decision := guard.Evaluate()
	if decision.Permit || !decision.Has(ReasonDeadManUnconfirmed) {
		t.Fatalf("pre-Arm received press granted authority: %+v", decision)
	}

	source.advance(time.Nanosecond)
	source.observe(teleop.State{})
	guard.Process(teleop.ButtonEvent{
		Meta:   inputHeader(source),
		Button: deadMan,
		Phase:  teleop.PhaseReleased,
	})
	source.advance(time.Nanosecond)
	source.observe(heldState())
	guard.Process(teleop.ButtonEvent{
		Meta:    inputHeader(source),
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})
	if decision := guard.Evaluate(); !decision.Permit {
		t.Fatalf("genuine post-Arm press was refused: %+v", decision)
	}
}

func TestDeadManRejectsMalformedReceivedMonotonic(t *testing.T) {
	tests := []struct {
		name     string
		received func(time.Duration) time.Duration
	}{
		{name: "negative", received: func(time.Duration) time.Duration { return -1 }},
		{name: "future", received: func(now time.Duration) time.Duration { return now + 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newFakeSource()
			source.advance(10 * time.Millisecond)
			source.observe(teleop.State{})
			guard := New(WithCommandTimeout(time.Second), WithDeadMan(deadMan))
			if err := guard.Bind(source); err != nil {
				t.Fatal(err)
			}
			if err := guard.Arm(); err != nil {
				t.Fatal(err)
			}
			source.advance(time.Nanosecond)
			source.observe(heldState())
			now := source.Monotonic()
			guard.Process(teleop.ButtonEvent{
				Meta: teleop.Header{
					Monotonic:         now,
					ReceivedMonotonic: test.received(now),
				},
				Button:  deadMan,
				Pressed: true,
				Phase:   teleop.PhasePressed,
			})
			decision := guard.Evaluate()
			if decision.Permit || !decision.Has(ReasonSourceError) {
				t.Fatalf("malformed received time decision = %+v", decision)
			}
		})
	}
}

// TestNeverObservedInputDeniesOutput covers a controller that opened but from
// which no observation ever arrived: silence that has never been broken is not
// evidence of a working input path.
func TestNeverObservedInputDeniesOutput(t *testing.T) {
	source := newFakeSource()
	source.setMeta(func(meta *teleop.StateMeta) { meta.Sequence = 0 })
	guard := New()
	if err := guard.Bind(source); err != nil {
		t.Fatal(err)
	}
	if err := guard.Arm(); !errors.Is(err, ErrUnsafeToArm) {
		t.Fatalf("arm error = %v, want ErrUnsafeToArm", err)
	}

	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("output must be denied before any observation arrives")
	}
	if !decision.Has(ReasonNoInput) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
}

func TestGuardRejectsFutureMonotonicEvidence(t *testing.T) {
	t.Run("observation", func(t *testing.T) {
		source := newFakeSource()
		source.setMeta(func(meta *teleop.StateMeta) {
			meta.ReceivedMonotonic = time.Second
		})
		guard := New()
		if err := guard.Bind(source); err != nil {
			t.Fatal(err)
		}
		if err := guard.Arm(); !errors.Is(err, ErrUnsafeToArm) {
			t.Fatalf("Arm error = %v", err)
		}
		if decision := guard.Evaluate(); !decision.Has(ReasonSourceError) {
			t.Fatalf("future observation decision = %+v", decision)
		}
	})

	t.Run("strict transport", func(t *testing.T) {
		config := DefaultMaritimeConfig(deadMan)
		guard, err := NewMaritime(config)
		if err != nil {
			t.Fatal(err)
		}
		source := newFakeSource()
		source.setMeta(func(meta *teleop.StateMeta) {
			meta.TransportSilenceVerifiable = true
			meta.TransportCheckSequence = 1
			meta.LastTransportCheckMonotonic = time.Second
		})
		if err := guard.Bind(source); err != nil {
			t.Fatal(err)
		}
		guard.Heartbeat()
		if err := guard.Arm(); !errors.Is(err, ErrUnsafeToArm) {
			t.Fatalf("Arm error = %v", err)
		}
		if decision := guard.Evaluate(); !decision.Has(ReasonTransportUnverifiable) {
			t.Fatalf("future transport decision = %+v", decision)
		}
	})
}

func TestCommandTimeoutInhibitsAndNeutralizesCommand(t *testing.T) {
	guard, source := liveGuard(t)

	source.advance(99 * time.Millisecond)
	if decision := guard.Evaluate(); !decision.Permit {
		t.Fatalf("input inside the timeout must stay authorized: %v", decision.Reasons)
	}

	source.advance(2 * time.Millisecond)
	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("input older than the command timeout must inhibit output")
	}
	if !decision.Has(ReasonCommandTimeout) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
	if decision.Command.Button(deadMan) || decision.Command.LeftStick != (teleop.Stick{}) {
		t.Fatal("an inhibited decision must carry the neutral command state")
	}
	if decision.State != StateSafe {
		t.Fatalf("state = %q, want %q", decision.State, StateSafe)
	}
}

// TestCommandTimeoutLatches is the property that keeps a transient radio
// dropout from becoming an unexpected movement when the link returns.
func TestCommandTimeoutLatches(t *testing.T) {
	guard, source := liveGuard(t)

	source.advance(200 * time.Millisecond)
	if guard.Evaluate().Permit {
		t.Fatal("timeout must inhibit")
	}

	source.observe(teleop.State{})
	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("fresh input must not silently restore authority after a trip")
	}
	if !decision.Has(ReasonNotArmed) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}

	rearmAndEngage(t, guard, source)
	if !guard.Evaluate().Permit {
		t.Fatal("an explicit re-arm must restore authority")
	}
}

func TestLatchingCanBeDisabled(t *testing.T) {
	guard, source := liveGuard(t, WithLatching(false))

	source.advance(200 * time.Millisecond)
	if guard.Evaluate().Permit {
		t.Fatal("timeout must inhibit")
	}
	source.observe(heldState())
	if !guard.Evaluate().Permit {
		t.Fatal("a non-latching guard must resume when the condition clears")
	}
}

// TestDeadManReleaseDoesNotLatch distinguishes normal operation from a fault:
// letting go is expected and must not require re-arming.
func TestDeadManReleaseDoesNotLatch(t *testing.T) {
	guard, source := liveGuard(t)

	guard.Process(teleop.ButtonEvent{
		Meta:   inputHeader(source),
		Button: deadMan,
		Phase:  teleop.PhaseReleased,
	})
	source.observe(teleop.State{})

	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("releasing the dead-man control must inhibit output")
	}
	if decision.State != StateArmed {
		t.Fatalf("state = %q, want %q", decision.State, StateArmed)
	}
	if !decision.Has(ReasonDeadManReleased) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}

	source.advance(10 * time.Millisecond)
	guard.Process(teleop.ButtonEvent{
		Meta:    inputHeader(source),
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})
	source.observe(heldState())
	if !guard.Evaluate().Permit {
		t.Fatal("re-engaging must restore authority without a re-arm")
	}
}

// TestDeadManReactuationDeadline detects a control held down indefinitely,
// which is what a taped or wedged switch looks like.
func TestDeadManReactuationDeadline(t *testing.T) {
	guard, source := liveGuard(t, WithDeadManReactuation(500*time.Millisecond))

	for elapsed := time.Duration(0); elapsed < 400*time.Millisecond; elapsed += 50 * time.Millisecond {
		source.advance(50 * time.Millisecond)
		source.observe(heldState())
		if !guard.Evaluate().Permit {
			t.Fatalf("a held control must stay authorized before the deadline at %s", elapsed)
		}
	}

	source.advance(150 * time.Millisecond)
	source.observe(heldState())
	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("a control held past the re-actuation deadline must inhibit output")
	}
	if !decision.Has(ReasonDeadManStale) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}

	// A fault clears engagement proof. Recovery requires release, re-arm, and a
	// new post-arm press in that order.
	rearmAndEngage(t, guard, source)
	if !guard.Evaluate().Permit {
		t.Fatal("release, re-arm, and re-actuation must restore authority")
	}
}

// TestDeadManHeldWithoutObservedPressIsStale covers a Guard bound while the
// control was already down: it cannot bound the hold, so it must not trust it.
func TestDeadManHeldWithoutObservedPressIsStale(t *testing.T) {
	source := newFakeSource()
	guard := New(WithDeadMan(deadMan))
	if err := guard.Bind(source); err != nil {
		t.Fatal(err)
	}
	source.observe(heldState())
	if err := guard.Arm(); !errors.Is(err, ErrUnsafeToArm) {
		t.Fatalf("arm error = %v, want ErrUnsafeToArm", err)
	}

	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("an unobserved hold must not authorize output")
	}
	if !decision.Has(ReasonDeadManReleaseRequired) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
	rearmAndEngage(t, guard, source)
	if !guard.Evaluate().Permit {
		t.Fatal("startup hold must require release, arm, then a new press")
	}
}

func TestLoopWatchdogDetectsStalledControlLoop(t *testing.T) {
	source := newFakeSource()
	guard := New(
		WithCommandTimeout(time.Second),
		WithLoopWatchdog(50*time.Millisecond),
	)
	guard.Bind(source)
	source.observe(teleop.State{})
	guard.Heartbeat()
	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}
	if !guard.Evaluate().Permit {
		t.Fatal("a heartbeat inside the watchdog interval must permit output")
	}

	source.advance(60 * time.Millisecond)
	source.observe(teleop.State{})
	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("a stalled control loop must inhibit output even with fresh input")
	}
	if !decision.Has(ReasonLoopStalled) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
}

func TestEmergencyStopLatchesUntilReset(t *testing.T) {
	guard, source := liveGuard(t)

	guard.EmergencyStop("obstacle in the work area")
	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("an emergency stop must inhibit output")
	}
	if !decision.Has(ReasonEmergencyStop) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}

	if guard.Arm() == nil {
		t.Fatal("arming during an emergency stop must fail")
	}

	guard.Reset("area cleared")
	if guard.Evaluate().Permit {
		t.Fatal("reset alone must not restore authority")
	}
	rearmAndEngage(t, guard, source)
	if !guard.Evaluate().Permit {
		t.Fatal("reset followed by arm must restore authority")
	}
}

func TestDisconnectAndSyntheticStateInhibitOutput(t *testing.T) {
	guard, source := liveGuard(t)

	source.setMeta(func(meta *teleop.StateMeta) { meta.Connected = false })
	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("a disconnected controller must inhibit output")
	}
	if !decision.Has(ReasonDisconnected) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}

	guard2, source2 := liveGuard(t)
	source2.setMeta(func(meta *teleop.StateMeta) { meta.Synthetic = true })
	decision = guard2.Evaluate()
	if decision.Permit {
		t.Fatal("synthesized state must inhibit output")
	}
	if !decision.Has(ReasonSynthetic) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
}

func TestControllerFaultInhibitsOutput(t *testing.T) {
	guard, source := liveGuard(t)

	close(source.done)
	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("a terminated controller must inhibit output")
	}
	if !decision.Has(ReasonControllerFault) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
}

// TestCommandTimeoutIgnoresWallClock is the reason the timeout is measured on
// the monotonic clock: a wall-clock step must not make stale input look fresh
// or fresh input look stale.
func TestCommandTimeoutIgnoresWallClock(t *testing.T) {
	guard, source := liveGuard(t)

	// A wall clock jumping an hour backwards leaves monotonic readings alone.
	source.setMeta(func(meta *teleop.StateMeta) {
		meta.ReceivedAt = time.Now().Add(-time.Hour)
	})
	if !guard.Evaluate().Permit {
		t.Fatal("a wall-clock step must not affect the command timeout")
	}
}

func TestDisarmInhibitsUntilRearmed(t *testing.T) {
	guard, source := liveGuard(t)

	guard.Disarm("shift handover")
	if guard.Evaluate().Permit {
		t.Fatal("a disarmed guard must inhibit output")
	}
	source.observe(heldState())
	if guard.Evaluate().Permit {
		t.Fatal("fresh input must not undo a disarm")
	}
	rearmAndEngage(t, guard, source)
	if !guard.Evaluate().Permit {
		t.Fatal("re-arming must restore authority")
	}
}

func TestConcurrentEvaluationIsRaceFree(t *testing.T) {
	guard, source := liveGuard(t)

	var waiting sync.WaitGroup
	for range 8 {
		waiting.Go(func() {
			for range 200 {
				guard.Heartbeat()
				guard.Evaluate()
				source.observe(heldState())
			}
		})
	}
	waiting.Wait()
}

func TestBindIsOneShot(t *testing.T) {
	guard := New()
	if err := guard.Bind(newFakeSource()); err != nil {
		t.Fatal(err)
	}
	if err := guard.Bind(newFakeSource()); !errors.Is(err, ErrAlreadyBound) {
		t.Fatalf("second Bind error = %v, want ErrAlreadyBound", err)
	}
}

func TestBindCannotClearPreexistingEmergencyStop(t *testing.T) {
	guard := New()
	guard.EmergencyStop("stop requested while controller opens")
	if err := guard.Bind(newFakeSource()); err != nil {
		t.Fatal(err)
	}
	if err := guard.Arm(); !errors.Is(err, errArmDuringStop) {
		t.Fatalf("Arm error = %v, want emergency-stop refusal", err)
	}
	decision := guard.Evaluate()
	if decision.Permit || !decision.Has(ReasonEmergencyStop) {
		t.Fatalf("post-bind emergency-stop decision = %+v", decision)
	}
}

func TestArmRequiresNeutralReleasedStateAndPostArmPress(t *testing.T) {
	source := newFakeSource()
	source.observe(teleop.State{LeftStick: teleop.Stick{X: 0.06}})
	guard := New(WithDeadMan(deadMan))
	if err := guard.Bind(source); err != nil {
		t.Fatal(err)
	}
	if err := guard.Arm(); !errors.Is(err, ErrUnsafeToArm) {
		t.Fatalf("non-neutral Arm error = %v, want ErrUnsafeToArm", err)
	} else {
		var armErr *ArmError
		if !errors.As(err, &armErr) || !slices.Contains(armErr.Reasons, ReasonControlsNotNeutral) {
			t.Fatalf("Arm reasons = %v, want controls_not_neutral", armErr)
		}
	}

	// Drift inside the arming tolerances is accepted, but live input is not
	// modified by those tolerances.
	source.observe(teleop.State{
		LeftStick:   teleop.Stick{X: 0.03, Y: 0.02},
		LeftTrigger: 0.01,
	})
	if err := guard.Arm(); err != nil {
		t.Fatalf("Arm inside neutral tolerance: %v", err)
	}
	if guard.Evaluate().State != StateArmed {
		t.Fatal("released dead-man must leave the guard armed, not live")
	}

	held := heldState()
	held.LeftStick = teleop.Stick{X: 0.4}
	source.advance(time.Nanosecond)
	source.observe(held)
	decision := guard.Evaluate()
	if decision.Permit || !decision.Has(ReasonDeadManUnconfirmed) {
		t.Fatalf("held state before post-arm edge = %+v", decision)
	}
	guard.Process(teleop.ButtonEvent{
		Meta:    inputHeader(source),
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})
	decision = guard.Evaluate()
	if !decision.Permit || decision.Command.LeftStick.X != 0.4 {
		t.Fatalf("post-arm engagement decision = %+v", decision)
	}
}

func TestArmRequiresEveryDigitalControlReleased(t *testing.T) {
	source := newFakeSource()
	var state teleop.State
	state.SetButton(teleop.ButtonFaceSouth, true)
	source.observe(state)
	guard := New(WithDeadMan(deadMan))
	if err := guard.Bind(source); err != nil {
		t.Fatal(err)
	}
	if err := guard.Arm(); !errors.Is(err, ErrUnsafeToArm) {
		t.Fatalf("Arm error = %v, want ErrUnsafeToArm", err)
	}
}

func TestResetAlwaysEndsIdle(t *testing.T) {
	guard, _ := liveGuard(t)
	guard.Reset("operator reset while armed")
	decision := guard.Evaluate()
	if decision.Permit || !decision.Has(ReasonNotArmed) || guard.lifecycle != lifecycleIdle {
		t.Fatalf("decision after Reset while armed = %+v, lifecycle=%q", decision, guard.lifecycle)
	}
}

func TestInputIntegritySignalsLatch(t *testing.T) {
	tests := []struct {
		name   string
		fault  func(*Guard, *fakeSource)
		reason Reason
	}{
		{
			name: "invalid metadata",
			fault: func(_ *Guard, source *fakeSource) {
				source.setMeta(func(meta *teleop.StateMeta) { meta.Invalid = true })
			},
			reason: ReasonInvalidInput,
		},
		{
			name: "invalid event",
			fault: func(guard *Guard, _ *fakeSource) {
				guard.Process(teleop.ErrorEvent{Err: teleop.ErrInvalidState})
			},
			reason: ReasonInvalidInput,
		},
		{
			name: "input gap",
			fault: func(guard *Guard, _ *fakeSource) {
				guard.Process(teleop.GapEvent{Source: "input", Reason: "dropped"})
			},
			reason: ReasonInputGap,
		},
		{
			name: "source error",
			fault: func(guard *Guard, _ *fakeSource) {
				guard.Process(teleop.ErrorEvent{Err: errors.New("source failed")})
			},
			reason: ReasonSourceError,
		},
		{
			name: "stale metadata",
			fault: func(_ *Guard, source *fakeSource) {
				source.setMeta(func(meta *teleop.StateMeta) { meta.Stale = true })
			},
			reason: ReasonInputStale,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guard, source := liveGuard(t)
			test.fault(guard, source)
			decision := guard.Evaluate()
			if decision.Permit || !decision.Has(test.reason) || !decision.Has(ReasonNotArmed) {
				t.Fatalf("fault decision = %+v", decision)
			}
		})
	}
}

func TestSubscriptionDeliveryGapDoesNotTripInputIntegrity(t *testing.T) {
	guard, _ := liveGuard(t)
	guard.Process(teleop.GapEvent{
		Source:  "subscription",
		Dropped: 12,
		Reason:  "DeliveryLatest coalesced a full queue",
	})
	decision := guard.Evaluate()
	if !decision.Permit || decision.Has(ReasonInputGap) {
		t.Fatalf("subscription diagnostic changed safety authority: %+v", decision)
	}
}

func TestInputIntegrityLatchesEvenWhenOrdinaryLatchingDisabled(t *testing.T) {
	tests := []struct {
		name   string
		fault  func(*Guard, *fakeSource)
		reason Reason
	}{
		{
			name: "invalid input",
			fault: func(guard *Guard, _ *fakeSource) {
				guard.Process(teleop.ErrorEvent{Err: teleop.ErrInvalidState})
			},
			reason: ReasonInvalidInput,
		},
		{
			name: "stale input",
			fault: func(_ *Guard, source *fakeSource) {
				source.setMeta(func(meta *teleop.StateMeta) { meta.Stale = true })
			},
			reason: ReasonInputStale,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guard, source := liveGuard(t, WithLatching(false))
			test.fault(guard, source)
			decision := guard.Evaluate()
			if decision.Permit || !decision.Has(test.reason) ||
				!decision.Has(ReasonNotArmed) {
				t.Fatalf("integrity-fault decision = %+v", decision)
			}
		})
	}
}

func TestStrictMaritimeProfileRequiresTransportProof(t *testing.T) {
	config := DefaultMaritimeConfig(deadMan)
	guard, err := NewMaritime(config)
	if err != nil {
		t.Fatal(err)
	}
	source := newFakeSource()
	if err := guard.Bind(source); err != nil {
		t.Fatal(err)
	}
	guard.Heartbeat()
	if err := guard.Arm(); !errors.Is(err, ErrUnsafeToArm) {
		t.Fatalf("Arm error = %v, want ErrUnsafeToArm", err)
	} else {
		var armErr *ArmError
		if !errors.As(err, &armErr) ||
			!slices.Contains(armErr.Reasons, ReasonTransportUnverifiable) {
			t.Fatalf("Arm reasons = %v, want transport_unverifiable", armErr)
		}
	}

	checkedAt := source.Monotonic()
	source.setMeta(func(meta *teleop.StateMeta) {
		meta.TransportSilenceVerifiable = true
		meta.TransportCheckSequence = 1
		meta.LastTransportCheckMonotonic = checkedAt
	})
	guard.Heartbeat()
	if err := guard.Arm(); err != nil {
		t.Fatalf("Arm with transport proof: %v", err)
	}
}

func TestStrictMaritimeProfileUsesFreshTransportProofForSteadyState(t *testing.T) {
	config := DefaultMaritimeConfig(deadMan)
	config.CommandTimeout = 20 * time.Millisecond
	config.TransportTimeout = 50 * time.Millisecond
	config.DeadManReactuation = time.Second
	config.LoopWatchdog = 100 * time.Millisecond
	guard, err := NewMaritime(config)
	if err != nil {
		t.Fatal(err)
	}
	source := newFakeSource()
	source.setMeta(func(meta *teleop.StateMeta) {
		meta.TransportSilenceVerifiable = true
		meta.TransportCheckSequence = 1
	})
	if err := guard.Bind(source); err != nil {
		t.Fatal(err)
	}
	guard.Heartbeat()
	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}
	source.advance(time.Nanosecond)
	source.observe(heldState())
	guard.Process(teleop.ButtonEvent{
		Meta:    inputHeader(source),
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})

	// No controller value changes after the press. Independent transport checks
	// continue, so observation age is diagnostic and must not become a false
	// command timeout.
	source.advance(30 * time.Millisecond)
	checkedAt := source.Monotonic()
	source.setMeta(func(meta *teleop.StateMeta) {
		meta.TransportCheckSequence++
		meta.LastTransportCheckMonotonic = checkedAt
		meta.Stale = true
	})
	guard.Heartbeat()
	decision := guard.Evaluate()
	if !decision.Permit || decision.InputAge != 30*time.Millisecond ||
		decision.Has(ReasonCommandTimeout) {
		t.Fatalf("fresh transport decision = %+v", decision)
	}

	// Once independent checks stop, the transport deadline—not state age—trips
	// and latches the guard.
	source.advance(50 * time.Millisecond)
	guard.Heartbeat()
	decision = guard.Evaluate()
	if decision.Permit || !decision.Has(ReasonTransportTimeout) ||
		decision.Has(ReasonCommandTimeout) || !decision.Has(ReasonNotArmed) {
		t.Fatalf("stalled transport decision = %+v", decision)
	}
}

func TestConfigurationRejectsUnsafeNegativeValues(t *testing.T) {
	options := []Option{
		WithCommandTimeout(-time.Nanosecond),
		WithDeadManReactuation(-time.Nanosecond),
		WithLoopWatchdog(-time.Nanosecond),
		WithArmNeutralTolerances(float32(math.NaN()), 0),
		WithArmNeutralTolerances(0, 1),
	}
	for index, option := range options {
		t.Run(fmt.Sprintf("option-%d", index), func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("invalid option did not panic")
				}
			}()
			_ = New(option)
		})
	}

	invalid := DefaultMaritimeConfig(deadMan)
	invalid.TransportTimeout = 0
	if _, err := NewMaritime(invalid); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("NewMaritime error = %v, want ErrInvalidConfiguration", err)
	}
}

func TestLifecycleTransitionTableIsExhaustive(t *testing.T) {
	tests := []struct {
		from    lifecycle
		event   command
		to      lifecycle
		allowed bool
	}{
		{lifecycleIdle, commandArm, lifecycleArmed, true},
		{lifecycleIdle, commandDisarm, lifecycleIdle, true},
		{lifecycleIdle, commandTrip, lifecycleIdle, false},
		{lifecycleIdle, commandStop, lifecycleStopped, true},
		{lifecycleIdle, commandReset, lifecycleIdle, true},
		{lifecycleArmed, commandArm, lifecycleArmed, true},
		{lifecycleArmed, commandDisarm, lifecycleIdle, true},
		{lifecycleArmed, commandTrip, lifecycleIdle, true},
		{lifecycleArmed, commandStop, lifecycleStopped, true},
		{lifecycleArmed, commandReset, lifecycleIdle, true},
		{lifecycleStopped, commandArm, lifecycleStopped, false},
		{lifecycleStopped, commandDisarm, lifecycleStopped, true},
		{lifecycleStopped, commandTrip, lifecycleStopped, false},
		{lifecycleStopped, commandStop, lifecycleStopped, true},
		{lifecycleStopped, commandReset, lifecycleIdle, true},
	}
	guard := &Guard{source: newFakeSource()}
	for _, test := range tests {
		t.Run(string(test.from)+"/"+string(test.event), func(t *testing.T) {
			next, err := gate.Fire(t.Context(), test.from, test.event, guard)
			if (err == nil) != test.allowed {
				t.Fatalf("Fire error = %v, allowed=%t", err, test.allowed)
			}
			if next != test.to {
				t.Fatalf("next = %q, want %q", next, test.to)
			}
		})
	}
}
