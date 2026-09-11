package assured

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/safety"
)

// Assured Session errors are intentionally distinguishable so operators can
// surface a missing guarantee rather than presenting a generic open failure.
var (
	ErrInvalidConfig         = errors.New("teleop/assured: invalid configuration")
	ErrAuditGrade            = errors.New("teleop/assured: exact backend audit stream required")
	ErrTransportUnverifiable = errors.New("teleop/assured: transport silence is unverifiable")
	ErrControllerContract    = errors.New("teleop/assured: controller lacks assured-session contract")
	ErrEvidenceNotDurable    = errors.New("teleop/assured: evidence is not locally durable")
	ErrSessionClosed         = errors.New("teleop/assured: session closed")
)

// ProvenanceConfigKey is reserved for the normalized Assured Session control
// profile injected into Provenance.Config. Callers must not supply this key.
const ProvenanceConfigKey = "teleop.assured.v1"

type assuredProvenanceProfile struct {
	// The v1 wire spelling is retained for existing incident tooling. It records
	// the effective strict profile for both Config.Safety and legacy Maritime.
	Maritime   assuredSafetyProfile    `json:"maritime"`
	Authority  assuredAuthorityProfile `json:"authority"`
	Evidence   assuredEvidenceProfile  `json:"evidence"`
	Processors []string                `json:"processors,omitempty"`
}

type assuredSafetyProfile struct {
	CommandTimeout      time.Duration    `json:"command_timeout_ns"`
	TransportTimeout    time.Duration    `json:"transport_timeout_ns"`
	DeadMan             teleop.ControlID `json:"dead_man"`
	DeadManReactuation  time.Duration    `json:"dead_man_reactuation_ns"`
	LoopWatchdog        time.Duration    `json:"loop_watchdog_ns"`
	ArmStickTolerance   float32          `json:"arm_stick_tolerance"`
	ArmTriggerTolerance float32          `json:"arm_trigger_tolerance"`
}

type assuredAuthorityProfile struct {
	PolicyType                   string             `json:"policy_type"`
	EngineeredSafeState          assuredSafeCommand `json:"engineered_safe_state"`
	CommandTTL                   time.Duration      `json:"command_ttl_ns"`
	RequireAppliedAcknowledgment bool               `json:"require_applied_acknowledgment"`
	ClockSource                  string             `json:"clock_source"`
}

type assuredSafeCommand struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type assuredEvidenceProfile struct {
	FlushEveryEvent     bool          `json:"flush_every_event"`
	SynchronousAudit    bool          `json:"synchronous_audit"`
	PipelineAttestation bool          `json:"pipeline_attestation"`
	RequiredWitness     bool          `json:"required_witness"`
	CheckpointInterval  time.Duration `json:"checkpoint_interval_ns"`
	CheckpointEvery     uint64        `json:"checkpoint_every"`
	WitnessTimeout      time.Duration `json:"witness_timeout_ns"`
	ShutdownTimeout     time.Duration `json:"shutdown_timeout_ns"`
	LivenessInterval    time.Duration `json:"liveness_interval_ns"`
}

// Config contains the guarantees required to create an Assured Session.
// Resources in Config are owned by the returned Session once controller open
// begins; startup rollback closes the Recorder and EvidenceStore in the same
// order as normal shutdown.
//
// An Assured Session deliberately has no permissive defaults. In particular,
// it requires an Ed25519 signer, an external Anchor in a separate custody
// domain, required application/operator provenance fields, a strict Guard,
// a receiver-enforced actuator lease, and sync-capable local evidence.
type Config struct {
	// EvidenceStore is the sync-capable local journal. Signer authenticates its
	// tree heads; Anchor must publish those heads in a separate custody domain.
	EvidenceStore EvidenceStore
	Signer        crypto.Signer
	Anchor        audit.Anchor
	// Provenance must identify the application/version, operator,
	// authorization, and application control configuration. Assured reserves
	// ProvenanceConfigKey and injects its normalized effective control profile.
	Provenance audit.Provenance

	// Safety is the validated, domain-neutral strict interlock profile.
	// Set either Safety or legacy Maritime, never both. Values are not merged.
	Safety safety.StrictConfig
	// Maritime is the original spelling of Safety. Its guarantees are identical.
	//
	// Deprecated: use Safety.
	Maritime safety.MaritimeConfig
	// Authority supplies the application-defined safe command and receiver
	// lease; Actuator is the sole system-under-control output adapter.
	Authority safety.AuthorityConfig
	Actuator  safety.Actuator
	// Processors run after Safety Authority and in the given order (for example,
	// gestures then actions). Instances must be non-nil pointers. Their exact
	// identities and order are sealed; application configuration must retain
	// their effective bindings and thresholds in Provenance.Config.
	Processors []teleop.Processor

	// CheckpointInterval controls how often locally durable evidence is signed
	// and offered to the external witness while the recorder is healthy.
	// CheckpointEvery additionally limits the nominal lag by event volume; zero
	// selects audit's conservative default. Neither is an outage-time bound.
	CheckpointInterval time.Duration
	CheckpointEvery    uint64
	// WitnessTimeout bounds each Anchor call and startup witness wait.
	WitnessTimeout time.Duration
	// ShutdownTimeout bounds Authority safe-state work and controller drain.
	// Recorder finalization still waits for each already-bounded Anchor call and
	// for the configured writer, Sync, and signer callbacks so the EvidenceStore
	// is never closed underneath a footer write. Those latter interfaces have no
	// cancellation contract; deployments needing a hard bound must supervise
	// them out of process.
	ShutdownTimeout time.Duration
}

// Session is a single fail-closed controller session. It intentionally does
// not expose the underlying Controller, Guard, Recorder, or output adapters;
// doing so would create paths around durable evidence and safety authority.
type Session struct {
	controller *teleop.Controller
	authority  *safety.Authority
	recorder   *audit.Recorder
	store      EvidenceStore
	bridge     *evidenceBridge

	publicKey       ed25519.PublicKey
	witnessTimeout  time.Duration
	shutdownTimeout time.Duration

	stateMu sync.Mutex
	closing bool

	outputMu sync.Mutex

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// ID exposes the immutable session identity for independently issued operator
// grants. It grants no access to the controller or output adapter.
func (session *Session) ID() teleop.SessionID { return session.authority.Session() }

// OpenSource opens source with the Recorder and Authority processor attached
// before ingest starts. Exact-backend audit delivery and independently
// verifiable transport health are mandatory. ctx bounds startup; the returned
// Session owns its lifetime through Close, including evidenced safe shutdown.
func OpenSource(
	ctx context.Context,
	source teleop.InputSource,
	config Config,
) (*Session, error) {
	if source == nil || nilInterface(source) {
		return nil, fmt.Errorf("%w: nil input source", ErrInvalidConfig)
	}
	prepared, err := prepareConfig(config)
	if err != nil {
		return nil, err
	}
	config = prepared
	descriptor, descriptorErr := sourceDescriptor(source)
	if descriptorErr != nil {
		return nil, descriptorErr
	}
	if err := validateDescriptor(descriptor); err != nil {
		return nil, err
	}
	if _, ok := source.(teleop.TransportHealthSource); !ok {
		return nil, ErrTransportUnverifiable
	}
	return open(ctx, config, func(required []teleop.OpenOption) (teleop.GameController, error) {
		controller, err := openInputSource(source, required)
		return controller, err
	})
}

// OpenProvider opens id through provider without exposing the resulting raw
// controller. The provider must return the concrete *teleop.Controller created
// with the supplied options; wrappers cannot prove that Assured's recorder,
// processor, clock, and synchronous barriers are the ones serving the Session.
// ctx bounds startup; use Session.Close for the returned session's lifetime.
func OpenProvider(
	ctx context.Context,
	provider teleop.Provider,
	id teleop.DeviceID,
	config Config,
) (*Session, error) {
	if provider == nil || nilInterface(provider) {
		return nil, fmt.Errorf("%w: nil provider", ErrInvalidConfig)
	}
	if id == "" {
		return nil, fmt.Errorf("%w: empty device ID", ErrInvalidConfig)
	}
	prepared, err := prepareConfig(config)
	if err != nil {
		return nil, err
	}
	config = prepared
	return open(ctx, config, func(required []teleop.OpenOption) (teleop.GameController, error) {
		return provider.Open(ctx, id, required...)
	})
}

type controllerOpener func([]teleop.OpenOption) (teleop.GameController, error)

func open(
	ctx context.Context,
	config Config,
	opener controllerOpener,
) (*Session, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil open context", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	checkpointEvery := config.CheckpointEvery
	if checkpointEvery == 0 {
		checkpointEvery = audit.DefaultCheckpointEvery
	}
	recorder := audit.NewRecorder(
		config.EvidenceStore,
		audit.WithFlushEveryEvent(true),
		audit.WithSigner(config.Signer),
		audit.WithProvenance(config.Provenance),
		audit.WithAnchor(config.Anchor),
		audit.WithRequiredWitness(true),
		audit.WithAnchorTimeout(config.WitnessTimeout),
		audit.WithCheckpoints(config.CheckpointInterval, checkpointEvery),
	)
	bridge := &evidenceBridge{recorder: recorder}
	guard, err := safety.NewStrict(config.Safety)
	if err != nil {
		return nil, errors.Join(err, closeUnopened(config.EvidenceStore, recorder))
	}
	authority, err := safety.NewAuthorityWithGuard(
		config.Authority,
		guard,
		bridge,
		config.Actuator,
	)
	if err != nil {
		return nil, errors.Join(err, closeUnopened(config.EvidenceStore, recorder))
	}

	livenessInterval := assuredLivenessInterval(config.Safety)
	processor := authority.Processor()
	processors := append([]teleop.Processor{processor}, config.Processors...)
	attestation, err := teleop.NewPipelineAttestation(teleop.PipelineRequirements{
		AuditSinks:       []teleop.EventSink{recorder},
		Processors:       processors,
		SynchronousAudit: true,
		LivenessInterval: livenessInterval,
		ShutdownTimeout:  config.ShutdownTimeout,
	})
	if err != nil {
		return nil, errors.Join(err, closeUnopened(config.EvidenceStore, recorder))
	}
	required := []teleop.OpenOption{
		teleop.WithAuditSink(recorder),
		teleop.WithSynchronousAudit(),
		teleop.WithProcessor(processor),
		// Persist periodic independent transport evidence even when controller
		// state is steady. Assured fixes this interval from the validated
		// transport timeout so it cannot be disabled or weakened by callers.
		teleop.WithLiveness(livenessInterval, 0),
		teleop.WithShutdownTimeout(config.ShutdownTimeout),
		// Providers commonly bind their Open context to controller lifetime.
		// Assured owns ordered shutdown itself; explicitly select the lifetime
		// required by the seal instead of relying on provider defaults.
		teleop.WithContext(context.Background()),
	}
	for _, additional := range config.Processors {
		required = append(required, teleop.WithProcessor(additional))
	}
	// The seal must remain last; all configured processors participate.
	required = append(required, attestation.Option())
	opened, openErr := callControllerOpener(opener, required)
	if openErr != nil {
		return nil, errors.Join(openErr, closeUnopened(config.EvidenceStore, recorder))
	}
	if opened == nil || nilInterface(opened) {
		err = fmt.Errorf("%w: provider returned nil controller", ErrControllerContract)
		closeErr := closeOpened(ctx, config, authority, nil, recorder, config.EvidenceStore)
		return nil, errors.Join(err, closeErr)
	}
	controller, ok := opened.(*teleop.Controller)
	if !ok {
		err = ErrControllerContract
		closeErr := closeOpened(ctx, config, authority, opened, recorder, config.EvidenceStore)
		return nil, errors.Join(err, closeErr)
	}
	if attestErr := controller.AttestPipeline(attestation); attestErr != nil {
		err = errors.Join(ErrControllerContract, attestErr)
		closeErr := closeOpened(ctx, config, authority, controller, recorder, config.EvidenceStore)
		return nil, errors.Join(err, closeErr)
	}
	descriptor, descriptorErr := controllerDescriptor(controller)
	if descriptorErr != nil {
		closeErr := closeOpened(ctx, config, authority, controller, recorder, config.EvidenceStore)
		return nil, errors.Join(descriptorErr, closeErr)
	}
	if err := validateDescriptor(descriptor); err != nil {
		closeErr := closeOpened(ctx, config, authority, controller, recorder, config.EvidenceStore)
		return nil, errors.Join(err, closeErr)
	}
	capabilities, capabilityErr := controllerCapabilities(controller)
	if capabilityErr != nil {
		closeErr := closeOpened(ctx, config, authority, controller, recorder, config.EvidenceStore)
		return nil, errors.Join(capabilityErr, closeErr)
	}
	if capabilities.AuditGrade != teleop.AuditExactBackendStream {
		err = fmt.Errorf("%w: controller capability is %q", ErrAuditGrade, capabilities.AuditGrade)
		closeErr := closeOpened(ctx, config, authority, controller, recorder, config.EvidenceStore)
		return nil, errors.Join(err, closeErr)
	}

	bridge.bind(controller, descriptor)
	session := &Session{
		controller:      controller,
		authority:       authority,
		recorder:        recorder,
		store:           config.EvidenceStore,
		bridge:          bridge,
		publicKey:       recorder.PublicKey(),
		witnessTimeout:  config.WitnessTimeout,
		shutdownTimeout: config.ShutdownTimeout,
		closeDone:       make(chan struct{}),
	}
	if err := waitForTransport(ctx, controller, config.Safety.TransportTimeout); err != nil {
		return nil, errors.Join(err, session.closeNow())
	}
	if err := bindAuthority(ctx, authority, controller); err != nil {
		return nil, errors.Join(err, session.closeNow())
	}
	if err := session.checkpointAndWitness(ctx, "assured session ready"); err != nil {
		return nil, errors.Join(err, session.closeNow())
	}

	go session.finalizeWhenControllerStops()
	return session, nil
}

// Descriptor returns an isolated device descriptor.
func (session *Session) Descriptor() teleop.Descriptor {
	return session.controller.Descriptor()
}

// Capabilities returns an isolated capability description.
func (session *Session) Capabilities() teleop.Capabilities {
	return session.controller.Capabilities()
}

// Snapshot returns the latest isolated controller state.
func (session *Session) Snapshot() teleop.State { return session.controller.Snapshot() }

// SnapshotWithMeta returns the state and its freshness evidence.
func (session *Session) SnapshotWithMeta() (teleop.State, teleop.StateMeta) {
	return session.controller.SnapshotWithMeta()
}

// Subscribe returns a bounded read-only view of the canonical event stream.
func (session *Session) Subscribe(options teleop.SubscriptionOptions) (teleop.Subscription, error) {
	if err := session.active(context.Background()); err != nil {
		return nil, err
	}
	return session.controller.Subscribe(options)
}

// EvidenceStatus returns an isolated snapshot of local durability and witness
// coverage.
func (session *Session) EvidenceStatus() audit.EvidenceStatus {
	return session.recorder.EvidenceStatus()
}

// Done closes after ordered finalization, including EvidenceStore closure.
func (session *Session) Done() <-chan struct{} { return session.closeDone }

// Err returns the joined finalization error after Done closes, or nil while
// the Session is active.
func (session *Session) Err() error {
	select {
	case <-session.closeDone:
		return session.closeErr
	default:
		return nil
	}
}

// Arm delegates the complete evidenced transition to Authority.
func (session *Session) Arm(ctx context.Context, detail string) error {
	if err := session.active(ctx); err != nil {
		return err
	}
	return session.authority.Arm(ctx, detail)
}

// Reset clears a latched stop but leaves output inhibited until Arm.
func (session *Session) Reset(ctx context.Context, detail string) error {
	if err := session.active(ctx); err != nil {
		return err
	}
	return session.authority.Reset(ctx, detail)
}

// Disarm revokes authority and synchronously attempts the engineered safe
// state. Caller cancellation does not suppress revocation or the bounded
// safe-state attempt; use Close for a Session already shutting down.
func (session *Session) Disarm(ctx context.Context, detail string) error {
	if err := session.activeSafety(ctx); err != nil {
		return err
	}
	return session.authority.Disarm(ctx, detail)
}

// EmergencyStop latches an inhibit and synchronously attempts the engineered
// safe state. It does not replace an independent hardware emergency stop.
// Caller cancellation does not suppress revocation or the bounded safe-state
// attempt; use Close for a Session already shutting down.
func (session *Session) EmergencyStop(ctx context.Context, detail string) error {
	if err := session.activeSafety(ctx); err != nil {
		return err
	}
	return session.authority.EmergencyStop(ctx, detail)
}

// Apply is the only exposed vessel-command path.
func (session *Session) Apply(
	ctx context.Context,
	request safety.ApplyRequest,
) (safety.ApplyResult, error) {
	if err := session.active(ctx); err != nil {
		return safety.ApplyResult{}, err
	}
	return session.authority.Apply(ctx, request)
}

// SetRumble synchronously records an attempt, performs the output, and records
// its outcome. If outcome evidence is unavailable, it immediately attempts to
// stop rumble so an unaudited effect cannot remain active.
func (session *Session) SetRumble(ctx context.Context, rumble teleop.Rumble) error {
	if err := session.active(ctx); err != nil {
		return err
	}
	session.outputMu.Lock()
	defer session.outputMu.Unlock()
	if err := session.active(ctx); err != nil {
		return err
	}

	attemptID, attemptErr := session.bridge.recordCommand(ctx, teleop.Command{
		Name:       "assured.rumble.attempt",
		Payload:    rumbleAttempt{Requested: rumble},
		Authorized: true,
	})
	if attemptErr != nil {
		return attemptErr
	}

	operationErr := session.controller.SetRumble(ctx, rumble)
	outcomeCtx, cancelOutcome := session.safetyContext(ctx)
	_, outcomeErr := session.bridge.recordCommand(outcomeCtx, teleop.Command{
		Name: "assured.rumble.outcome",
		Payload: rumbleOutcome{
			Attempt:   attemptID.String(),
			Requested: rumble,
			Accepted:  operationErr == nil,
			Error:     errorText(operationErr),
		},
		Authorized: operationErr == nil,
		Reason:     errorText(operationErr),
		Causes:     []teleop.EventID{attemptID},
	})
	cancelOutcome()
	if outcomeErr == nil {
		return operationErr
	}

	stopCtx, cancelStop := session.safetyContext(ctx)
	stopErr := session.controller.SetRumble(stopCtx, teleop.Rumble{})
	_, stopEvidenceErr := session.bridge.recordCommand(stopCtx, teleop.Command{
		Name: "assured.rumble.safe_stop",
		Payload: rumbleOutcome{
			Attempt:   attemptID.String(),
			Requested: teleop.Rumble{},
			Accepted:  stopErr == nil,
			Error:     errorText(stopErr),
		},
		Authorized: stopErr == nil,
		Reason:     errorText(stopErr),
		Causes:     []teleop.EventID{attemptID},
	})
	cancelStop()
	return errors.Join(operationErr, outcomeErr, stopErr, stopEvidenceErr)
}

type rumbleAttempt struct {
	Requested teleop.Rumble `json:"requested"`
}

type rumbleOutcome struct {
	Attempt   string        `json:"attempt"`
	Requested teleop.Rumble `json:"requested"`
	Accepted  bool          `json:"accepted"`
	Error     string        `json:"error,omitempty"`
}

// Close is ordered and idempotent: Authority first applies the engineered
// safe state; Controller then drains and closes; Recorder writes its signed,
// required-witness footer; finally the local store is synchronized and closed.
func (session *Session) Close() error {
	session.closeOnce.Do(func() {
		session.closeErr = session.finalize()
		// Publish completion only after closeErr is final. Closing the channel is
		// the happens-before edge used by Done waiters and Err readers.
		close(session.closeDone)
	})
	<-session.closeDone
	return session.closeErr
}

func (session *Session) closeNow() error { return session.Close() }

func (session *Session) finalizeWhenControllerStops() {
	<-session.controller.Done()
	_ = session.Close()
}

func (session *Session) finalize() (result error) {
	session.stateMu.Lock()
	session.closing = true
	session.stateMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), session.shutdownTimeout)
	defer cancel()
	// An unexpected controller termination has already drained and closed its
	// synchronous command path. Switch to the dedicated post-terminal recorder
	// stream only in that proven state so Authority.Close evidence still lands
	// before the signed footer. Explicit Close continues through Controller.
	terminalAtStart := channelClosed(session.controller.Done())
	if terminalAtStart {
		session.bridge.enableTerminalRecording()
	}
	authorityErr := session.authority.Close(ctx)
	// Controller termination can race the first close transaction. If its Done
	// barrier crossed while Authority.Close was using RecordCommandSync, switch to
	// the dedicated terminal stream and retry the documented idempotent fallback.
	// The retry has a distinct lifecycle attempt and a higher actuator sequence.
	if !terminalAtStart && channelClosed(session.controller.Done()) {
		session.bridge.enableTerminalRecording()
		authorityErr = errors.Join(authorityErr, session.authority.Close(ctx))
	}
	// No rumble call that passed its preflight checks may cross controller
	// shutdown: wait for it to finish, then Controller.Close performs the final
	// source-level rumble stop while closing prevents any successor.
	session.outputMu.Lock()
	defer session.outputMu.Unlock()
	controllerErr := closeGameController(session.controller)
	recorderErr := session.recorder.Close()
	storeSyncErr := syncEvidenceStore(session.store)
	storeCloseErr := closeEvidenceStore(session.store)
	return errors.Join(
		authorityErr,
		controllerErr,
		recorderErr,
		storeSyncErr,
		storeCloseErr,
	)
}

func (session *Session) active(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return session.activeSafety(ctx)
}

// Revoking authority must not inherit caller cancellation. Keep the lifetime
// check shared with ordinary operations so ordered finalization still owns a
// closing Session. Authority supplies its own bounded safety contexts.
func (session *Session) activeSafety(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidConfig)
	}
	session.stateMu.Lock()
	closing := session.closing
	session.stateMu.Unlock()
	if closing {
		return ErrSessionClosed
	}
	select {
	case <-session.controller.Done():
		return errors.Join(ErrSessionClosed, session.controller.Err())
	default:
		return nil
	}
}

func (session *Session) safetyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), session.shutdownTimeout)
}

func (session *Session) checkpointAndWitness(ctx context.Context, reason string) error {
	witnessCtx, cancel := context.WithTimeout(ctx, session.witnessTimeout)
	defer cancel()
	receipt, err := session.recorder.CheckpointAndWait(witnessCtx, reason)
	if err != nil {
		return err
	}
	status := session.recorder.EvidenceStatus()
	if err := validateDurability(status, 1); err != nil {
		return err
	}
	if receipt.Checkpoint.Session != session.controller.Session() {
		return fmt.Errorf("%w: startup witness does not cover the controller session", audit.ErrWitnessRequired)
	}
	if err := audit.VerifyCheckpoint(session.publicKey, receipt.Checkpoint); err != nil {
		return fmt.Errorf("verify startup witness: %w", err)
	}
	return nil
}

type evidenceBridge struct {
	mu         sync.RWMutex
	controller *teleop.Controller
	recorder   *audit.Recorder
	identities map[safety.EvidenceID]teleop.EventID
	terminal   bool
	session    teleop.SessionID
	device     teleop.DeviceID

	terminalMu       sync.Mutex
	terminalSequence uint64
}

func (bridge *evidenceBridge) bind(
	controller *teleop.Controller,
	descriptor teleop.Descriptor,
) {
	bridge.mu.Lock()
	bridge.controller = controller
	bridge.identities = make(map[safety.EvidenceID]teleop.EventID)
	bridge.session = controller.Session()
	bridge.device = descriptor.ID
	bridge.mu.Unlock()
}

// enableTerminalRecording is the sole exception to routing Activity Evidence
// through Controller.RecordCommandSync. It is enabled only after Controller's
// Done channel proves that its sink queues have drained and its identity
// allocator can no longer accept work. A private stream then lets ordered
// shutdown evidence reach the still-open Recorder before footer finalization.
func (bridge *evidenceBridge) enableTerminalRecording() {
	bridge.mu.Lock()
	if bridge.controller != nil && channelClosed(bridge.controller.Done()) {
		bridge.terminal = true
	}
	bridge.mu.Unlock()
}

func (bridge *evidenceBridge) Commit(
	ctx context.Context,
	record safety.EvidenceRecord,
) (safety.EvidenceID, error) {
	reason := record.Detail
	if record.Error != "" {
		reason = record.Error
	}
	causes := append([]teleop.EventID(nil), record.Causes...)
	bridge.mu.RLock()
	parent, parentKnown := bridge.identities[record.ParentID]
	pendingParents := len(bridge.identities)
	bridge.mu.RUnlock()
	// Authority is serialized, so healthy traffic has only a handful of open
	// chains. Repeated partially recorded failures must fail closed, not grow
	// unbounded bookkeeping or evict identities and silently lose causality.
	if pendingParents >= maxPendingEvidenceParents && !parentKnown && evidenceCanBeParent(record.Kind) {
		return "", fmt.Errorf("%w: too many unfinished authority evidence chains", ErrEvidenceNotDurable)
	}
	if parentKnown && !containsEventID(causes, parent) {
		causes = append(causes, parent)
	}
	id, err := bridge.recordCommand(ctx, teleop.Command{
		Name:       string(record.Kind),
		Payload:    record.Clone(),
		Authorized: evidenceAuthorized(record),
		Reason:     reason,
		Causes:     causes,
	})
	if err != nil {
		return "", err
	}
	evidenceID := safety.EvidenceID(id.String())
	bridge.advanceIdentity(record.Kind, record.ParentID, evidenceID, id)
	return evidenceID, nil
}

const maxPendingEvidenceParents = 64

func (bridge *evidenceBridge) advanceIdentity(
	kind safety.EvidenceKind,
	parentID safety.EvidenceID,
	evidenceID safety.EvidenceID,
	eventID teleop.EventID,
) {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if parentID != "" {
		delete(bridge.identities, parentID)
	}
	if evidenceCanBeParent(kind) {
		bridge.identities[evidenceID] = eventID
	}
}

func evidenceCanBeParent(kind safety.EvidenceKind) bool {
	switch kind {
	case safety.EvidenceLifecycleAttempt,
		safety.EvidenceDecision,
		safety.EvidenceIntent,
		safety.EvidenceSent:
		return true
	default:
		return false
	}
}

func (bridge *evidenceBridge) recordCommand(
	ctx context.Context,
	command teleop.Command,
) (teleop.EventID, error) {
	bridge.mu.RLock()
	controller := bridge.controller
	terminal := bridge.terminal
	bridge.mu.RUnlock()
	if controller == nil {
		return teleop.EventID{}, fmt.Errorf("%w: evidence bridge is unbound", ErrControllerContract)
	}
	before := bridge.recorder.EvidenceStatus()
	var id teleop.EventID
	var err error
	if terminal {
		id, err = bridge.recordTerminalCommand(ctx, command)
	} else {
		id, err = controller.RecordCommandSync(ctx, command)
	}
	if err != nil {
		return teleop.EventID{}, err
	}
	after := bridge.recorder.EvidenceStatus()
	if err := validateDurability(after, before.AcceptedEvents+1); err != nil {
		return teleop.EventID{}, err
	}
	return id, nil
}

func (bridge *evidenceBridge) recordTerminalCommand(
	ctx context.Context,
	command teleop.Command,
) (teleop.EventID, error) {
	bridge.terminalMu.Lock()
	defer bridge.terminalMu.Unlock()
	if ctx == nil {
		return teleop.EventID{}, fmt.Errorf("%w: nil terminal evidence context", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return teleop.EventID{}, err
	}
	if command.Name == "" {
		return teleop.EventID{}, fmt.Errorf("%w: terminal command name is empty", ErrInvalidConfig)
	}
	payload, err := json.Marshal(command.Payload)
	if err != nil {
		return teleop.EventID{}, fmt.Errorf("encode terminal command: %w", err)
	}
	monotonic, err := controllerMonotonic(bridge.controller)
	if err != nil {
		return teleop.EventID{}, err
	}
	now, err := controllerWallTime(bridge.controller)
	if err != nil {
		return teleop.EventID{}, err
	}
	now = now.UTC()
	id := teleop.EventID{
		Session:  bridge.session,
		Stream:   "assured-terminal",
		Sequence: bridge.terminalSequence + 1,
	}
	event := teleop.CommandEvent{
		Meta: teleop.Header{
			ID:                id,
			DeviceID:          bridge.device,
			ObservedAt:        now,
			ReceivedAt:        now,
			PublishedAt:       now,
			Monotonic:         monotonic,
			ReceivedMonotonic: monotonic,
			Causes:            append([]teleop.EventID(nil), command.Causes...),
		},
		Command:    command.Name,
		Payload:    payload,
		Authorized: command.Authorized,
		Reason:     command.Reason,
	}
	if err := bridge.recorder.Record(ctx, event); err != nil {
		return teleop.EventID{}, err
	}
	bridge.terminalSequence = id.Sequence
	return id, nil
}

func evidenceAuthorized(record safety.EvidenceRecord) bool {
	if record.Policy != nil {
		return record.Policy.Permit && record.Decision != nil && record.Decision.Permit
	}
	if record.Actuator != nil {
		return !record.Actuator.Fallback
	}
	if record.Decision != nil {
		return record.Decision.Permit
	}
	return record.Error == ""
}

func validateDurability(status audit.EvidenceStatus, minimum uint64) error {
	if status.Err != nil {
		return errors.Join(ErrEvidenceNotDurable, status.Err)
	}
	if status.Durability != "fsync-every-record" {
		return fmt.Errorf(
			"%w: recorder reports %q",
			ErrEvidenceNotDurable,
			status.Durability,
		)
	}
	if status.AcceptedEvents < minimum || status.LocallyDurableEvents < status.AcceptedEvents {
		return fmt.Errorf(
			"%w: accepted=%d durable=%d minimum=%d",
			ErrEvidenceNotDurable,
			status.AcceptedEvents,
			status.LocallyDurableEvents,
			minimum,
		)
	}
	return nil
}

func containsEventID(ids []teleop.EventID, target teleop.EventID) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// prepareConfig validates the caller's configuration and then crosses its two
// open-ended JSON boundaries exactly once. The returned configuration contains
// only frozen safe-state bytes and JSON-native provenance, so the Recorder and
// Authority cannot observe different results from a stateful Marshaler.
func prepareConfig(config Config) (Config, error) {
	// Resolve the compatibility spelling once, before validation or callbacks.
	// Reject dual configuration even when equal: no precedence or field merging
	// may silently change the interlocks that are validated and recorded.
	if config.Maritime != (safety.StrictConfig{}) {
		if config.Safety != (safety.StrictConfig{}) {
			return Config{}, fmt.Errorf("%w: set either Safety or Maritime, not both", ErrInvalidConfig)
		}
		config.Safety = config.Maritime
		config.Maritime = safety.MaritimeConfig{}
	}
	if err := validateConfig(config); err != nil {
		return Config{}, err
	}
	if _, exists := config.Provenance.Config[ProvenanceConfigKey]; exists {
		return Config{}, fmt.Errorf(
			"%w: provenance config key %q is reserved",
			ErrInvalidConfig,
			ProvenanceConfigKey,
		)
	}

	safePayload, err := normalizeConfigurationJSON(
		"engineered safe-state payload",
		config.Authority.EngineeredSafeState.Payload,
	)
	if err != nil {
		return Config{}, err
	}
	config.Authority.EngineeredSafeState.Payload = safePayload

	provenance, err := normalizeProvenance(config.Provenance)
	if err != nil {
		return Config{}, err
	}
	checkpointEvery := config.CheckpointEvery
	if checkpointEvery == 0 {
		checkpointEvery = audit.DefaultCheckpointEvery
	}
	config.CheckpointEvery = checkpointEvery
	config.Processors = slices.Clone(config.Processors)
	processorTypes := make([]string, len(config.Processors))
	for i, processor := range config.Processors {
		processorTypes[i] = reflect.TypeOf(processor).String()
	}

	// Keep a separate slice in provenance even though both copies contain the
	// same normalized bytes. Neither subsystem can mutate the other's payload.
	provenanceSafePayload := append(json.RawMessage(nil), safePayload...)
	provenance.Config[ProvenanceConfigKey] = assuredProvenanceProfile{
		Processors: processorTypes,
		Maritime: assuredSafetyProfile{
			CommandTimeout:      config.Safety.CommandTimeout,
			TransportTimeout:    config.Safety.TransportTimeout,
			DeadMan:             config.Safety.DeadMan,
			DeadManReactuation:  config.Safety.DeadManReactuation,
			LoopWatchdog:        config.Safety.LoopWatchdog,
			ArmStickTolerance:   config.Safety.ArmStickTolerance,
			ArmTriggerTolerance: config.Safety.ArmTriggerTolerance,
		},
		Authority: assuredAuthorityProfile{
			PolicyType: reflect.TypeOf(config.Authority.Policy).String(),
			EngineeredSafeState: assuredSafeCommand{
				Name:    config.Authority.EngineeredSafeState.Name,
				Payload: provenanceSafePayload,
			},
			CommandTTL:                   config.Authority.CommandTTL,
			RequireAppliedAcknowledgment: config.Authority.RequireAppliedAcknowledgment,
			ClockSource:                  "system",
		},
		Evidence: assuredEvidenceProfile{
			FlushEveryEvent:     true,
			SynchronousAudit:    true,
			PipelineAttestation: true,
			RequiredWitness:     true,
			CheckpointInterval:  config.CheckpointInterval,
			CheckpointEvery:     checkpointEvery,
			WitnessTimeout:      config.WitnessTimeout,
			ShutdownTimeout:     config.ShutdownTimeout,
			LivenessInterval:    assuredLivenessInterval(config.Safety),
		},
	}
	config.Provenance = provenance
	return config, nil
}

func assuredLivenessInterval(config safety.StrictConfig) time.Duration {
	return max(config.TransportTimeout/2, time.Nanosecond)
}

func normalizeConfigurationJSON(label string, value any) (encoded json.RawMessage, err error) {
	if value == nil {
		return nil, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			encoded = nil
			err = errors.Join(
				ErrInvalidConfig,
				teleop.ErrCallbackPanic,
				fmt.Errorf("%s JSON callback: %v", label, recovered),
			)
		}
	}()
	payload, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return nil, errors.Join(
			ErrInvalidConfig,
			fmt.Errorf("%s: %w", label, marshalErr),
		)
	}
	return append(json.RawMessage(nil), payload...), nil
}

func normalizeProvenance(provenance audit.Provenance) (normalized audit.Provenance, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			normalized = audit.Provenance{}
			err = errors.Join(
				ErrInvalidConfig,
				teleop.ErrCallbackPanic,
				fmt.Errorf("provenance JSON callback: %v", recovered),
			)
		}
	}()
	encoded, marshalErr := json.Marshal(provenance)
	if marshalErr != nil {
		return audit.Provenance{}, errors.Join(
			ErrInvalidConfig,
			fmt.Errorf("provenance: %w", marshalErr),
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&normalized); decodeErr != nil {
		return audit.Provenance{}, errors.Join(
			ErrInvalidConfig,
			fmt.Errorf("normalize provenance: %w", decodeErr),
		)
	}
	return normalized, nil
}

func validateConfig(config Config) (result error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = errors.Join(
				result,
				ErrInvalidConfig,
				fmt.Errorf("%w: configuration callback: %v", teleop.ErrCallbackPanic, recovered),
			)
		}
	}()
	if config.EvidenceStore == nil || nilInterface(config.EvidenceStore) {
		result = errors.Join(result, fmt.Errorf("%w: EvidenceStore is required", ErrInvalidConfig))
	}
	if config.Signer == nil || nilInterface(config.Signer) {
		result = errors.Join(result, fmt.Errorf("%w: Signer is required", ErrInvalidConfig))
	} else if key, err := signerPublicKey(config.Signer); err != nil {
		result = errors.Join(result, fmt.Errorf("%w: Signer: %w", ErrInvalidConfig, err))
	} else if len(key) != ed25519.PublicKeySize {
		result = errors.Join(result, fmt.Errorf(
			"%w: Signer must expose an Ed25519 public key",
			ErrInvalidConfig,
		))
	}
	if config.Anchor == nil || nilInterface(config.Anchor) {
		result = errors.Join(result, fmt.Errorf("%w: external Anchor is required", ErrInvalidConfig))
	}
	if config.Actuator == nil || nilInterface(config.Actuator) {
		result = errors.Join(result, fmt.Errorf("%w: Actuator is required", ErrInvalidConfig))
	}
	if config.Authority.Policy == nil || nilInterface(config.Authority.Policy) {
		result = errors.Join(result, fmt.Errorf("%w: Authority.Policy is required", ErrInvalidConfig))
	}
	for i, processor := range config.Processors {
		value := reflect.ValueOf(processor)
		if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
			result = errors.Join(result, fmt.Errorf("%w: processor %d must be a non-nil pointer instance", ErrInvalidConfig, i))
		}
	}
	if err := validateProvenance(config.Provenance); err != nil {
		result = errors.Join(result, err)
	}
	if config.CheckpointInterval <= 0 {
		result = errors.Join(result, fmt.Errorf("%w: CheckpointInterval must be positive", ErrInvalidConfig))
	}
	if config.WitnessTimeout <= 0 {
		result = errors.Join(result, fmt.Errorf("%w: WitnessTimeout must be positive", ErrInvalidConfig))
	}
	if config.ShutdownTimeout <= 0 {
		result = errors.Join(result, fmt.Errorf("%w: ShutdownTimeout must be positive", ErrInvalidConfig))
	}
	if config.Authority.CommandTTL <= 0 {
		result = errors.Join(result, fmt.Errorf("%w: command TTL must be positive", ErrInvalidConfig))
	}
	if config.Authority.EngineeredSafeState.Name == "" {
		result = errors.Join(result, fmt.Errorf(
			"%w: engineered safe-state command name is required",
			ErrInvalidConfig,
		))
	}
	if !config.Authority.RequireAppliedAcknowledgment {
		result = errors.Join(result, fmt.Errorf(
			"%w: Authority.RequireAppliedAcknowledgment must be enabled",
			ErrInvalidConfig,
		))
	}
	if config.Authority.Now != nil {
		result = errors.Join(result, fmt.Errorf(
			"%w: Authority.Now must be nil; Assured requires the system clock",
			ErrInvalidConfig,
		))
	}
	for _, deadline := range []struct {
		name  string
		value time.Duration
	}{
		{name: "command timeout", value: config.Safety.CommandTimeout},
		{name: "transport timeout", value: config.Safety.TransportTimeout},
		{name: "loop watchdog", value: config.Safety.LoopWatchdog},
		{name: "dead-man re-actuation", value: config.Safety.DeadManReactuation},
		{name: "shutdown timeout", value: config.ShutdownTimeout},
	} {
		if deadline.value > 0 && config.Authority.CommandTTL > deadline.value {
			result = errors.Join(result, fmt.Errorf(
				"%w: command TTL %s exceeds %s %s",
				ErrInvalidConfig,
				config.Authority.CommandTTL,
				deadline.name,
				deadline.value,
			))
		}
	}
	if _, err := safety.NewStrict(config.Safety); err != nil {
		result = errors.Join(result, ErrInvalidConfig, err)
	}
	return result
}

func validateProvenance(provenance audit.Provenance) error {
	var missing []string
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "application", value: provenance.Application},
		{name: "application_version", value: provenance.ApplicationVersion},
		{name: "operator", value: provenance.Operator},
		{name: "authorization", value: provenance.Authorization},
	} {
		if field.value == "" {
			missing = append(missing, field.name)
		}
	}
	if len(provenance.Config) == 0 {
		missing = append(missing, "config")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: incomplete provenance fields %v", ErrInvalidConfig, missing)
	}
	return nil
}

func validateDescriptor(descriptor teleop.Descriptor) error {
	if descriptor.Capability.AuditGrade != teleop.AuditExactBackendStream {
		return fmt.Errorf(
			"%w: device %q reports %q",
			ErrAuditGrade,
			descriptor.ID,
			descriptor.Capability.AuditGrade,
		)
	}
	return nil
}

func waitForTransport(
	ctx context.Context,
	controller *teleop.Controller,
	timeout time.Duration,
) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, meta, snapshotErr := controllerSnapshot(controller)
		if snapshotErr != nil {
			return errors.Join(ErrTransportUnverifiable, snapshotErr)
		}
		if meta.TransportSilenceVerifiable && meta.TransportCheckSequence > 0 && meta.Connected {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return errors.Join(ErrTransportUnverifiable, waitCtx.Err())
		case <-controller.Done():
			return errors.Join(ErrTransportUnverifiable, controller.Err())
		case <-ticker.C:
		}
	}
}

func sourceDescriptor(source teleop.InputSource) (descriptor teleop.Descriptor, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: input source descriptor: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	return source.Descriptor(), nil
}

func openInputSource(
	source teleop.InputSource,
	options []teleop.OpenOption,
) (controller *teleop.Controller, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: open input source: %v", teleop.ErrCallbackPanic, recovered)
			if closeErr := closeInputSource(source); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
	}()
	return teleop.NewController(source, options...)
}

func closeInputSource(source teleop.InputSource) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: close input source: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	return source.Close()
}

func callControllerOpener(
	opener controllerOpener,
	options []teleop.OpenOption,
) (controller teleop.GameController, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: provider open: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	return opener(options)
}

func controllerDescriptor(
	controller *teleop.Controller,
) (descriptor teleop.Descriptor, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: controller descriptor: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	return controller.Descriptor(), nil
}

func controllerCapabilities(
	controller *teleop.Controller,
) (capabilities teleop.Capabilities, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: controller capabilities: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	return controller.Capabilities(), nil
}

func controllerSnapshot(
	controller *teleop.Controller,
) (state teleop.State, meta teleop.StateMeta, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: controller snapshot: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	state, meta = controller.SnapshotWithMeta()
	return state, meta, nil
}

func controllerMonotonic(controller *teleop.Controller) (monotonic time.Duration, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: controller monotonic clock: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	return controller.Monotonic(), nil
}

func controllerWallTime(controller *teleop.Controller) (now time.Time, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: controller wall clock: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	return controller.Now(), nil
}

func bindAuthority(
	ctx context.Context,
	authority *safety.Authority,
	controller *teleop.Controller,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: bind safety authority: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	return authority.Bind(ctx, controller)
}

func channelClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func closeUnopened(store EvidenceStore, recorder *audit.Recorder) error {
	return errors.Join(recorder.Close(), syncEvidenceStore(store), closeEvidenceStore(store))
}

func closeOpened(
	ctx context.Context,
	config Config,
	authority *safety.Authority,
	controller teleop.GameController,
	recorder *audit.Recorder,
	store EvidenceStore,
) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), config.ShutdownTimeout)
	defer cancel()
	var authorityErr, controllerErr error
	if authority != nil {
		authorityErr = authority.Close(shutdownCtx)
	}
	if controller != nil && !nilInterface(controller) {
		controllerErr = closeGameController(controller)
	}
	return errors.Join(
		authorityErr,
		controllerErr,
		recorder.Close(),
		syncEvidenceStore(store),
		closeEvidenceStore(store),
	)
}

func signerPublicKey(signer crypto.Signer) (key ed25519.PublicKey, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			key = nil
			err = fmt.Errorf("%w: signer public key: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	if signer == nil || nilInterface(signer) {
		return nil, fmt.Errorf("signer is nil")
	}
	public := signer.Public()
	key, ok := public.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is %T, want ed25519.PublicKey", public)
	}
	return append(ed25519.PublicKey(nil), key...), nil
}

func syncEvidenceStore(store EvidenceStore) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: evidence store sync: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	if store == nil || nilInterface(store) {
		return fmt.Errorf("%w: evidence store is nil", ErrInvalidConfig)
	}
	return store.Sync()
}

func closeEvidenceStore(store EvidenceStore) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: evidence store close: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	if store == nil || nilInterface(store) {
		return fmt.Errorf("%w: evidence store is nil", ErrInvalidConfig)
	}
	return store.Close()
}

func closeGameController(controller teleop.GameController) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: controller close: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	return controller.Close()
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var _ safety.Evidence = (*evidenceBridge)(nil)
