// Package safety gates operator input behind a command timeout and a dead-man
// switch.
//
// teleop transports input; it does not decide whether acting on that input is
// safe. A Guard makes that decision explicit and fails closed: it authorizes
// output only when it can affirmatively establish that input is fresh, that an
// operator is present and engaged, that the application's own control loop is
// running, and that nothing has tripped since the last arming. Any condition
// it cannot prove inhibits output.
//
// A Guard is not a certified safety controller and does not replace a
// hardware emergency stop or an independent safety-rated interlock. It is the
// software half of a dead-man policy, and it is only as good as the actuation
// path that honors its decisions.
//
// Typical use:
//
//	guard := safety.New(
//	    safety.WithCommandTimeout(150*time.Millisecond),
//	    safety.WithDeadMan(xbox.ButtonBumperRight),
//	    safety.WithDeadManReactuation(30*time.Second),
//	    safety.WithLoopWatchdog(100*time.Millisecond),
//	)
//	controller, err := provider.Open(ctx, id, teleop.WithProcessor(guard))
//	if err != nil {
//	    return err
//	}
//	guard.Bind(controller)
//
//	for range ticker.C {
//	    guard.Heartbeat()
//	    decision := guard.Evaluate()
//	    drive(decision.Command) // neutral whenever inhibited
//	}
package safety

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/open-ships/teleop"
)

const (
	// DefaultCommandTimeout is the maximum age of the newest observation for
	// which output stays authorized.
	DefaultCommandTimeout = 250 * time.Millisecond
	// DefaultDeadManReactuation requires the operator to release and re-engage
	// the dead-man control periodically. A control that is held indefinitely
	// is indistinguishable from one that has been taped, wedged, or left in
	// the hand of an incapacitated operator.
	DefaultDeadManReactuation = 60 * time.Second
)

// ErrUnbound reports an operation on a Guard that has no controller.
var ErrUnbound = errors.New("teleop/safety: guard is not bound to a controller")

// Source is the controller surface a Guard depends on. *teleop.Controller
// satisfies it.
type Source interface {
	SnapshotWithMeta() (teleop.State, teleop.StateMeta)
	Monotonic() time.Duration
	Done() <-chan struct{}
}

type options struct {
	commandTimeout time.Duration
	deadMan        teleop.ControlID
	reactuation    time.Duration
	loopTimeout    time.Duration
	latching       bool
}

// Option configures a Guard.
type Option func(*options)

// WithCommandTimeout sets the maximum age of the newest observation for which
// output stays authorized, measured on the monotonic clock. It cannot be
// disabled; a non-positive value leaves the default in place.
//
// Choose it from the worst tolerable actuation overrun, not from the expected
// input rate. Note that a change-driven backend legitimately falls silent
// while a control is held steady, so pair a short timeout with
// teleop.WithLiveness so held input keeps producing observations.
func WithCommandTimeout(timeout time.Duration) Option {
	return func(o *options) {
		if timeout > 0 {
			o.commandTimeout = timeout
		}
	}
}

// WithDeadMan requires that a control be held for output to be authorized.
// Releasing it inhibits output immediately.
func WithDeadMan(control teleop.ControlID) Option {
	return func(o *options) { o.deadMan = control }
}

// WithDeadManReactuation requires the dead-man control to be released and
// pressed again within the given interval. Set it to zero to allow an
// indefinite hold, which defeats the purpose of the switch.
func WithDeadManReactuation(interval time.Duration) Option {
	return func(o *options) { o.reactuation = interval }
}

// WithLoopWatchdog inhibits output when the application has not called
// Heartbeat within the interval. It detects a stalled or deadlocked control
// loop, which fresh controller input alone cannot reveal.
func WithLoopWatchdog(interval time.Duration) Option {
	return func(o *options) {
		if interval > 0 {
			o.loopTimeout = interval
		}
	}
}

// WithLatching controls whether a trip requires an explicit Arm to clear. It
// defaults to true: automatic resumption after a fault is how a transient
// dropout becomes an unexpected movement.
func WithLatching(latching bool) Option {
	return func(o *options) { o.latching = latching }
}

// Guard authorizes or inhibits operator output. It is safe for concurrent use.
//
// A Guard is also a teleop processor. Attaching it with teleop.WithProcessor
// gives it lossless edge detection on the dead-man control and publishes every
// transition into the controller's event stream and audit sinks.
type Guard struct {
	options options

	mu     sync.Mutex
	source Source

	armed   bool
	latched bool
	estop   bool

	// heartbeat and deadManSince are monotonic readings from the bound
	// controller's session clock. Zero is a legitimate reading at the start of
	// a session, so each carries a separate flag rather than treating zero as
	// "never set".
	heartbeat     time.Duration
	heartbeatSeen bool
	deadManSince  time.Duration
	deadManHeld   bool

	state   State
	reasons []Reason
	detail  string

	// lastPublished is the decision behind the most recent published event,
	// so transitions can be emitted without republishing a steady state.
	lastPublished Decision
	published     bool
}

// New returns a Guard that inhibits output until it is bound and armed.
func New(opts ...Option) *Guard {
	configured := options{
		commandTimeout: DefaultCommandTimeout,
		reactuation:    DefaultDeadManReactuation,
		latching:       true,
	}
	for _, option := range opts {
		if option != nil {
			option(&configured)
		}
	}
	return &Guard{
		options: configured,
		state:   StateSafe,
		reasons: []Reason{ReasonUnbound},
	}
}

// Bind attaches the Guard to a controller. Call it after Open. Until it is
// called, every decision inhibits output.
func (g *Guard) Bind(source Source) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.source = source
}

// Arm authorizes output subject to the configured conditions, and clears a
// latched trip. It fails while an emergency stop is active: an operator must
// Reset first, which keeps a stop from being cleared by reflex.
func (g *Guard) Arm() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.source == nil {
		return ErrUnbound
	}
	if g.estop {
		return errors.New("teleop/safety: cannot arm during an emergency stop")
	}
	g.armed = true
	g.latched = false
	g.detail = ""
	return nil
}

// Disarm inhibits output until the next Arm.
func (g *Guard) Disarm(detail string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = false
	g.latched = true
	g.detail = detail
}

// EmergencyStop latches an inhibit that only Reset can clear.
func (g *Guard) EmergencyStop(detail string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.estop = true
	g.armed = false
	g.latched = true
	g.detail = detail
}

// Reset clears an emergency stop. Output stays inhibited until Arm.
func (g *Guard) Reset(detail string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.estop = false
	g.detail = detail
}

// Heartbeat records that the application control loop is alive. Call it once
// per control-loop iteration when a loop watchdog is configured.
func (g *Guard) Heartbeat() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.source == nil {
		return
	}
	g.heartbeat = g.source.Monotonic()
	g.heartbeatSeen = true
}

// Evaluate returns the authorization decision for this instant and is the only
// safe way to gate output. Call it immediately before acting, every time: a
// decision describes the moment it was made and nothing after it.
//
// Evaluate is not read-only. A condition that fails here latches the Guard, so
// output stays inhibited until an operator re-arms.
func (g *Guard) Evaluate() Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.evaluateLocked()
}

// State returns the current authorization state without re-evaluating.
func (g *Guard) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

func (g *Guard) evaluateLocked() Decision {
	if g.source == nil {
		g.state = StateSafe
		g.reasons = []Reason{ReasonUnbound}
		return Decision{State: StateSafe, Reasons: g.copyReasons()}
	}

	now := g.source.Monotonic()
	state, meta := g.source.SnapshotWithMeta()
	reasons := make([]Reason, 0, 4)

	select {
	case <-g.source.Done():
		reasons = append(reasons, ReasonControllerFault)
	default:
	}

	// Input freshness. An input path that has never delivered an observation
	// has never been shown to work, so it is treated as failed rather than as
	// merely quiet.
	var age time.Duration
	switch {
	case meta.Sequence == 0:
		reasons = append(reasons, ReasonNoInput)
		age = now
	default:
		age = now - meta.ReceivedMonotonic
		if age < 0 {
			age = 0
		}
		if age >= g.options.commandTimeout {
			reasons = append(reasons, ReasonCommandTimeout)
		}
	}
	if !meta.Connected {
		reasons = append(reasons, ReasonDisconnected)
	}
	if meta.Synthetic {
		reasons = append(reasons, ReasonSynthetic)
	}

	if g.options.loopTimeout > 0 {
		if !g.heartbeatSeen || now-g.heartbeat >= g.options.loopTimeout {
			reasons = append(reasons, ReasonLoopStalled)
		}
	}

	if g.options.deadMan != "" {
		switch {
		case !state.Button(g.options.deadMan):
			reasons = append(reasons, ReasonDeadManReleased)
		case !g.deadManHeld:
			// Held, but the Guard never saw the press that started the hold:
			// it cannot bound how long the control has been down.
			reasons = append(reasons, ReasonDeadManStale)
		case g.options.reactuation > 0 && now-g.deadManSince >= g.options.reactuation:
			reasons = append(reasons, ReasonDeadManStale)
		}
	}

	if g.estop {
		reasons = append(reasons, ReasonEmergencyStop)
	}
	if !g.armed {
		reasons = append(reasons, ReasonNotArmed)
	}

	// Any unmet condition latches, so a transient fault cannot silently
	// restore authority when it clears.
	if len(reasons) > 0 && g.options.latching && g.armed {
		if !onlyDeadManEngagement(reasons) {
			g.armed = false
			g.latched = true
			reasons = appendUnique(reasons, ReasonNotArmed)
		}
	}

	decision := Decision{
		Reasons:     reasons,
		InputAge:    age,
		EvaluatedAt: now,
	}
	switch {
	case len(reasons) == 0:
		decision.Permit = true
		decision.State = StateLive
		decision.Command = state
	case onlyDeadManEngagement(reasons):
		// Every automatic condition holds; the operator has simply let go.
		decision.State = StateArmed
	default:
		decision.State = StateSafe
	}

	g.state = decision.State
	g.reasons = append([]Reason(nil), reasons...)
	return decision
}

// onlyDeadManEngagement reports a decision blocked solely because the operator
// is not currently pressing the dead-man control. That is normal operation
// rather than a fault, so it must not latch.
func onlyDeadManEngagement(reasons []Reason) bool {
	if len(reasons) == 0 {
		return false
	}
	for _, reason := range reasons {
		if reason != ReasonDeadManReleased {
			return false
		}
	}
	return true
}

func appendUnique(reasons []Reason, reason Reason) []Reason {
	for _, candidate := range reasons {
		if candidate == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}

func (g *Guard) copyReasons() []Reason {
	return append([]Reason(nil), g.reasons...)
}

// Process implements teleop.Processor for callers that cannot supply a
// ProcessingContext. It tracks dead-man edges but publishes no events.
func (g *Guard) Process(event teleop.Event) []teleop.Event {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observeLocked(event)
	return nil
}

// ProcessContext implements teleop.ContextProcessor. It observes dead-man
// edges losslessly and publishes a decision event whenever authorization
// changes.
func (g *Guard) ProcessContext(
	_ context.Context,
	pc teleop.ProcessingContext,
	event teleop.Event,
) ([]teleop.Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observeLocked(event)
	decision := g.evaluateLocked()
	return g.transitionLocked(pc, decision, event.Header().ID), nil
}

// Advance implements teleop.AdvancingProcessor.
func (g *Guard) Advance(time.Time) []teleop.Event { return nil }

// AdvanceContext implements teleop.ContextAdvancingProcessor. Periodic
// re-evaluation is what lets a command timeout or a stalled control loop trip
// the Guard even when no input is arriving and the application never calls
// Evaluate.
func (g *Guard) AdvanceContext(
	_ context.Context,
	pc teleop.ProcessingContext,
	now time.Time,
) ([]teleop.Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	decision := g.evaluateLocked()
	return g.transitionLocked(pc, decision, teleop.EventID{}), nil
}

// observeLocked tracks dead-man press and release edges from the event stream.
// Sampling the snapshot would miss a press and release that fall between two
// evaluations, which is exactly the case a re-actuation deadline exists to
// detect.
func (g *Guard) observeLocked(event teleop.Event) {
	if g.options.deadMan == "" {
		return
	}
	button, ok := event.(teleop.ButtonEvent)
	if !ok {
		pointer, isPointer := event.(*teleop.ButtonEvent)
		if !isPointer {
			return
		}
		button = *pointer
	}
	if button.Button != g.options.deadMan {
		return
	}
	if button.Pressed {
		// Only a rising edge restarts the re-actuation deadline; a repeated
		// press report for a control already down must not extend it.
		if !g.deadManHeld {
			g.deadManHeld = true
			g.deadManSince = button.Meta.Monotonic
		}
		return
	}
	g.deadManHeld = false
	g.deadManSince = 0
}

// transitionLocked emits an event only when authorization actually changed, so
// a steady state costs nothing in the log.
func (g *Guard) transitionLocked(
	pc teleop.ProcessingContext,
	decision Decision,
	cause teleop.EventID,
) []teleop.Event {
	if g.published && !g.changedLocked(decision) {
		return nil
	}
	g.published = true
	g.lastPublished = decision

	var causes []teleop.EventID
	if cause != (teleop.EventID{}) {
		causes = []teleop.EventID{cause}
	}
	header := pc.NewHeader("safety", pc.Now(), 0, causes...)
	return []teleop.Event{Event{
		Meta:     header,
		State:    decision.State,
		Permit:   decision.Permit,
		Reasons:  append([]Reason(nil), decision.Reasons...),
		InputAge: decision.InputAge,
		Detail:   g.detail,
	}}
}

func (g *Guard) changedLocked(decision Decision) bool {
	if decision.State != g.lastPublished.State ||
		decision.Permit != g.lastPublished.Permit ||
		len(decision.Reasons) != len(g.lastPublished.Reasons) {
		return true
	}
	for index, reason := range decision.Reasons {
		if g.lastPublished.Reasons[index] != reason {
			return true
		}
	}
	return false
}
