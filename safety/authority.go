package safety

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/open-ships/teleop"
)

// Authority errors are deliberately distinguishable so a supervisor can make
// evidence, actuator, and lifecycle faults independently visible.
var (
	ErrAuthorityClosed   = errors.New("teleop/safety: authority closed")
	ErrAuthorityUnbound  = errors.New("teleop/safety: authority is not bound")
	ErrEvidence          = errors.New("teleop/safety: durable evidence unavailable")
	ErrEvidenceIdentity  = errors.New("teleop/safety: evidence returned no identity")
	ErrActuatorUncertain = errors.New("teleop/safety: actuator outcome uncertain")
	ErrActuatorTimeout   = errors.New("teleop/safety: actuator acknowledgement timed out")
	ErrActuatorRejected  = errors.New("teleop/safety: actuator rejected command")
	ErrAuthorityRevoked  = errors.New("teleop/safety: authority changed during command")
	ErrInvalidAuthority  = errors.New("teleop/safety: invalid authority configuration")
	ErrAuthorityClock    = errors.New("teleop/safety: authority clock unavailable")
)

// VesselCommand is an application command before it is snapshotted for
// evidence and transmission. Payload must be JSON representable. Authority
// encodes it exactly once so the evidence ledger and actuator see identical
// bytes even if the caller later mutates Payload.
type VesselCommand struct {
	Name    string
	Payload any
}

// EncodedCommand is the immutable representation shared by evidence and the
// actuator adapter.
type EncodedCommand struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Clone returns an isolated command.
func (command EncodedCommand) Clone() EncodedCommand {
	command.Payload = append(json.RawMessage(nil), command.Payload...)
	return command
}

// AuthoritySource is the controller interface required by Authority. Session
// identity makes a mistaken rebind detectable instead of allowing dead-man or
// lifecycle authority to move silently between controllers. For a permitted
// snapshot, StateMeta.Sequence must identify that snapshot's canonical
// ObservationEvent in the bound session's "input" stream; Authority uses that
// identity as mandatory causal evidence for live actuation.
type AuthoritySource interface {
	Source
	Session() teleop.SessionID
}

// EvidenceID is the durable identity assigned by an Evidence adapter.
type EvidenceID string

// Evidence is the durability seam used by Authority. Commit must not return
// success until the record has crossed the adapter's configured durability
// barrier. An in-memory queue admission is not a valid production
// implementation of this contract.
type Evidence interface {
	Commit(context.Context, EvidenceRecord) (EvidenceID, error)
}

// EvidenceFunc adapts a function to Evidence.
type EvidenceFunc func(context.Context, EvidenceRecord) (EvidenceID, error)

// Commit implements Evidence.
func (fn EvidenceFunc) Commit(ctx context.Context, record EvidenceRecord) (EvidenceID, error) {
	if fn == nil {
		return "", fmt.Errorf("%w: nil evidence function", ErrEvidence)
	}
	return fn(ctx, record.Clone())
}

// EvidenceKind identifies a step in an authority operation.
type EvidenceKind string

const (
	EvidenceLifecycleAttempt EvidenceKind = "authority.lifecycle.attempt"
	EvidenceLifecycleOutcome EvidenceKind = "authority.lifecycle.outcome"
	EvidenceDecision         EvidenceKind = "authority.decision"
	EvidenceIntent           EvidenceKind = "authority.command.intent"
	EvidenceSent             EvidenceKind = "authority.command.sent"
	EvidenceAcknowledged     EvidenceKind = "authority.command.acknowledged"
	EvidenceRejected         EvidenceKind = "authority.command.rejected"
	EvidenceTimeout          EvidenceKind = "authority.command.timeout"
	EvidenceFailure          EvidenceKind = "authority.command.failure"
)

// LifecycleAction identifies an operator or supervisor request to Authority.
type LifecycleAction string

const (
	LifecycleBind          LifecycleAction = "bind"
	LifecycleArm           LifecycleAction = "arm"
	LifecycleDisarm        LifecycleAction = "disarm"
	LifecycleEmergencyStop LifecycleAction = "emergency_stop"
	LifecycleReset         LifecycleAction = "reset"
	LifecycleClose         LifecycleAction = "close"
)

// LifecycleEvidence records both attempts and their outcomes. Accepted is
// meaningful on EvidenceLifecycleOutcome; attempts are retained even when the
// Guard refuses them.
type LifecycleEvidence struct {
	Action   LifecycleAction `json:"action"`
	Detail   string          `json:"detail,omitempty"`
	Accepted bool            `json:"accepted,omitempty"`
}

// EvidenceRecord is the implementation-neutral record committed at the
// Evidence seam. ParentID and DecisionID form the exact
// decision -> intent -> sent -> outcome chain. Causes retain controller event
// identity across that chain.
type EvidenceRecord struct {
	Kind EvidenceKind `json:"kind"`

	ControllerSession teleop.SessionID `json:"controller_session"`
	RecordSequence    uint64           `json:"record_sequence"`
	CommandSequence   uint64           `json:"command_sequence,omitempty"`
	RecordedAt        time.Time        `json:"recorded_at"`

	ParentID   EvidenceID       `json:"parent_id,omitempty"`
	DecisionID EvidenceID       `json:"decision_id,omitempty"`
	Causes     []teleop.EventID `json:"causes,omitempty"`

	Lifecycle      *LifecycleEvidence      `json:"lifecycle,omitempty"`
	Decision       *Decision               `json:"decision,omitempty"`
	Requested      *EncodedCommand         `json:"requested,omitempty"`
	Applied        *EncodedCommand         `json:"applied,omitempty"`
	Actuator       *ActuatorCommand        `json:"actuator,omitempty"`
	Acknowledgment *ActuatorAcknowledgment `json:"acknowledgment,omitempty"`
	Detail         string                  `json:"detail,omitempty"`
	Error          string                  `json:"error,omitempty"`
}

// Clone returns an isolated record suitable for retaining asynchronously.
func (record EvidenceRecord) Clone() EvidenceRecord {
	record.Causes = slices.Clone(record.Causes)
	if record.Lifecycle != nil {
		value := *record.Lifecycle
		record.Lifecycle = &value
	}
	if record.Decision != nil {
		value := cloneAuthorityDecision(*record.Decision)
		record.Decision = &value
	}
	if record.Requested != nil {
		value := record.Requested.Clone()
		record.Requested = &value
	}
	if record.Applied != nil {
		value := record.Applied.Clone()
		record.Applied = &value
	}
	if record.Actuator != nil {
		value := record.Actuator.Clone()
		record.Actuator = &value
	}
	if record.Acknowledgment != nil {
		value := *record.Acknowledgment
		record.Acknowledgment = &value
	}
	return record
}

// ActuatorCommand is the lease-bearing envelope sent to the system under
// control. An actuator must reject stale sequence numbers and cease honoring a
// command no later than ExpiresAt. TTL is included so an adapter can enforce a
// local monotonic lease rather than trusting synchronized wall clocks.
type ActuatorCommand struct {
	ControllerSession teleop.SessionID `json:"controller_session"`
	Sequence          uint64           `json:"sequence"`
	Command           EncodedCommand   `json:"command"`
	IssuedAt          time.Time        `json:"issued_at"`
	ExpiresAt         time.Time        `json:"expires_at"`
	TTL               time.Duration    `json:"ttl"`
	Fallback          bool             `json:"fallback"`
	Reason            string           `json:"reason,omitempty"`
	DecisionID        EvidenceID       `json:"decision_id,omitempty"`
	IntentID          EvidenceID       `json:"intent_id,omitempty"`
}

// Clone returns an isolated actuator command.
func (command ActuatorCommand) Clone() ActuatorCommand {
	command.Command = command.Command.Clone()
	return command
}

// ActuatorAcknowledgment is the actuator's outcome for one exact authority
// session and sequence. Accepted=false is an explicit rejection, not an
// acknowledgement of application.
type ActuatorAcknowledgment struct {
	ControllerSession teleop.SessionID `json:"controller_session"`
	Sequence          uint64           `json:"sequence"`
	Accepted          bool             `json:"accepted"`
	AppliedAt         time.Time        `json:"applied_at,omitempty"`
	Detail            string           `json:"detail,omitempty"`
}

// ActuatorReceipt waits for the outcome of a command that Send accepted. The
// split interface lets Authority durably distinguish transport acceptance from
// actuator acknowledgement.
type ActuatorReceipt interface {
	Await(context.Context) (ActuatorAcknowledgment, error)
}

// Actuator is the only output seam used by Authority. Implementations must
// honor context cancellation, enforce sequence and expiry at the receiving
// side, and make Send safe to follow with a higher-sequence fallback after an
// uncertain outcome.
type Actuator interface {
	Send(context.Context, ActuatorCommand) (ActuatorReceipt, error)
}

// ActuatorFunc adapts a function to Actuator.
type ActuatorFunc func(context.Context, ActuatorCommand) (ActuatorReceipt, error)

// Send implements Actuator.
func (fn ActuatorFunc) Send(
	ctx context.Context,
	command ActuatorCommand,
) (ActuatorReceipt, error) {
	if fn == nil {
		return nil, fmt.Errorf("%w: nil actuator function", ErrActuatorUncertain)
	}
	return fn(ctx, command.Clone())
}

// ReceiptFunc adapts a function to ActuatorReceipt.
type ReceiptFunc func(context.Context) (ActuatorAcknowledgment, error)

// Await implements ActuatorReceipt.
func (fn ReceiptFunc) Await(ctx context.Context) (ActuatorAcknowledgment, error) {
	if fn == nil {
		return ActuatorAcknowledgment{}, fmt.Errorf(
			"%w: nil actuator receipt function",
			ErrActuatorUncertain,
		)
	}
	return fn(ctx)
}

// AuthorityConfig defines the non-optional output policy. EngineeredSafeState
// is application-defined because neutral controller input is not necessarily a
// vessel's safe actuator state.
type AuthorityConfig struct {
	EngineeredSafeState VesselCommand
	CommandTTL          time.Duration
	// RequireAppliedAcknowledgment rejects an otherwise accepted response that
	// does not assert when the actuator applied the command. The timestamp is an
	// adapter claim, not independent physical-state proof; production adapters
	// must authenticate it and derive it from the receiving system.
	RequireAppliedAcknowledgment bool
	// Now is a deterministic wall-clock seam for evidence and lease envelopes.
	// Safety input freshness remains on the Guard's monotonic controller clock.
	Now func() time.Time
}

// ApplyRequest is application intent. Authority snapshots Intent, evaluates
// the Guard itself, and substitutes EngineeredSafeState whenever output is not
// authorized.
type ApplyRequest struct {
	Intent VesselCommand
	Causes []teleop.EventID
	Detail string
}

// ApplyResult describes the command Authority ultimately attempted. A nil
// error means the command was durably evidenced, sent, and positively
// acknowledged. Permit remains false when that successful command was the
// Engineered Safe State selected by an inhibiting Guard decision.
type ApplyResult struct {
	Decision             Decision
	Requested            EncodedCommand
	Applied              EncodedCommand
	Fallback             bool
	CommandSequence      uint64
	DecisionEvidenceID   EvidenceID
	IntentEvidenceID     EvidenceID
	OutcomeEvidenceID    EvidenceID
	Acknowledgment       *ActuatorAcknowledgment
	FallbackSequence     uint64
	FallbackAcknowledged bool
}

// Authority owns the only supported sequence from safety decision to durable
// intent, actuator lease, acknowledgement, and evidence outcome. Its public
// interface intentionally does not expose Guard.Evaluate or Heartbeat.
type Authority struct {
	guard                        *Guard
	evidence                     Evidence
	actuator                     Actuator
	safe                         EncodedCommand
	ttl                          time.Duration
	now                          func() time.Time
	requireAppliedAcknowledgment bool

	processor *authorityProcessor
	serial    chan struct{}

	clockCall     chan struct{}
	evidenceCall  chan struct{}
	liveSendCall  chan struct{}
	safeSendCall  chan struct{}
	liveAwaitCall chan struct{}
	safeAwaitCall chan struct{}

	stateMu      sync.Mutex
	source       AuthoritySource
	session      teleop.SessionID
	bound        bool
	closed       bool
	active       uint64
	activeID     uint64
	activeLive   bool
	activeInput  Decision
	activeCancel context.CancelFunc

	recordSequence  uint64 // serial protects the evidence order.
	commandSequence uint64 // serial protects actuator order.
	safetyEpoch     uint64 // stateMu protects later safety requests from earlier grants.
}

// NewAuthority returns a fail-closed authority with an internally owned Guard.
// The caller attaches Processor while opening the controller, then calls Bind.
func NewAuthority(
	config AuthorityConfig,
	evidence Evidence,
	actuator Actuator,
	guardOptions ...Option,
) (*Authority, error) {
	return NewAuthorityWithGuard(config, New(guardOptions...), evidence, actuator)
}

// NewAuthorityWithGuard returns an Authority around an already validated
// Guard. Production sessions use this constructor with NewMaritime so strict
// profile validation remains the Guard module's single source of truth.
func NewAuthorityWithGuard(
	config AuthorityConfig,
	guard *Guard,
	evidence Evidence,
	actuator Actuator,
) (*Authority, error) {
	if guard == nil {
		return nil, fmt.Errorf("%w: nil guard", ErrInvalidAuthority)
	}
	if evidence == nil {
		return nil, fmt.Errorf("%w: nil evidence adapter", ErrInvalidAuthority)
	}
	if actuator == nil {
		return nil, fmt.Errorf("%w: nil actuator adapter", ErrInvalidAuthority)
	}
	if config.CommandTTL <= 0 {
		return nil, fmt.Errorf("%w: command TTL must be positive", ErrInvalidAuthority)
	}
	safe, err := encodeVesselCommand(config.EngineeredSafeState)
	if err != nil {
		return nil, errors.Join(
			ErrInvalidAuthority,
			fmt.Errorf("engineered safe state: %w", err),
		)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	authority := &Authority{
		guard:                        guard,
		evidence:                     evidence,
		actuator:                     actuator,
		safe:                         safe,
		ttl:                          config.CommandTTL,
		now:                          now,
		requireAppliedAcknowledgment: config.RequireAppliedAcknowledgment,
		serial:                       make(chan struct{}, 1),
		clockCall:                    newCallbackGate(),
		evidenceCall:                 newCallbackGate(),
		liveSendCall:                 newCallbackGate(),
		safeSendCall:                 newCallbackGate(),
		liveAwaitCall:                newCallbackGate(),
		safeAwaitCall:                newCallbackGate(),
	}
	authority.serial <- struct{}{}
	authority.processor = &authorityProcessor{authority: authority}
	return authority, nil
}

// Processor returns Authority's private Guard adapter. Attach this exact value
// with teleop.WithProcessor before Bind. The concrete adapter deliberately does
// not expose the Guard for direct evaluation or lifecycle mutation.
func (authority *Authority) Processor() teleop.Processor {
	return authority.processor
}

// Session returns the bound controller session, or zero before Bind.
func (authority *Authority) Session() teleop.SessionID {
	authority.stateMu.Lock()
	defer authority.stateMu.Unlock()
	return authority.session
}

// Bind attaches Authority to exactly one controller session. Rebinding is
// rejected so dead-man edge history and authority cannot cross sessions.
func (authority *Authority) Bind(ctx context.Context, source AuthoritySource) error {
	if err := validContext(ctx); err != nil {
		return err
	}
	if source == nil {
		return fmt.Errorf("%w: nil source", ErrAuthorityUnbound)
	}
	session := source.Session()
	if session == (teleop.SessionID{}) {
		return fmt.Errorf("%w: source has zero session", ErrAuthorityUnbound)
	}
	if err := authority.acquire(ctx); err != nil {
		return err
	}
	defer authority.release()

	authority.stateMu.Lock()
	if authority.closed {
		authority.stateMu.Unlock()
		return ErrAuthorityClosed
	}
	if authority.bound {
		authority.stateMu.Unlock()
		return fmt.Errorf("%w: authority is already bound", ErrInvalidAuthority)
	}
	authority.session = session
	authority.stateMu.Unlock()

	attempt, attemptErr := authority.commit(ctx, EvidenceRecord{
		Kind: EvidenceLifecycleAttempt,
		Lifecycle: &LifecycleEvidence{
			Action: LifecycleBind,
			Detail: session.String(),
		},
	})
	if attemptErr != nil {
		authority.stateMu.Lock()
		authority.session = teleop.SessionID{}
		authority.stateMu.Unlock()
		return attemptErr
	}

	authority.stateMu.Lock()
	bindErr := authority.guard.Bind(source)
	if bindErr == nil {
		authority.source = source
		authority.bound = true
	}
	authority.stateMu.Unlock()

	_, outcomeErr := authority.commit(ctx, EvidenceRecord{
		Kind:     EvidenceLifecycleOutcome,
		ParentID: attempt,
		Lifecycle: &LifecycleEvidence{
			Action:   LifecycleBind,
			Detail:   session.String(),
			Accepted: bindErr == nil,
		},
		Error: errorText(bindErr),
	})
	if bindErr != nil {
		authority.stateMu.Lock()
		authority.session = teleop.SessionID{}
		authority.stateMu.Unlock()
	}
	if bindErr != nil {
		return errors.Join(bindErr, outcomeErr)
	}
	if outcomeErr != nil {
		authority.terminateBound("bind outcome evidence unavailable")
		_, fallbackErr := authority.executeFallbackLocked(
			ctx,
			authority.guard.Evaluate(),
			nil,
			"bind outcome evidence unavailable",
		)
		return errors.Join(outcomeErr, fallbackErr)
	}

	// Binding finishes with a fully evidenced and acknowledged safe-state
	// transaction. Besides establishing the actuator's initial lease, this is
	// the first evidence that the Authority loop is alive, so it is the earliest
	// point at which a strict Guard heartbeat may be seeded.
	decision := authority.guard.Evaluate()
	result, fallbackErr := authority.executeFallbackLocked(
		ctx,
		decision,
		nil,
		"bind bootstrap",
	)
	if fallbackErr == nil && result.Acknowledgment != nil && result.Acknowledgment.Accepted {
		authority.stateMu.Lock()
		authority.guard.Heartbeat()
		authority.stateMu.Unlock()
	}
	if fallbackErr != nil {
		authority.terminateBound("bind bootstrap safe state failed")
	}
	return fallbackErr
}

// Arm records every attempt and outcome. Evidence failure cannot grant
// authority; if outcome recording fails after arming, Authority disarms and
// synchronously attempts the Engineered Safe State.
func (authority *Authority) Arm(ctx context.Context, detail string) error {
	return authority.permissiveLifecycle(ctx, LifecycleArm, detail, func() error {
		return authority.guard.Arm()
	})
}

// Reset records every attempt and outcome. A reset that cannot be evidenced is
// converted back into an emergency stop.
func (authority *Authority) Reset(ctx context.Context, detail string) error {
	return authority.permissiveLifecycle(ctx, LifecycleReset, detail, func() error {
		authority.guard.Reset(detail)
		return nil
	})
}

// Disarm revokes authority immediately, cancels an in-flight command, records
// the lifecycle request, and synchronously attempts the Engineered Safe State.
// The safe-state attempt still occurs when evidence is unavailable.
func (authority *Authority) Disarm(ctx context.Context, detail string) error {
	return authority.safeLifecycle(ctx, LifecycleDisarm, detail, false)
}

// EmergencyStop latches the Guard immediately, cancels an in-flight command,
// records every request (including repeats), and synchronously attempts the
// Engineered Safe State. It complements rather than replaces a hardware stop.
func (authority *Authority) EmergencyStop(ctx context.Context, detail string) error {
	return authority.safeLifecycle(ctx, LifecycleEmergencyStop, detail, false)
}

// Close permanently rejects requested commands, cancels an in-flight command,
// disarms the Guard, and serializes a final Engineered Safe State attempt. A
// repeated Close retries that safe-state command, which is useful after a
// transient actuator fault.
func (authority *Authority) Close(ctx context.Context) error {
	return authority.safeLifecycle(ctx, LifecycleClose, "authority shutdown", true)
}

// Apply is the sole safe command-application path. It owns Guard evaluation,
// durable decision and intent recording, actuator sequencing and expiry,
// acknowledgement, outcome evidence, fallback, and loop heartbeat.
func (authority *Authority) Apply(
	ctx context.Context,
	request ApplyRequest,
) (ApplyResult, error) {
	if err := validContext(ctx); err != nil {
		return ApplyResult{}, err
	}
	requested, err := encodeVesselCommand(request.Intent)
	if err != nil {
		failureDetail := boundedFailureDetail(
			fmt.Sprintf("invalid command intent %q", request.Intent.Name),
			err,
		)
		if acquireErr := authority.acquireSafety(ctx); acquireErr != nil {
			return ApplyResult{}, errors.Join(err, acquireErr)
		}
		defer authority.release()
		// Invalid input must not become a back door around lifecycle state. In
		// particular, Close is terminal: even a malformed later request may not
		// cause another actuator transmission.
		if readyErr := authority.requireReady(); readyErr != nil {
			return ApplyResult{}, errors.Join(err, readyErr)
		}
		authority.inhibit(failureDetail)
		fallback, fallbackErr := authority.executeFallbackLocked(
			ctx,
			Decision{State: StateSafe, Reasons: []Reason{ReasonOperator}},
			request.Causes,
			failureDetail,
		)
		return fallback, errors.Join(err, fallbackErr)
	}
	request.Causes = slices.Clone(request.Causes)

	if err := authority.acquire(ctx); err != nil {
		return ApplyResult{Requested: requested}, err
	}
	defer authority.release()

	opCtx, cancel, decision, activeID, err := authority.beginApply(ctx)
	if err != nil {
		return ApplyResult{Requested: requested}, err
	}
	defer func() {
		cancel()
		authority.endApply(activeID)
	}()
	request.Causes = authority.withDecisionInputCause(request.Causes, decision)

	decisionID, decisionErr := authority.recordDecision(opCtx, decision, request.Causes, request.Detail)
	if decisionErr != nil {
		authority.markActiveFallback(activeID)
		authority.inhibit("decision evidence unavailable")
		fallback, fallbackErr := authority.executeFallbackLocked(
			ctx,
			decision,
			request.Causes,
			"decision evidence unavailable",
		)
		fallback.Requested = requested
		return fallback, errors.Join(decisionErr, fallbackErr)
	}

	selected := requested
	fallback := !decision.Permit
	reason := request.Detail
	if fallback {
		selected = authority.safe.Clone()
		reason = decisionReason(decision)
	}
	result, applyErr := authority.executeLocked(
		opCtx,
		requested,
		selected,
		decision,
		decisionID,
		request.Causes,
		fallback,
		reason,
	)
	if applyErr != nil {
		if fallback {
			authority.inhibit("engineered safe state failed")
			return result, applyErr
		}
		authority.markActiveFallback(activeID)
		failureReason := "requested command outcome uncertain"
		fallbackDecision := decision
		fallbackCauses := request.Causes
		authority.stateMu.Lock()
		current := authority.guard.Evaluate()
		authority.stateMu.Unlock()
		if errors.Is(applyErr, ErrAuthorityRevoked) ||
			!sameDecisionInput(decision, current) {
			applyErr = errors.Join(applyErr, ErrAuthorityRevoked)
			failureReason = "authority revoked before actuator send"
			fallbackDecision = current
			fallbackCauses = authority.withDecisionInputCause(fallbackCauses, current)
		}
		authority.inhibit(failureReason)
		safeResult, safeErr := authority.executeFallbackLocked(
			ctx,
			fallbackDecision,
			fallbackCauses,
			failureReason,
		)
		result.FallbackSequence = safeResult.CommandSequence
		result.FallbackAcknowledged = safeResult.Acknowledgment != nil &&
			safeResult.Acknowledgment.Accepted
		return result, errors.Join(applyErr, safeErr)
	}

	if fallback {
		// A successfully acknowledged safe command establishes that the control loop
		// completed the configured adapter transaction, even though operator output
		// remains inhibited. It does not establish the actuator's physical state.
		authority.stateMu.Lock()
		authority.guard.Heartbeat()
		authority.stateMu.Unlock()
		return result, nil
	}

	// Serialize the final authority check with EmergencyStop. Either the check
	// and heartbeat happen first, after which the stop sends a newer fallback,
	// or the stop happens first and this command is immediately superseded.
	authority.stateMu.Lock()
	current := authority.guard.Evaluate()
	if sameDecisionInput(decision, current) {
		authority.guard.Heartbeat()
		authority.stateMu.Unlock()
		return result, nil
	}
	authority.stateMu.Unlock()

	revokedCtx, cancelRevoked := authority.safetyContext(ctx)
	defer cancelRevoked()
	authority.markActiveFallback(activeID)
	revokedCauses := authority.withDecisionInputCause(request.Causes, current)
	revokedID, revokedErr := authority.recordDecision(
		revokedCtx,
		current,
		revokedCauses,
		"authority changed after actuator acknowledgement",
	)
	_ = revokedID
	safeResult, safeErr := authority.executeFallbackLocked(
		ctx,
		current,
		revokedCauses,
		"authority changed after actuator acknowledgement",
	)
	result.FallbackSequence = safeResult.CommandSequence
	result.FallbackAcknowledged = safeResult.Acknowledgment != nil &&
		safeResult.Acknowledgment.Accepted
	return result, errors.Join(ErrAuthorityRevoked, revokedErr, safeErr)
}

func (authority *Authority) permissiveLifecycle(
	ctx context.Context,
	action LifecycleAction,
	detail string,
	apply func() error,
) error {
	if err := validContext(ctx); err != nil {
		return err
	}
	// Capture before waiting for serial. Any Disarm, Close, or EmergencyStop
	// that linearizes after this operation began must supersede its later grant.
	authority.stateMu.Lock()
	startedSafetyEpoch := authority.safetyEpoch
	authority.stateMu.Unlock()
	if err := authority.acquire(ctx); err != nil {
		return err
	}
	defer authority.release()
	if err := authority.requireReady(); err != nil {
		return err
	}

	attempt, attemptErr := authority.commit(ctx, EvidenceRecord{
		Kind: EvidenceLifecycleAttempt,
		Lifecycle: &LifecycleEvidence{
			Action: action,
			Detail: detail,
		},
	})
	if attemptErr != nil {
		authority.inhibit("lifecycle evidence unavailable")
		_, fallbackErr := authority.executeFallbackLocked(
			ctx,
			authority.guard.Evaluate(),
			nil,
			"lifecycle evidence unavailable",
		)
		return errors.Join(attemptErr, fallbackErr)
	}

	var readinessErr error
	if action == LifecycleArm && authority.guard.options.loopTimeout > 0 {
		// An Arm may follow a slow external witness round trip, so the Bind
		// heartbeat is not necessarily current. Exercise the whole configured safe
		// command adapter path immediately before arming instead of exposing a
		// free-standing Heartbeat
		// that a separate goroutine could use to defeat the loop watchdog.
		decision := authority.guard.Evaluate()
		result, err := authority.executeFallbackLocked(
			ctx,
			decision,
			nil,
			"arm watchdog readiness preflight",
		)
		readinessErr = err
		if err == nil && result.Acknowledgment != nil && result.Acknowledgment.Accepted {
			authority.stateMu.Lock()
			authority.guard.Heartbeat()
			authority.stateMu.Unlock()
		} else if err == nil {
			readinessErr = fmt.Errorf(
				"%w: arm readiness fallback was not acknowledged",
				ErrActuatorUncertain,
			)
		}
	}

	authority.stateMu.Lock()
	actionErr := readinessErr
	if actionErr == nil && authority.safetyEpoch != startedSafetyEpoch {
		actionErr = fmt.Errorf(
			"%w: lifecycle %s was superseded by a safety action",
			ErrAuthorityRevoked,
			action,
		)
	}
	if actionErr == nil {
		actionErr = apply()
	}
	if action == LifecycleArm && actionErr != nil {
		// Revoke inherited authority before waiting on outcome evidence.
		authority.guard.Disarm("arm attempt failed")
	}
	authority.stateMu.Unlock()
	outcome := &LifecycleEvidence{Action: action, Detail: detail, Accepted: actionErr == nil}
	_, outcomeErr := authority.commit(ctx, EvidenceRecord{
		Kind:      EvidenceLifecycleOutcome,
		ParentID:  attempt,
		Lifecycle: outcome,
		Error:     errorText(actionErr),
	})
	if action == LifecycleArm && actionErr != nil {
		// A refused re-Arm must not leave authority inherited from an earlier
		// successful Arm. Revoke first, then make a new higher-sequence safe-state
		// attempt even when the readiness preflight was itself uncertain.
		_, fallbackErr := authority.executeFallbackLocked(
			ctx,
			authority.guard.Evaluate(),
			nil,
			"arm attempt failed",
		)
		return errors.Join(actionErr, outcomeErr, fallbackErr)
	}
	if outcomeErr != nil && actionErr == nil {
		// Granting or clearing authority without its outcome evidence is not a
		// successful lifecycle operation.
		authority.stateMu.Lock()
		if action == LifecycleReset {
			authority.guard.EmergencyStop("reset outcome evidence unavailable")
		} else {
			authority.guard.Disarm("arm outcome evidence unavailable")
		}
		authority.stateMu.Unlock()
		_, fallbackErr := authority.executeFallbackLocked(
			ctx,
			authority.guard.Evaluate(),
			nil,
			"lifecycle outcome evidence unavailable",
		)
		return errors.Join(outcomeErr, fallbackErr)
	}
	if action == LifecycleArm && actionErr == nil && outcomeErr == nil {
		// Publish the freshest possible loop evidence only after the accepted Arm
		// outcome itself crossed the evidence barrier. A caller must begin its
		// continuous Apply loop before this watchdog interval elapses.
		authority.stateMu.Lock()
		authority.guard.Heartbeat()
		authority.stateMu.Unlock()
	}
	return errors.Join(actionErr, outcomeErr)
}

func (authority *Authority) safeLifecycle(
	ctx context.Context,
	action LifecycleAction,
	detail string,
	closeAuthority bool,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Safety tightening happens before consulting external callbacks or waiting
	// for the serialization token. A panicking clock must not suppress a stop.
	// This cancels a live Apply while the lifecycle call waits to issue the
	// higher-sequence Engineered Safe State.
	authority.stateMu.Lock()
	authority.safetyEpoch++
	if closeAuthority {
		authority.closed = true
	}
	switch action {
	case LifecycleEmergencyStop:
		authority.guard.EmergencyStop(detail)
	default:
		authority.guard.Disarm(detail)
	}
	if authority.activeCancel != nil {
		authority.activeCancel()
	}
	authority.stateMu.Unlock()
	clockCtx, cancelClock := authority.safetyContext(ctx)
	occurredAt, occurredErr := authority.currentTime(clockCtx)
	cancelClock()
	if occurredErr != nil {
		// The configured clock is evidence metadata, not permission to suppress an
		// emergency fallback. Use a bounded system-clock lease and report the fault.
		occurredAt = time.Now().UTC()
	}

	if err := authority.acquireSafety(ctx); err != nil {
		return err
	}
	defer authority.release()
	// A permissive lifecycle operation may already have held serial when this
	// later safety action first tightened the Guard. Reassert under serial so an
	// earlier Arm or Reset cannot overwrite a later Disarm, Close, or E-stop
	// while the safety action waits its turn.
	authority.stateMu.Lock()
	if closeAuthority {
		authority.closed = true
	}
	switch action {
	case LifecycleEmergencyStop:
		authority.guard.EmergencyStop(detail)
	default:
		authority.guard.Disarm(detail)
	}
	if authority.activeCancel != nil {
		authority.activeCancel()
	}
	authority.stateMu.Unlock()

	safeCtx, cancelSafe := authority.safetyContext(ctx)
	defer cancelSafe()
	attempt, attemptErr := authority.commit(safeCtx, EvidenceRecord{
		Kind:       EvidenceLifecycleAttempt,
		RecordedAt: occurredAt,
		Lifecycle: &LifecycleEvidence{
			Action: action,
			Detail: detail,
		},
	})
	attemptErr = errors.Join(occurredErr, attemptErr)
	outcome, outcomeErr := authority.commit(safeCtx, EvidenceRecord{
		Kind:     EvidenceLifecycleOutcome,
		ParentID: attempt,
		Lifecycle: &LifecycleEvidence{
			Action:   action,
			Detail:   detail,
			Accepted: true,
		},
	})
	_ = outcome

	decision := authority.guard.Evaluate()
	_, fallbackErr := authority.executeFallbackLocked(
		safeCtx,
		decision,
		nil,
		string(action),
	)
	return errors.Join(attemptErr, outcomeErr, fallbackErr)
}

func (authority *Authority) beginApply(
	ctx context.Context,
) (context.Context, context.CancelFunc, Decision, uint64, error) {
	authority.stateMu.Lock()
	defer authority.stateMu.Unlock()
	if authority.closed {
		return nil, nil, Decision{}, 0, ErrAuthorityClosed
	}
	if !authority.bound || authority.source == nil {
		return nil, nil, Decision{}, 0, ErrAuthorityUnbound
	}
	opCtx, cancel := authority.commandContext(ctx)
	authority.activeID++
	id := authority.activeID
	decision := authority.guard.Evaluate()
	authority.active = id
	authority.activeLive = decision.Permit
	authority.activeInput = cloneAuthorityDecision(decision)
	authority.activeCancel = cancel
	return opCtx, cancel, decision, id, nil
}

func (authority *Authority) endApply(id uint64) {
	authority.stateMu.Lock()
	if authority.active == id {
		authority.active = 0
		authority.activeLive = false
		authority.activeInput = Decision{}
		authority.activeCancel = nil
	}
	authority.stateMu.Unlock()
}

func (authority *Authority) markActiveFallback(id uint64) {
	authority.stateMu.Lock()
	if authority.active == id {
		authority.activeLive = false
		authority.activeInput = Decision{}
	}
	authority.stateMu.Unlock()
}

func (authority *Authority) cancelLiveApplyIfDecisionChanged() {
	authority.stateMu.Lock()
	defer authority.stateMu.Unlock()
	if !authority.activeLive || authority.activeCancel == nil {
		return
	}
	if sameDecisionInput(authority.activeInput, authority.guard.Evaluate()) {
		return
	}
	// Cancel only the requested-command phase. Apply switches to an independent
	// safety context before its higher-sequence fallback, so an input fault or a
	// newer observation can never cancel the safe command it caused.
	authority.activeLive = false
	authority.activeInput = Decision{}
	authority.activeCancel()
}

func (authority *Authority) executeFallbackLocked(
	ctx context.Context,
	decision Decision,
	causes []teleop.EventID,
	reason string,
) (ApplyResult, error) {
	// Evidence and physical actuation deliberately receive independent budgets.
	// A journal adapter that consumes (or ignores) its deadline must never spend
	// the only lease window available for the higher-sequence safe command.
	decisionCtx, cancelDecision := authority.safetyContext(ctx)
	decisionID, decisionErr := authority.recordDecision(
		decisionCtx,
		decision,
		causes,
		reason,
	)
	cancelDecision()
	executionCtx, cancelExecution := authority.safetyContext(ctx)
	defer cancelExecution()
	result, applyErr := authority.executeLocked(
		executionCtx,
		authority.safe,
		authority.safe,
		decision,
		decisionID,
		causes,
		true,
		reason,
	)
	return result, errors.Join(decisionErr, applyErr)
}

func (authority *Authority) executeLocked(
	ctx context.Context,
	requested EncodedCommand,
	applied EncodedCommand,
	decision Decision,
	decisionID EvidenceID,
	causes []teleop.EventID,
	fallback bool,
	reason string,
) (ApplyResult, error) {
	authority.commandSequence++
	sequence := authority.commandSequence
	result := ApplyResult{
		Decision:           cloneAuthorityDecision(decision),
		Requested:          requested.Clone(),
		Applied:            applied.Clone(),
		Fallback:           fallback,
		CommandSequence:    sequence,
		DecisionEvidenceID: decisionID,
	}

	intentCtx := ctx
	var cancelIntent context.CancelFunc
	if fallback {
		intentCtx, cancelIntent = authority.safetyContext(ctx)
	}
	intentID, intentErr := authority.commit(intentCtx, EvidenceRecord{
		Kind:            EvidenceIntent,
		CommandSequence: sequence,
		ParentID:        decisionID,
		DecisionID:      decisionID,
		Causes:          causes,
		Requested:       commandPointer(requested),
		Applied:         commandPointer(applied),
		Detail:          reason,
	})
	if cancelIntent != nil {
		cancelIntent()
	}
	result.IntentEvidenceID = intentID
	if intentErr != nil && !fallback {
		return result, intentErr
	}
	if !fallback {
		// Intent durability is a FIFO barrier behind every earlier canonical input
		// event. Re-evaluate after that barrier, immediately before entering the
		// actuator boundary, so a fault processed while evidence was committing
		// cannot inherit the earlier permit decision.
		authority.stateMu.Lock()
		current := authority.guard.Evaluate()
		authority.stateMu.Unlock()
		if !sameDecisionInput(decision, current) {
			result.Decision = cloneAuthorityDecision(current)
			return result, ErrAuthorityRevoked
		}
	}

	clockCtx := ctx
	var cancelClock context.CancelFunc
	if fallback {
		clockCtx, cancelClock = authority.safetyContext(ctx)
	}
	issuedAt, clockErr := authority.currentTime(clockCtx)
	if cancelClock != nil {
		cancelClock()
	}
	if clockErr != nil {
		if !fallback {
			return result, errors.Join(intentErr, clockErr)
		}
		// A clock callback is outside the trusted safety core. Its failure cannot
		// prevent a best-effort Engineered Safe State with a bounded lease.
		issuedAt = time.Now().UTC()
		intentErr = errors.Join(intentErr, clockErr)
	}
	if fallback {
		// The wall-clock callback is another external seam. Only after it returns
		// (or times out and falls back to the system clock) do we start the budget
		// that bounds physical Send and Await.
		actuationCtx, cancelActuation := authority.safetyContext(ctx)
		defer cancelActuation()
		ctx = actuationCtx
	}
	expiresAt := issuedAt.Add(authority.ttl)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expiresAt) {
		expiresAt = deadline.UTC()
	}
	envelope := ActuatorCommand{
		ControllerSession: authority.session,
		Sequence:          sequence,
		Command:           applied.Clone(),
		IssuedAt:          issuedAt,
		ExpiresAt:         expiresAt,
		TTL:               max(expiresAt.Sub(issuedAt), 0),
		Fallback:          fallback,
		Reason:            reason,
		DecisionID:        decisionID,
		IntentID:          intentID,
	}
	if envelope.TTL <= 0 {
		err := fmt.Errorf("%w: command lease expired before send", ErrActuatorTimeout)
		recordErr := authority.recordActuatorError(
			ctx,
			envelope,
			intentID,
			decisionID,
			causes,
			err,
		)
		return result, errors.Join(intentErr, err, recordErr)
	}
	if !fallback {
		// The configured clock is also an external callback and may have consumed
		// most of the command budget. Check once more at the last in-process point
		// before Send. A revocation after this boundary is handled by cancellation,
		// receiver sequence/expiry, and a newer fallback; software cannot make the
		// transport call and an asynchronous stop physically atomic.
		authority.stateMu.Lock()
		current := authority.guard.Evaluate()
		authority.stateMu.Unlock()
		if !sameDecisionInput(decision, current) {
			result.Decision = cloneAuthorityDecision(current)
			return result, ErrAuthorityRevoked
		}
	}

	sendCall := authority.liveSendCall
	awaitCall := authority.liveAwaitCall
	if fallback {
		sendCall = authority.safeSendCall
		awaitCall = authority.safeAwaitCall
	}
	receipt, sendErr := callActuatorSend(authority.actuator, sendCall, ctx, envelope)
	if sendErr != nil || receipt == nil {
		if sendErr == nil {
			sendErr = fmt.Errorf("%w: actuator returned nil receipt", ErrActuatorUncertain)
		}
		classified := classifyActuatorError(ctx, sendErr)
		recordErr := authority.recordActuatorError(
			ctx,
			envelope,
			intentID,
			decisionID,
			causes,
			sendErr,
		)
		return result, errors.Join(intentErr, classified, recordErr)
	}

	sentRecord := EvidenceRecord{
		Kind:            EvidenceSent,
		CommandSequence: sequence,
		// A fallback waits for the receiver before committing this record so
		// evidence latency cannot consume its lease. Preserve the actual issuance
		// chronology instead of letting commit time look like transmission time.
		RecordedAt: envelope.IssuedAt,
		ParentID:   intentID,
		DecisionID: decisionID,
		Causes:     causes,
		Requested:  commandPointer(requested),
		Applied:    commandPointer(applied),
		Actuator:   actuatorCommandPointer(envelope),
	}

	var (
		ack     ActuatorAcknowledgment
		ackErr  error
		sentID  EvidenceID
		sentErr error
	)
	await := func() {
		ack, ackErr = callActuatorAwait(receipt, awaitCall, ctx)
		if ackErr == nil {
			validationTime, clockErr := authority.currentTime(ctx)
			if clockErr != nil {
				ackErr = clockErr
			} else {
				ackErr = validateAcknowledgment(
					envelope,
					ack,
					validationTime,
					authority.requireAppliedAcknowledgment,
				)
			}
		}
	}
	if fallback {
		// Once a safe command has crossed Send, wait for its receipt before an
		// evidence callback can consume the remainder of its receiver lease.
		await()
	}
	sentCtx := ctx
	var cancelSent context.CancelFunc
	if fallback {
		sentCtx, cancelSent = authority.safetyContext(ctx)
	}
	sentID, sentErr = authority.commit(sentCtx, sentRecord)
	if cancelSent != nil {
		cancelSent()
	}
	if sentErr != nil && !fallback {
		return result, errors.Join(intentErr, sentErr)
	}
	if !fallback {
		await()
	}
	if ackErr != nil {
		classified := classifyActuatorError(ctx, ackErr)
		recordErr := authority.recordActuatorError(
			ctx,
			envelope,
			sentID,
			decisionID,
			causes,
			ackErr,
		)
		return result, errors.Join(intentErr, sentErr, classified, recordErr)
	}
	result.Acknowledgment = &ack

	kind := EvidenceAcknowledged
	var outcomeErr error
	if !ack.Accepted {
		kind = EvidenceRejected
		outcomeErr = fmt.Errorf("%w: %s", ErrActuatorRejected, ack.Detail)
	}
	outcomeCtx := ctx
	var cancelOutcome context.CancelFunc
	if fallback {
		outcomeCtx, cancelOutcome = authority.safetyContext(ctx)
	}
	outcomeID, evidenceErr := authority.commit(outcomeCtx, EvidenceRecord{
		Kind:            kind,
		CommandSequence: sequence,
		ParentID:        sentID,
		DecisionID:      decisionID,
		Causes:          causes,
		Requested:       commandPointer(requested),
		Applied:         commandPointer(applied),
		Actuator:        actuatorCommandPointer(envelope),
		Acknowledgment:  acknowledgmentPointer(ack),
		Error:           errorText(outcomeErr),
	})
	if cancelOutcome != nil {
		cancelOutcome()
	}
	result.OutcomeEvidenceID = outcomeID
	return result, errors.Join(intentErr, sentErr, outcomeErr, evidenceErr)
}

func (authority *Authority) recordActuatorError(
	ctx context.Context,
	command ActuatorCommand,
	parent EvidenceID,
	decision EvidenceID,
	causes []teleop.EventID,
	err error,
) error {
	kind := EvidenceFailure
	classified := classifyActuatorError(ctx, err)
	if errors.Is(classified, ErrActuatorTimeout) {
		kind = EvidenceTimeout
	}
	safeCtx, cancelSafe := authority.safetyContext(ctx)
	defer cancelSafe()
	_, recordErr := authority.commit(safeCtx, EvidenceRecord{
		Kind:            kind,
		CommandSequence: command.Sequence,
		ParentID:        parent,
		DecisionID:      decision,
		Causes:          causes,
		Applied:         commandPointer(command.Command),
		Actuator:        actuatorCommandPointer(command),
		Error:           errorText(classified),
	})
	return recordErr
}

func (authority *Authority) recordDecision(
	ctx context.Context,
	decision Decision,
	causes []teleop.EventID,
	detail string,
) (EvidenceID, error) {
	value := cloneAuthorityDecision(decision)
	return authority.commit(ctx, EvidenceRecord{
		Kind:     EvidenceDecision,
		Causes:   causes,
		Decision: &value,
		Detail:   detail,
	})
}

func (authority *Authority) commit(
	ctx context.Context,
	record EvidenceRecord,
) (EvidenceID, error) {
	commitCtx, cancelCommit := context.WithTimeout(ctx, authority.ttl)
	defer cancelCommit()
	authority.recordSequence++
	record.RecordSequence = authority.recordSequence
	record.ControllerSession = authority.session
	if record.RecordedAt.IsZero() {
		recordedAt, err := authority.currentTime(commitCtx)
		if err != nil {
			return "", errors.Join(ErrEvidence, err)
		}
		record.RecordedAt = recordedAt
	}
	id, err := callEvidenceCommit(
		authority.evidence,
		authority.evidenceCall,
		commitCtx,
		record.Clone(),
	)
	if err != nil {
		return "", errors.Join(ErrEvidence, err)
	}
	if id == "" {
		return "", ErrEvidenceIdentity
	}
	return id, nil
}

func (authority *Authority) inhibit(detail string) {
	authority.stateMu.Lock()
	authority.guard.Disarm(detail)
	authority.stateMu.Unlock()
}

// terminateBound makes a partially initialized bound Authority permanently
// unusable after Bind has crossed the Guard boundary but cannot establish its
// evidenced safe bootstrap. Keeping the session binding permits Close to issue
// another session-bound fallback, while closed rejects every later grant path.
func (authority *Authority) terminateBound(detail string) {
	authority.stateMu.Lock()
	authority.closed = true
	authority.guard.EmergencyStop(detail)
	if authority.activeCancel != nil {
		authority.activeCancel()
	}
	authority.stateMu.Unlock()
}

func (authority *Authority) requireReady() error {
	authority.stateMu.Lock()
	defer authority.stateMu.Unlock()
	if authority.closed {
		return ErrAuthorityClosed
	}
	if !authority.bound || authority.source == nil {
		return ErrAuthorityUnbound
	}
	return nil
}

func (authority *Authority) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-authority.serial:
		return nil
	}
}

func (authority *Authority) acquireSafety(ctx context.Context) error {
	safeCtx, cancelSafe := authority.safetyContext(ctx)
	defer cancelSafe()
	return authority.acquire(safeCtx)
}

func (authority *Authority) release() { authority.serial <- struct{}{} }

func (authority *Authority) commandContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, authority.ttl)
}

// safetyContext intentionally outlives caller cancellation for at most one
// command TTL. A canceled caller must not suppress a disarm, emergency stop, or
// uncertainty fallback. Context values are retained for actuator adapters.
func (authority *Authority) safetyContext(
	ctx context.Context,
) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	base := context.WithoutCancel(ctx)
	return context.WithTimeout(base, authority.ttl)
}

func encodeVesselCommand(command VesselCommand) (encoded EncodedCommand, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			encoded = EncodedCommand{}
			err = errors.Join(
				ErrInvalidAuthority,
				teleop.ErrCallbackPanic,
				fmt.Errorf("encode command %q payload panic: %v", command.Name, recovered),
			)
		}
	}()
	if command.Name == "" {
		return EncodedCommand{}, fmt.Errorf("%w: command name is empty", ErrInvalidAuthority)
	}
	var payload json.RawMessage
	if command.Payload != nil {
		encoded, err := json.Marshal(command.Payload)
		if err != nil {
			return EncodedCommand{}, fmt.Errorf("encode command %q: %w", command.Name, err)
		}
		payload = encoded
	}
	return EncodedCommand{Name: command.Name, Payload: payload}, nil
}

func cloneAuthorityDecision(decision Decision) Decision {
	decision.Reasons = slices.Clone(decision.Reasons)
	decision.Command = decision.Command.Clone()
	return decision
}

func sameDecisionInput(expected, current Decision) bool {
	return current.Permit &&
		current.InputSequence == expected.InputSequence &&
		reflect.DeepEqual(current.Command, expected.Command)
}

func (authority *Authority) withDecisionInputCause(
	causes []teleop.EventID,
	decision Decision,
) []teleop.EventID {
	if !decision.Permit || decision.InputSequence == 0 {
		return causes
	}
	authority.stateMu.Lock()
	session := authority.session
	authority.stateMu.Unlock()
	cause := teleop.EventID{
		Session:  session,
		Stream:   "input",
		Sequence: decision.InputSequence,
	}
	for _, existing := range causes {
		if existing == cause {
			return causes
		}
	}
	return append(causes, cause)
}

func commandPointer(command EncodedCommand) *EncodedCommand {
	value := command.Clone()
	return &value
}

func actuatorCommandPointer(command ActuatorCommand) *ActuatorCommand {
	value := command.Clone()
	return &value
}

func acknowledgmentPointer(ack ActuatorAcknowledgment) *ActuatorAcknowledgment {
	value := ack
	return &value
}

func decisionReason(decision Decision) string {
	if len(decision.Reasons) == 0 {
		return "engineered safe state"
	}
	encoded, _ := json.Marshal(decision.Reasons)
	return string(encoded)
}

func classifyActuatorError(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) ||
		(ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return errors.Join(ErrActuatorTimeout, err)
	}
	return errors.Join(ErrActuatorUncertain, err)
}

func validateAcknowledgment(
	command ActuatorCommand,
	ack ActuatorAcknowledgment,
	now time.Time,
	requireApplied bool,
) error {
	if ack.ControllerSession != command.ControllerSession {
		return fmt.Errorf(
			"%w: acknowledgement session does not match command",
			ErrActuatorUncertain,
		)
	}
	if ack.Sequence != command.Sequence {
		return fmt.Errorf(
			"%w: acknowledgement sequence %d does not match %d",
			ErrActuatorUncertain,
			ack.Sequence,
			command.Sequence,
		)
	}
	if !now.Before(command.ExpiresAt) {
		return fmt.Errorf("%w: acknowledgement arrived after lease expiry", ErrActuatorTimeout)
	}
	if !ack.Accepted && !ack.AppliedAt.IsZero() {
		return fmt.Errorf(
			"%w: rejected acknowledgement claims application",
			ErrActuatorUncertain,
		)
	}
	if ack.Accepted && requireApplied && ack.AppliedAt.IsZero() {
		return fmt.Errorf(
			"%w: accepted acknowledgement has no application time",
			ErrActuatorUncertain,
		)
	}
	if !ack.AppliedAt.IsZero() {
		if ack.AppliedAt.Before(command.IssuedAt) ||
			!ack.AppliedAt.Before(command.ExpiresAt) ||
			ack.AppliedAt.After(now) {
			return fmt.Errorf(
				"%w: application time %s is outside command lease [%s,%s) or after acknowledgement time %s",
				ErrActuatorUncertain,
				ack.AppliedAt,
				command.IssuedAt,
				command.ExpiresAt,
				now,
			)
		}
	}
	return nil
}

func validContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidAuthority)
	}
	return ctx.Err()
}

func (authority *Authority) currentTime(ctx context.Context) (time.Time, error) {
	if ctx == nil {
		return time.Time{}, fmt.Errorf("%w: nil clock context", ErrAuthorityClock)
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, errors.Join(ErrAuthorityClock, err)
	}
	if err := acquireCallbackGateContext(ctx, authority.clockCall); err != nil {
		return time.Time{}, errors.Join(ErrAuthorityClock, err)
	}
	type outcome struct {
		now time.Time
		err error
	}
	result := make(chan outcome, 1)
	go func() {
		value := outcome{}
		defer func() {
			if recovered := recover(); recovered != nil {
				value.now = time.Time{}
				value.err = errors.Join(
					ErrAuthorityClock,
					teleop.ErrCallbackPanic,
					fmt.Errorf("clock callback panic: %v", recovered),
				)
			}
			releaseCallbackGate(authority.clockCall)
			result <- value
		}()
		value.now = authority.now().UTC()
		if value.now.IsZero() {
			value.err = fmt.Errorf("%w: clock returned zero time", ErrAuthorityClock)
		}
	}()
	select {
	case value := <-result:
		return value.now, value.err
	case <-ctx.Done():
		return time.Time{}, errors.Join(ErrAuthorityClock, ctx.Err())
	}
}

func callEvidenceCommit(
	evidence Evidence,
	gate chan struct{},
	ctx context.Context,
	record EvidenceRecord,
) (EvidenceID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !acquireCallbackGate(gate) {
		return "", fmt.Errorf(
			"%w: prior evidence callback is still outstanding",
			ErrEvidence,
		)
	}
	type outcome struct {
		id  EvidenceID
		err error
	}
	result := make(chan outcome, 1)
	// The buffered handoff lets Authority honor ctx even if a defective adapter
	// ignores it. The gate bounds a stranded worker to one; later evidence calls
	// fail immediately so a safety fallback can continue without another leak.
	go func() {
		value := outcome{}
		defer func() {
			if recovered := recover(); recovered != nil {
				value.id = ""
				value.err = errors.Join(
					ErrEvidence,
					teleop.ErrCallbackPanic,
					fmt.Errorf("evidence adapter panic: %v", recovered),
				)
			}
			releaseCallbackGate(gate)
			result <- value
		}()
		value.id, value.err = evidence.Commit(ctx, record.Clone())
	}()
	select {
	case value := <-result:
		return value.id, value.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func callActuatorSend(
	actuator Actuator,
	gate chan struct{},
	ctx context.Context,
	command ActuatorCommand,
) (ActuatorReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !acquireCallbackGate(gate) {
		return nil, fmt.Errorf(
			"%w: prior actuator send is still outstanding",
			ErrActuatorUncertain,
		)
	}
	type outcome struct {
		receipt ActuatorReceipt
		err     error
	}
	result := make(chan outcome, 1)
	// Requested and fallback sends use separate one-worker gates: one uncertain
	// live send cannot suppress its newer safe fallback, while repeated failures
	// cannot grow an unbounded set of abandoned workers.
	go func() {
		value := outcome{}
		defer func() {
			if recovered := recover(); recovered != nil {
				value.receipt = nil
				value.err = errors.Join(
					ErrActuatorUncertain,
					teleop.ErrCallbackPanic,
					fmt.Errorf("actuator send panic: %v", recovered),
				)
			}
			releaseCallbackGate(gate)
			result <- value
		}()
		value.receipt, value.err = actuator.Send(ctx, command.Clone())
	}()
	select {
	case value := <-result:
		return value.receipt, value.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func callActuatorAwait(
	receipt ActuatorReceipt,
	gate chan struct{},
	ctx context.Context,
) (ActuatorAcknowledgment, error) {
	if err := ctx.Err(); err != nil {
		return ActuatorAcknowledgment{}, err
	}
	if !acquireCallbackGate(gate) {
		return ActuatorAcknowledgment{}, fmt.Errorf(
			"%w: prior actuator receipt wait is still outstanding",
			ErrActuatorUncertain,
		)
	}
	type outcome struct {
		ack ActuatorAcknowledgment
		err error
	}
	result := make(chan outcome, 1)
	go func() {
		value := outcome{}
		defer func() {
			if recovered := recover(); recovered != nil {
				value.ack = ActuatorAcknowledgment{}
				value.err = errors.Join(
					ErrActuatorUncertain,
					teleop.ErrCallbackPanic,
					fmt.Errorf("actuator receipt panic: %v", recovered),
				)
			}
			releaseCallbackGate(gate)
			result <- value
		}()
		value.ack, value.err = receipt.Await(ctx)
	}()
	select {
	case value := <-result:
		return value.ack, value.err
	case <-ctx.Done():
		return ActuatorAcknowledgment{}, ctx.Err()
	}
}

func newCallbackGate() chan struct{} {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return gate
}

func acquireCallbackGate(gate chan struct{}) bool {
	select {
	case <-gate:
		return true
	default:
		return false
	}
}

func acquireCallbackGateContext(ctx context.Context, gate chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gate:
		return nil
	}
}

func releaseCallbackGate(gate chan struct{}) { gate <- struct{}{} }

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return safeErrorText(err)
}

func boundedFailureDetail(prefix string, err error) string {
	const maximumRunes = 1024
	detail := prefix
	if err != nil {
		detail += ": " + safeErrorText(err)
	}
	runes := []rune(detail)
	if len(runes) <= maximumRunes {
		return detail
	}
	return string(runes[:maximumRunes-1]) + "…"
}

func safeErrorText(err error) (result string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = fmt.Sprintf("error text panic: %v", recovered)
		}
	}()
	return err.Error()
}

// authorityProcessor forwards the complete processor interface without
// exposing Authority's Guard to callers.
type authorityProcessor struct{ authority *Authority }

func (processor *authorityProcessor) Process(event teleop.Event) (events []teleop.Event) {
	defer processor.authority.cancelLiveApplyIfDecisionChanged()
	return processor.authority.guard.Process(event)
}

func (processor *authorityProcessor) ProcessContext(
	ctx context.Context,
	pc teleop.ProcessingContext,
	event teleop.Event,
) ([]teleop.Event, error) {
	defer processor.authority.cancelLiveApplyIfDecisionChanged()
	return processor.authority.guard.ProcessContext(ctx, pc, event)
}

func (processor *authorityProcessor) Advance(now time.Time) (events []teleop.Event) {
	defer processor.authority.cancelLiveApplyIfDecisionChanged()
	return processor.authority.guard.Advance(now)
}

func (processor *authorityProcessor) AdvanceContext(
	ctx context.Context,
	pc teleop.ProcessingContext,
	now time.Time,
) ([]teleop.Event, error) {
	defer processor.authority.cancelLiveApplyIfDecisionChanged()
	return processor.authority.guard.AdvanceContext(ctx, pc, now)
}
