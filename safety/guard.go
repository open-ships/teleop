// Package safety gates operator input behind a command timeout and a dead-man
// switch.
//
// teleop transports input; it does not decide whether acting on that input is
// safe. A Guard makes that decision explicit and fails closed: it authorizes
// output only when it can affirmatively establish that the configured input
// and transport freshness policy holds, that an operator is present and
// engaged, that the application's own control loop is running, and that
// nothing has tripped since the last arming. Any unestablished condition
// inhibits output.
//
// A Guard is not a certified safety controller and does not replace a
// hardware emergency stop or an independent safety-rated interlock. It is the
// software half of a dead-man policy, and it is only as good as the actuation
// path that honors its decisions.
//
// Hazardous production integrations should use assured.Session, which owns a
// strict maritime Guard together with durable evidence, Safety Authority,
// expiring actuator leases, acknowledgement validation, and ordered shutdown.
// Low-level users that construct a Guard directly should still route every
// actuator request through Authority; a separate Evaluate-then-send loop leaves
// a race in which authority can change between the decision and transmission.
package safety

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
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
	// DefaultMaritimeLoopWatchdog is the conservative application-loop watchdog
	// used by DefaultMaritimeConfig. Deployments must still choose all deadlines
	// from their own hazard analysis and enforce a shorter independent actuator
	// lease where necessary.
	DefaultMaritimeLoopWatchdog = 100 * time.Millisecond
	// DefaultArmStickTolerance permits small normalized stick drift during Arm
	// without applying an operational dead zone to live commands.
	DefaultArmStickTolerance float32 = 0.05
	// DefaultArmTriggerTolerance permits small normalized trigger drift during
	// Arm. Every digital control must still be released exactly.
	DefaultArmTriggerTolerance float32 = 0.02
)

var (
	// ErrUnbound reports an operation on a Guard that has no controller.
	ErrUnbound = errors.New("teleop/safety: guard is not bound to a controller")
	// ErrAlreadyBound reports an attempt to carry a Guard's authority or timing
	// proof into another controller session. Guards are deliberately one-shot.
	ErrAlreadyBound = errors.New("teleop/safety: guard is already bound")
	// ErrUnsafeToArm reports that one or more arming interlocks are not met.
	ErrUnsafeToArm = errors.New("teleop/safety: unsafe to arm")
	// ErrInvalidConfiguration reports a safety configuration that silently
	// weakens a required interlock.
	ErrInvalidConfiguration = errors.New("teleop/safety: invalid configuration")
)

// Source is the controller surface a Guard depends on. *teleop.Controller
// satisfies it.
type Source interface {
	SnapshotWithMeta() (teleop.State, teleop.StateMeta)
	Monotonic() time.Duration
	Done() <-chan struct{}
}

type options struct {
	commandTimeout      time.Duration
	deadMan             teleop.ControlID
	reactuation         time.Duration
	loopTimeout         time.Duration
	transportTimeout    time.Duration
	armStickTolerance   float32
	armTriggerTolerance float32
	latching            bool
	strict              bool
	validationErr       error
}

// MaritimeConfig is the validated production-oriented Guard profile. It
// requires every software interlock the Guard can provide and always latches.
// It complements, rather than replaces, an actuator-side timeout and a
// hardware emergency stop.
type MaritimeConfig struct {
	CommandTimeout      time.Duration
	TransportTimeout    time.Duration
	DeadMan             teleop.ControlID
	DeadManReactuation  time.Duration
	LoopWatchdog        time.Duration
	ArmStickTolerance   float32
	ArmTriggerTolerance float32
}

// DefaultMaritimeConfig returns the strict profile with conservative library
// defaults. A deployment must replace them when its hazard analysis requires
// shorter deadlines.
func DefaultMaritimeConfig(deadMan teleop.ControlID) MaritimeConfig {
	return MaritimeConfig{
		CommandTimeout:      DefaultCommandTimeout,
		TransportTimeout:    DefaultCommandTimeout,
		DeadMan:             deadMan,
		DeadManReactuation:  DefaultDeadManReactuation,
		LoopWatchdog:        DefaultMaritimeLoopWatchdog,
		ArmStickTolerance:   DefaultArmStickTolerance,
		ArmTriggerTolerance: DefaultArmTriggerTolerance,
	}
}

// Option configures a Guard.
type Option func(*options)

// WithCommandTimeout sets the maximum age of the newest observation for which
// output stays authorized, measured on the monotonic clock. It cannot be
// disabled; zero leaves the default in place and a negative value is rejected.
//
// Choose it from the worst tolerable actuation overrun, not from the expected
// input rate. A change-driven backend legitimately falls silent while a control
// is held steady, so an ordinary Guard will conservatively time out. The strict
// maritime profile additionally requires independent TransportHealthSource
// evidence; teleop.WithLiveness reports age but does not manufacture evidence.
func WithCommandTimeout(timeout time.Duration) Option {
	return func(o *options) {
		if timeout < 0 {
			o.validationErr = errors.Join(
				o.validationErr,
				fmt.Errorf("%w: negative command timeout %s", ErrInvalidConfiguration, timeout),
			)
			return
		}
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
	return func(o *options) {
		if interval < 0 {
			o.validationErr = errors.Join(
				o.validationErr,
				fmt.Errorf("%w: negative dead-man re-actuation %s", ErrInvalidConfiguration, interval),
			)
			return
		}
		o.reactuation = interval
	}
}

// WithLoopWatchdog inhibits output when the application has not called
// Heartbeat within the interval. It detects a stalled or deadlocked control
// loop, which fresh controller input alone cannot reveal.
func WithLoopWatchdog(interval time.Duration) Option {
	return func(o *options) {
		if interval < 0 {
			o.validationErr = errors.Join(
				o.validationErr,
				fmt.Errorf("%w: negative loop watchdog %s", ErrInvalidConfiguration, interval),
			)
			return
		}
		if interval > 0 {
			o.loopTimeout = interval
		}
	}
}

// WithArmNeutralTolerances sets the maximum radial stick magnitude and trigger
// value accepted while arming. It does not modify live controller input and is
// therefore not an operational dead zone. Values must be finite and in [0,1).
func WithArmNeutralTolerances(stick, trigger float32) Option {
	return func(o *options) {
		if !validNeutralTolerance(stick) || !validNeutralTolerance(trigger) {
			o.validationErr = errors.Join(
				o.validationErr,
				fmt.Errorf(
					"%w: arm neutral tolerances must be finite and in [0,1): %v, %v",
					ErrInvalidConfiguration,
					stick,
					trigger,
				),
			)
			return
		}
		o.armStickTolerance = stick
		o.armTriggerTolerance = trigger
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

	// lifecycle is the operator's standing intent, moved only through gate.
	lifecycle lifecycle

	// heartbeat and deadManSince are monotonic readings from the bound
	// controller's session clock. Zero is a legitimate reading at the start of
	// a session, so each carries a separate flag rather than treating zero as
	// "never set".
	heartbeat       time.Duration
	heartbeatSeen   bool
	deadManSince    time.Duration
	deadManHeld     bool
	deadManReleased bool
	armedAt         time.Duration
	// armedInputSequence disambiguates a post-Arm event that shares the same
	// monotonic clock tick as Arm on a coarse-resolution platform. Time remains
	// authoritative when it differs; exact controller order is only the
	// tie-breaker.
	armedInputSequence uint64

	invalidInput bool
	inputGap     bool
	sourceError  bool

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
		commandTimeout:      DefaultCommandTimeout,
		reactuation:         DefaultDeadManReactuation,
		armStickTolerance:   DefaultArmStickTolerance,
		armTriggerTolerance: DefaultArmTriggerTolerance,
		latching:            true,
	}
	for _, option := range opts {
		if option != nil {
			option(&configured)
		}
	}
	if configured.validationErr != nil {
		panic(configured.validationErr)
	}
	return newGuard(configured)
}

// NewMaritime returns a Guard using the validated strict maritime profile.
// Unlike New, it reports invalid configuration as an error because production
// values commonly come from deployment configuration rather than source code.
func NewMaritime(config MaritimeConfig) (*Guard, error) {
	var validationErr error
	if config.CommandTimeout <= 0 {
		validationErr = errors.Join(validationErr, fmt.Errorf(
			"%w: command timeout must be positive",
			ErrInvalidConfiguration,
		))
	}
	if config.TransportTimeout <= 0 {
		validationErr = errors.Join(validationErr, fmt.Errorf(
			"%w: transport timeout must be positive",
			ErrInvalidConfiguration,
		))
	}
	if config.DeadMan == "" {
		validationErr = errors.Join(validationErr, fmt.Errorf(
			"%w: dead-man control is required",
			ErrInvalidConfiguration,
		))
	}
	if config.DeadManReactuation <= 0 {
		validationErr = errors.Join(validationErr, fmt.Errorf(
			"%w: dead-man re-actuation must be positive",
			ErrInvalidConfiguration,
		))
	}
	if config.LoopWatchdog <= 0 {
		validationErr = errors.Join(validationErr, fmt.Errorf(
			"%w: loop watchdog must be positive",
			ErrInvalidConfiguration,
		))
	}
	if !validNeutralTolerance(config.ArmStickTolerance) ||
		!validNeutralTolerance(config.ArmTriggerTolerance) {
		validationErr = errors.Join(validationErr, fmt.Errorf(
			"%w: arm neutral tolerances must be finite and in [0,1)",
			ErrInvalidConfiguration,
		))
	}
	if validationErr != nil {
		return nil, validationErr
	}
	return newGuard(options{
		commandTimeout:      config.CommandTimeout,
		transportTimeout:    config.TransportTimeout,
		deadMan:             config.DeadMan,
		reactuation:         config.DeadManReactuation,
		loopTimeout:         config.LoopWatchdog,
		armStickTolerance:   config.ArmStickTolerance,
		armTriggerTolerance: config.ArmTriggerTolerance,
		latching:            true,
		strict:              true,
	}), nil
}

func validNeutralTolerance(value float32) bool {
	return !float32NonFinite(value) && value >= 0 && value < 1
}

func float32NonFinite(value float32) bool {
	return math.IsNaN(float64(value)) || math.IsInf(float64(value), 0)
}

func newGuard(configured options) *Guard {
	return &Guard{
		options:   configured,
		lifecycle: lifecycleIdle,
		state:     StateSafe,
		reasons:   []Reason{ReasonUnbound},
	}
}

// ArmError identifies every interlock that refused an Arm request.
type ArmError struct {
	Reasons []Reason
}

func (e *ArmError) Error() string {
	return fmt.Sprintf("%s: %v", ErrUnsafeToArm, e.Reasons)
}

func (*ArmError) Unwrap() error { return ErrUnsafeToArm }

// Bind attaches the Guard to exactly one controller session. Call it after
// Open. Until it is called, every decision inhibits output. Rebinding is
// rejected so lifecycle, heartbeat, and engagement proof cannot cross session
// clocks.
func (g *Guard) Bind(source Source) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if source == nil {
		return ErrUnbound
	}
	if g.source != nil {
		return ErrAlreadyBound
	}
	if g.options.strict {
		capable, ok := source.(interface{ Capabilities() teleop.Capabilities })
		if !ok {
			return fmt.Errorf(
				"%w: strict profile requires controller capabilities",
				ErrInvalidConfiguration,
			)
		}
		capabilities := capable.Capabilities()
		var deadManKind teleop.ControlKind
		for _, control := range capabilities.Controls {
			if control.ID == g.options.deadMan {
				deadManKind = control.Kind
				break
			}
		}
		if deadManKind != teleop.ControlButton && deadManKind != teleop.ControlDPad {
			return fmt.Errorf(
				"%w: dead-man control %q is not an available digital control",
				ErrInvalidConfiguration,
				g.options.deadMan,
			)
		}
	}
	g.source = source
	g.clearAuthorizationProofLocked()
	g.invalidInput = false
	g.inputGap = false
	g.sourceError = false
	g.state = StateSafe
	g.reasons = []Reason{ReasonNotArmed}
	g.detail = ""
	g.published = false
	g.lastPublished = Decision{}

	// A complete released snapshot is a valid startup baseline. A held startup
	// snapshot deliberately establishes no engagement proof.
	state, meta := source.SnapshotWithMeta()
	if g.options.deadMan != "" &&
		meta.Sequence > 0 && meta.Connected && !meta.Stale &&
		!meta.Synthetic && !meta.Invalid && !state.Button(g.options.deadMan) {
		g.deadManReleased = true
	}
	return nil
}

// Arm authorizes output subject to the configured conditions, and clears a
// latched trip. Every control must first be neutral, the dead-man must have an
// observed released baseline, and a new press is required after Arm. Arm fails
// while an emergency stop is active: an operator must Reset first, which keeps
// a stop from being cleared by reflex.
func (g *Guard) Arm() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.source == nil {
		return ErrUnbound
	}
	if g.lifecycle == lifecycleStopped {
		return errArmDuringStop
	}
	sample := g.sampleLocked()
	reasons := slices.Clone(sample.reasons)
	if !neutralForArm(
		sample.state,
		g.options.armStickTolerance,
		g.options.armTriggerTolerance,
	) {
		reasons = appendUnique(reasons, ReasonControlsNotNeutral)
	}
	if g.options.deadMan != "" &&
		(sample.state.Button(g.options.deadMan) || !g.deadManReleased) {
		reasons = appendUnique(reasons, ReasonDeadManReleaseRequired)
	}
	if len(reasons) > 0 {
		return &ArmError{Reasons: slices.Clone(reasons)}
	}
	next, err := gate.Fire(context.Background(), g.lifecycle, commandArm, g)
	if err != nil {
		// Report this package's own errors rather than the machine's refusal,
		// so a caller comparing against ErrUnbound keeps working and safety's
		// error contract stays independent of the machine.
		if errors.Is(err, ErrUnbound) {
			return ErrUnbound
		}
		return errArmDuringStop
	}
	g.lifecycle = next
	g.deadManHeld = false
	g.deadManSince = 0
	g.armedAt = sample.now
	g.armedInputSequence = sample.meta.Sequence
	g.detail = ""
	g.evaluateLocked()
	return nil
}

// Disarm inhibits output until the next Arm.
func (g *Guard) Disarm(detail string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fire(commandDisarm)
	g.detail = detail
	g.evaluateLocked()
}

// EmergencyStop latches an inhibit that only Reset can clear.
func (g *Guard) EmergencyStop(detail string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fire(commandStop)
	g.detail = detail
	g.evaluateLocked()
}

// Reset clears an emergency stop. Output stays inhibited until Arm.
func (g *Guard) Reset(detail string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fire(commandReset)
	g.detail = detail
	g.evaluateLocked()
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

	sample := g.sampleLocked()
	now := sample.now
	state := sample.state
	reasons := slices.Clone(sample.reasons)

	if g.options.deadMan != "" {
		switch {
		case !g.deadManReleased ||
			(g.lifecycle != lifecycleArmed && state.Button(g.options.deadMan)):
			reasons = append(reasons, ReasonDeadManReleaseRequired)
		case !state.Button(g.options.deadMan):
			reasons = append(reasons, ReasonDeadManReleased)
		case !g.deadManHeld:
			// Held, but the Guard never saw the press that started the hold,
			// so it cannot bound how long the control has been down. This is
			// ordinary while a press propagates through the pipeline, so it
			// inhibits without latching.
			reasons = append(reasons, ReasonDeadManUnconfirmed)
		case g.deadManSince < 0 || g.deadManSince > now:
			reasons = append(reasons, ReasonDeadManStale)
		case g.options.reactuation > 0 && now-g.deadManSince >= g.options.reactuation:
			reasons = append(reasons, ReasonDeadManStale)
		}
	}

	if g.lifecycle == lifecycleStopped {
		reasons = append(reasons, ReasonEmergencyStop)
	}
	if g.lifecycle != lifecycleArmed {
		reasons = append(reasons, ReasonNotArmed)
	}

	// Any unmet condition latches, so a transient fault cannot silently
	// restore authority when it clears.
	if len(reasons) > 0 && g.lifecycle == lifecycleArmed &&
		(g.options.latching || containsIntegrityFault(reasons)) {
		if !onlyDeadManEngagement(reasons) {
			g.fire(commandTrip)
			reasons = appendUnique(reasons, ReasonNotArmed)
		}
	}

	decision := Decision{
		Reasons:       reasons,
		InputAge:      sample.age,
		InputSequence: sample.meta.Sequence,
		EvaluatedAt:   now,
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
	g.reasons = slices.Clone(reasons)
	return decision
}

type inputSample struct {
	state   teleop.State
	meta    teleop.StateMeta
	now     time.Duration
	age     time.Duration
	reasons []Reason
}

func (g *Guard) sampleLocked() inputSample {
	state, meta := g.source.SnapshotWithMeta()
	now := g.source.Monotonic()
	sample := inputSample{
		state:   state,
		meta:    meta,
		now:     now,
		reasons: make([]Reason, 0, 10),
	}

	select {
	case <-g.source.Done():
		sample.reasons = append(sample.reasons, ReasonControllerFault)
		g.clearAuthorizationProofLocked()
	default:
	}
	if !meta.Connected || meta.Synthetic {
		g.clearAuthorizationProofLocked()
	}

	// Input freshness. An input path that has never delivered an observation
	// has never been shown to work, so it is treated as failed rather than quiet.
	switch meta.Sequence {
	case 0:
		sample.reasons = append(sample.reasons, ReasonNoInput)
		sample.age = max(now, 0)
	default:
		if meta.ReceivedMonotonic < 0 || meta.ReceivedMonotonic > now {
			sample.reasons = append(sample.reasons, ReasonSourceError)
		} else {
			sample.age = now - meta.ReceivedMonotonic
		}
		if !g.options.strict &&
			!slices.Contains(sample.reasons, ReasonSourceError) &&
			sample.age >= g.options.commandTimeout {
			sample.reasons = append(sample.reasons, ReasonCommandTimeout)
		}
	}
	if !meta.Connected {
		sample.reasons = append(sample.reasons, ReasonDisconnected)
	}
	if meta.Synthetic {
		sample.reasons = append(sample.reasons, ReasonSynthetic)
	}
	if meta.Invalid || g.invalidInput {
		sample.reasons = appendUnique(sample.reasons, ReasonInvalidInput)
	}
	// In the strict profile, observation silence is expected for a steady
	// control and independent transport evidence owns the deadline. A synthetic
	// stale neutralization still trips through ReasonSynthetic.
	if meta.Stale && !g.options.strict {
		sample.reasons = append(sample.reasons, ReasonInputStale)
	}
	if g.inputGap {
		sample.reasons = append(sample.reasons, ReasonInputGap)
	}
	if g.sourceError {
		sample.reasons = append(sample.reasons, ReasonSourceError)
	}
	if g.options.strict {
		switch {
		case !meta.TransportSilenceVerifiable:
			sample.reasons = append(sample.reasons, ReasonTransportUnverifiable)
		case meta.TransportCheckSequence == 0:
			sample.reasons = append(sample.reasons, ReasonTransportUnverifiable)
		case meta.LastTransportCheckMonotonic < 0 ||
			meta.LastTransportCheckMonotonic > now:
			sample.reasons = append(sample.reasons, ReasonTransportUnverifiable)
		case now-meta.LastTransportCheckMonotonic >= g.options.transportTimeout:
			sample.reasons = append(sample.reasons, ReasonTransportTimeout)
		}
	}
	if g.options.loopTimeout > 0 &&
		(!g.heartbeatSeen || g.heartbeat < 0 || g.heartbeat > now ||
			now-g.heartbeat >= g.options.loopTimeout) {
		sample.reasons = append(sample.reasons, ReasonLoopStalled)
	}
	return sample
}

// onlyDeadManEngagement reports a decision blocked solely because the operator
// is not currently engaging the dead-man control, or because an engagement has
// not yet been observed. Both are normal operation rather than faults, so
// neither may latch: a gate that demands a re-arm every time an operator lets
// go, or every time a press is still in flight, is one operators defeat.
func onlyDeadManEngagement(reasons []Reason) bool {
	if len(reasons) == 0 {
		return false
	}
	for _, reason := range reasons {
		if reason != ReasonDeadManReleased &&
			reason != ReasonDeadManUnconfirmed &&
			reason != ReasonDeadManReleaseRequired {
			return false
		}
	}
	return true
}

func containsIntegrityFault(reasons []Reason) bool {
	for _, reason := range reasons {
		switch reason {
		case ReasonInvalidInput,
			ReasonInputGap,
			ReasonInputStale,
			ReasonNoInput,
			ReasonSourceError,
			ReasonControllerFault,
			ReasonDisconnected,
			ReasonSynthetic,
			ReasonTransportUnverifiable,
			ReasonTransportTimeout:
			return true
		}
	}
	return false
}

func appendUnique(reasons []Reason, reason Reason) []Reason {
	if slices.Contains(reasons, reason) {
		return reasons
	}
	return append(reasons, reason)
}

func (g *Guard) copyReasons() []Reason {
	return slices.Clone(g.reasons)
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
	_ time.Time,
) ([]teleop.Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	decision := g.evaluateLocked()
	return g.transitionLocked(pc, decision, teleop.EventID{}), nil
}

// observeLocked tracks integrity signals and dead-man edges from the canonical
// event stream. Sampling only the snapshot would miss a press and release that
// fall between evaluations, which is exactly the case a re-actuation deadline
// exists to detect.
func (g *Guard) observeLocked(event teleop.Event) {
	switch value := event.(type) {
	case teleop.ObservationEvent:
		g.observeObservationLocked(value)
	case *teleop.ObservationEvent:
		g.observeObservationLocked(*value)
	case teleop.ErrorEvent:
		g.observeErrorLocked(value)
	case *teleop.ErrorEvent:
		g.observeErrorLocked(*value)
	case teleop.GapEvent:
		if value.Source != "subscription" {
			g.inputGap = true
		}
	case *teleop.GapEvent:
		if value.Source != "subscription" {
			g.inputGap = true
		}
	case teleop.ConnectionEvent:
		if value.State == teleop.Disconnected {
			g.clearAuthorizationProofLocked()
		}
	case *teleop.ConnectionEvent:
		if value.State == teleop.Disconnected {
			g.clearAuthorizationProofLocked()
		}
	}

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
	if button.Meta.Synthetic ||
		(button.Meta.ID.Stream != "" && button.Meta.ID.Stream != "input") {
		g.clearAuthorizationProofLocked()
		return
	}
	if g.source == nil {
		g.clearAuthorizationProofLocked()
		return
	}
	received := button.Meta.ReceivedMonotonic
	now := g.source.Monotonic()
	if received < 0 || received > now {
		g.sourceError = true
		g.clearAuthorizationProofLocked()
		return
	}
	if button.Pressed {
		// Only a rising edge restarts the re-actuation deadline; a repeated
		// press report for a control already down must not extend it. A press
		// before Arm or before an observed release is initialization, not proof
		// of operator engagement.
		if g.lifecycle == lifecycleArmed &&
			g.deadManReleased &&
			g.pressFollowsArmLocked(button.Meta, received) &&
			!g.deadManHeld {
			g.deadManHeld = true
			g.deadManSince = received
		}
		return
	}
	g.deadManHeld = false
	g.deadManSince = 0
	g.deadManReleased = true
}

func (g *Guard) observeObservationLocked(event teleop.ObservationEvent) {
	if event.Meta.Synthetic {
		g.clearAuthorizationProofLocked()
		return
	}
	// A subsequent complete physical observation resolves a transient event
	// signal. The lifecycle trip remains idle until an explicit Arm.
	g.invalidInput = false
	g.inputGap = false
	g.sourceError = false
	if g.options.deadMan != "" && !event.Current.Button(g.options.deadMan) {
		g.deadManHeld = false
		g.deadManSince = 0
		g.deadManReleased = true
	}
}

func (g *Guard) observeErrorLocked(event teleop.ErrorEvent) {
	if errors.Is(event.Err, teleop.ErrInvalidState) {
		g.invalidInput = true
		return
	}
	g.sourceError = true
}

func (g *Guard) clearAuthorizationProofLocked() {
	g.heartbeat = 0
	g.heartbeatSeen = false
	g.deadManSince = 0
	g.deadManHeld = false
	g.deadManReleased = false
	g.armedAt = 0
	g.armedInputSequence = 0
}

func (g *Guard) pressFollowsArmLocked(header teleop.Header, received time.Duration) bool {
	if received > g.armedAt {
		return true
	}
	if received != g.armedAt || g.armedInputSequence == 0 ||
		header.ID.Stream != "input" ||
		header.ID.Sequence <= g.armedInputSequence {
		return false
	}
	identified, ok := g.source.(interface{ Session() teleop.SessionID })
	if !ok {
		return false
	}
	session := identified.Session()
	return session != (teleop.SessionID{}) && header.ID.Session == session
}

func neutralForArm(state teleop.State, stickTolerance, triggerTolerance float32) bool {
	if float32NonFinite(state.LeftStick.X) ||
		float32NonFinite(state.LeftStick.Y) ||
		float32NonFinite(state.RightStick.X) ||
		float32NonFinite(state.RightStick.Y) ||
		float32NonFinite(state.LeftTrigger) ||
		float32NonFinite(state.RightTrigger) {
		return false
	}
	leftMagnitude := math.Hypot(float64(state.LeftStick.X), float64(state.LeftStick.Y))
	rightMagnitude := math.Hypot(float64(state.RightStick.X), float64(state.RightStick.Y))
	if leftMagnitude > float64(stickTolerance) ||
		rightMagnitude > float64(stickTolerance) ||
		state.LeftTrigger < 0 || state.RightTrigger < 0 ||
		state.LeftTrigger > triggerTolerance ||
		state.RightTrigger > triggerTolerance {
		return false
	}
	for _, control := range teleop.StandardButtonIDs() {
		if state.Button(control) {
			return false
		}
	}
	for _, pressed := range state.Buttons.Extensions {
		if pressed {
			return false
		}
	}
	return true
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
		Reasons:  slices.Clone(decision.Reasons),
		InputAge: decision.InputAge,
		Detail:   g.detail,
	}}
}

func (g *Guard) changedLocked(decision Decision) bool {
	return decision.State != g.lastPublished.State ||
		decision.Permit != g.lastPublished.Permit ||
		!slices.Equal(decision.Reasons, g.lastPublished.Reasons)
}
