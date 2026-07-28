package safety

import (
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

// liveGuard returns an armed, engaged Guard permitting output, which is the
// baseline each inhibiting condition is tested against.
func liveGuard(t *testing.T, opts ...Option) (*Guard, *fakeSource) {
	t.Helper()
	source := newFakeSource()
	guard := New(append([]Option{
		WithCommandTimeout(100 * time.Millisecond),
		WithDeadMan(deadMan),
	}, opts...)...)
	guard.Bind(source)

	// The Guard must witness the press that begins the hold.
	guard.Process(teleop.ButtonEvent{
		Meta:    teleop.Header{Monotonic: source.Monotonic()},
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})
	source.observe(heldState())
	if err := guard.Arm(); err != nil {
		t.Fatalf("arm: %v", err)
	}
	decision := guard.Evaluate()
	if !decision.Permit {
		t.Fatalf("baseline must permit, got %v", decision.Reasons)
	}
	return guard, source
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

// TestNeverObservedInputDeniesOutput covers a controller that opened but from
// which no observation ever arrived: silence that has never been broken is not
// evidence of a working input path.
func TestNeverObservedInputDeniesOutput(t *testing.T) {
	source := newFakeSource()
	source.setMeta(func(meta *teleop.StateMeta) { meta.Sequence = 0 })
	guard := New()
	guard.Bind(source)
	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}

	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("output must be denied before any observation arrives")
	}
	if !decision.Has(ReasonNoInput) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
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

	source.observe(heldState())
	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("fresh input must not silently restore authority after a trip")
	}
	if !decision.Has(ReasonNotArmed) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}

	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}
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
		Meta:   teleop.Header{Monotonic: source.Monotonic()},
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
		Meta:    teleop.Header{Monotonic: source.Monotonic()},
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

	// Releasing and pressing again restarts the deadline, but the trip
	// latched, so an explicit re-arm is still required.
	guard.Process(teleop.ButtonEvent{
		Meta:   teleop.Header{Monotonic: source.Monotonic()},
		Button: deadMan,
		Phase:  teleop.PhaseReleased,
	})
	guard.Process(teleop.ButtonEvent{
		Meta:    teleop.Header{Monotonic: source.Monotonic()},
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})
	source.observe(heldState())
	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}
	if !guard.Evaluate().Permit {
		t.Fatal("re-actuation followed by a re-arm must restore authority")
	}
}

// TestDeadManHeldWithoutObservedPressIsStale covers a Guard bound while the
// control was already down: it cannot bound the hold, so it must not trust it.
func TestDeadManHeldWithoutObservedPressIsStale(t *testing.T) {
	source := newFakeSource()
	guard := New(WithDeadMan(deadMan))
	guard.Bind(source)
	source.observe(heldState())
	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}

	decision := guard.Evaluate()
	if decision.Permit {
		t.Fatal("an unobserved hold must not authorize output")
	}
	if !decision.Has(ReasonDeadManUnconfirmed) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
	// An unobserved hold is ordinary rather than a fault, so it must not
	// demand a re-arm once the press is seen.
	if decision.State != StateArmed {
		t.Fatalf("state = %q, want %q", decision.State, StateArmed)
	}
	guard.Process(teleop.ButtonEvent{
		Meta:    teleop.Header{Monotonic: source.Monotonic()},
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})
	if !guard.Evaluate().Permit {
		t.Fatal("observing the press must restore authority without a re-arm")
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
	source.observe(heldState())
	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}
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
	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}
	if !guard.Evaluate().Permit {
		t.Fatal("re-arming must restore authority")
	}
}

func TestConcurrentEvaluationIsRaceFree(t *testing.T) {
	guard, source := liveGuard(t)

	var waiting sync.WaitGroup
	for range 8 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			for range 200 {
				guard.Heartbeat()
				guard.Evaluate()
				source.observe(heldState())
			}
		}()
	}
	waiting.Wait()
}
