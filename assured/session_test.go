package assured_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/assured"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/safety"
	"github.com/open-ships/teleop/testkit"
)

type syncStore struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	syncs    int
	failAt   int
	closed   bool
	closeErr error
}

type armablePanicStore struct {
	base *syncStore

	mu         sync.Mutex
	panicSync  bool
	panicClose bool
	syncCalls  int
	closeCalls int
}

func newArmablePanicStore() *armablePanicStore {
	return &armablePanicStore{base: &syncStore{}}
}

func (store *armablePanicStore) Write(payload []byte) (int, error) {
	return store.base.Write(payload)
}

func (store *armablePanicStore) Sync() error {
	store.mu.Lock()
	store.syncCalls++
	panics := store.panicSync
	store.mu.Unlock()
	if panics {
		panic("evidence store sync fault")
	}
	return store.base.Sync()
}

func (store *armablePanicStore) Close() error {
	store.mu.Lock()
	store.closeCalls++
	panics := store.panicClose
	store.mu.Unlock()
	if panics {
		panic("evidence store close fault")
	}
	return store.base.Close()
}

func (store *armablePanicStore) arm() {
	store.mu.Lock()
	store.panicSync = true
	store.panicClose = true
	store.mu.Unlock()
}

func (store *armablePanicStore) calls() (syncs, closes int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.syncCalls, store.closeCalls
}

func (store *syncStore) Write(payload []byte) (int, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return 0, io.ErrClosedPipe
	}
	return store.buffer.Write(payload)
}

func (store *syncStore) Sync() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return io.ErrClosedPipe
	}
	store.syncs++
	if store.failAt > 0 && store.syncs >= store.failAt {
		return errStoreSync
	}
	return nil
}

func (store *syncStore) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return store.closeErr
	}
	store.closed = true
	return store.closeErr
}

func (store *syncStore) bytes() []byte {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]byte(nil), store.buffer.Bytes()...)
}

func (store *syncStore) failAfter(additionalSyncs int) {
	store.mu.Lock()
	store.failAt = store.syncs + additionalSyncs
	store.mu.Unlock()
}

var errStoreSync = errors.New("test evidence sync failed")

type witnessAnchor struct {
	mu          sync.Mutex
	checkpoints []audit.Checkpoint
	err         error
}

func (anchor *witnessAnchor) Publish(_ context.Context, checkpoint audit.Checkpoint) error {
	anchor.mu.Lock()
	defer anchor.mu.Unlock()
	if anchor.err != nil {
		return anchor.err
	}
	anchor.checkpoints = append(anchor.checkpoints, checkpoint)
	return nil
}

func (anchor *witnessAnchor) snapshot() []audit.Checkpoint {
	anchor.mu.Lock()
	defer anchor.mu.Unlock()
	return append([]audit.Checkpoint(nil), anchor.checkpoints...)
}

type delayedAnchor struct {
	*witnessAnchor
	delay time.Duration
}

func (anchor *delayedAnchor) Publish(ctx context.Context, checkpoint audit.Checkpoint) error {
	timer := time.NewTimer(anchor.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return anchor.witnessAnchor.Publish(ctx, checkpoint)
	}
}

type acceptingActuator struct {
	mu       sync.Mutex
	commands []safety.ActuatorCommand
	failSend bool
	zeroTime bool
}

func (actuator *acceptingActuator) Send(
	_ context.Context,
	command safety.ActuatorCommand,
) (safety.ActuatorReceipt, error) {
	actuator.mu.Lock()
	actuator.commands = append(actuator.commands, command.Clone())
	fail := actuator.failSend
	actuator.mu.Unlock()
	if fail {
		return nil, errActuator
	}
	return safety.ReceiptFunc(func(context.Context) (safety.ActuatorAcknowledgment, error) {
		var appliedAt time.Time
		if !actuator.zeroTime {
			appliedAt = time.Now().UTC()
		}
		return safety.ActuatorAcknowledgment{
			ControllerSession: command.ControllerSession,
			Sequence:          command.Sequence,
			Accepted:          true,
			AppliedAt:         appliedAt,
		}, nil
	}), nil
}

func (actuator *acceptingActuator) snapshot() []safety.ActuatorCommand {
	actuator.mu.Lock()
	defer actuator.mu.Unlock()
	result := make([]safety.ActuatorCommand, len(actuator.commands))
	for index := range actuator.commands {
		result[index] = actuator.commands[index].Clone()
	}
	return result
}

type closeRaceActuator struct {
	mu       sync.Mutex
	commands []safety.ActuatorCommand
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (actuator *closeRaceActuator) Send(
	_ context.Context,
	command safety.ActuatorCommand,
) (safety.ActuatorReceipt, error) {
	actuator.mu.Lock()
	actuator.commands = append(actuator.commands, command.Clone())
	call := len(actuator.commands)
	actuator.mu.Unlock()
	if call == 2 {
		actuator.once.Do(func() { close(actuator.entered) })
		return safety.ReceiptFunc(func(context.Context) (safety.ActuatorAcknowledgment, error) {
			<-actuator.release
			return safety.ActuatorAcknowledgment{}, errors.New("controller stopped during acknowledgement")
		}), nil
	}
	return safety.ReceiptFunc(func(context.Context) (safety.ActuatorAcknowledgment, error) {
		return safety.ActuatorAcknowledgment{
			ControllerSession: command.ControllerSession,
			Sequence:          command.Sequence,
			Accepted:          true,
			AppliedAt:         command.IssuedAt,
		}, nil
	}), nil
}

func (actuator *closeRaceActuator) snapshot() []safety.ActuatorCommand {
	actuator.mu.Lock()
	defer actuator.mu.Unlock()
	commands := make([]safety.ActuatorCommand, len(actuator.commands))
	for index := range actuator.commands {
		commands[index] = actuator.commands[index].Clone()
	}
	return commands
}

var errActuator = errors.New("test actuator failed")

func exactSource(rumble bool) *testkit.FakeSource {
	const deadMan = teleop.ButtonBumperRight
	source := testkit.NewFakeSource(teleop.Descriptor{
		ID:        "assured:exact",
		Name:      "Assured exact source",
		Transport: teleop.TransportUSB,
		Backend:   "assured-test",
		Capability: teleop.Capabilities{
			AuditGrade: teleop.AuditExactBackendStream,
			Rumble:     rumble,
			Controls: []teleop.ControlDescriptor{{
				ID: deadMan, Kind: teleop.ControlButton,
			}},
		},
	}, 32)
	source.SetTransportHealth(teleop.TransportHealth{
		Sequence:          1,
		CheckedAt:         time.Now(),
		Connected:         true,
		SilenceVerifiable: true,
	})
	return source
}

func validConfig(
	t *testing.T,
	store assured.EvidenceStore,
	anchor audit.Anchor,
	actuator safety.Actuator,
) (assured.Config, ed25519.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	maritime := safety.DefaultMaritimeConfig(teleop.ButtonBumperRight)
	maritime.CommandTimeout = 500 * time.Millisecond
	maritime.TransportTimeout = 500 * time.Millisecond
	maritime.LoopWatchdog = 500 * time.Millisecond
	maritime.DeadManReactuation = time.Second
	return assured.Config{
		EvidenceStore: store,
		Signer:        private,
		Anchor:        anchor,
		Provenance: audit.Provenance{
			Application:        "assured-test",
			ApplicationVersion: "1.0.0",
			Operator:           "captain-test",
			Authorization:      "test-voyage",
			Config:             map[string]any{"mapping": "test-v1"},
		},
		Maritime: maritime,
		Authority: safety.AuthorityConfig{
			// Deliberately permissive TEST double; never a production policy.
			Policy: safety.CommandPolicyFunc(func(ctx context.Context, request safety.PolicyRequest) (safety.PolicyDecision, error) {
				return safety.PolicyDecision{Permit: true, PolicyID: "test-only", Subject: "test", Authorization: "test", ExpiresAt: time.Now().Add(time.Hour)}, nil
			}),
			EngineeredSafeState: safety.VesselCommand{
				Name: "vessel.safe",
				Payload: map[string]any{
					"propulsion": 0,
					"steering":   0,
				},
			},
			CommandTTL:                   100 * time.Millisecond,
			RequireAppliedAcknowledgment: true,
		},
		Actuator:           actuator,
		CheckpointInterval: 50 * time.Millisecond,
		CheckpointEvery:    128,
		WitnessTimeout:     time.Second,
		ShutdownTimeout:    time.Second,
	}, public
}

type statefulSafePayload struct {
	mu    sync.Mutex
	calls int
}

func (payload *statefulSafePayload) MarshalJSON() ([]byte, error) {
	payload.mu.Lock()
	defer payload.mu.Unlock()
	payload.calls++
	return []byte(fmt.Sprintf(
		`{"marshal_call":%d,"propulsion":0,"steering":0}`,
		payload.calls,
	)), nil
}

func (payload *statefulSafePayload) callCount() int {
	payload.mu.Lock()
	defer payload.mu.Unlock()
	return payload.calls
}

func TestAssuredManifestRecordsEffectiveControlProfileAndFrozenSafeState(t *testing.T) {
	for _, spelling := range []string{"Safety", "Maritime"} {
		t.Run(spelling, func(t *testing.T) { testAssuredManifestProfile(t, spelling) })
	}
}

func testAssuredManifestProfile(t *testing.T, spelling string) {
	t.Helper()
	store := &syncStore{}
	anchor := &witnessAnchor{}
	actuator := &acceptingActuator{}
	config, public := validConfig(t, store, anchor, actuator)
	expected := config.Maritime
	if spelling == "Safety" {
		config.Safety, config.Maritime = expected, safety.MaritimeConfig{}
	}
	stateful := &statefulSafePayload{}
	config.Authority.EngineeredSafeState.Payload = stateful
	config.CheckpointEvery = 0

	session, err := assured.OpenSource(t.Context(), exactSource(false), config)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if calls := stateful.callCount(); calls != 1 {
		t.Fatalf("safe-state MarshalJSON calls = %d, want exactly one", calls)
	}

	_, verification, err := audit.ReadTrusted(bytes.NewReader(store.bytes()), public)
	if err != nil {
		t.Fatalf("ReadTrusted: %v", err)
	}
	if verification.Provenance == nil {
		t.Fatal("trusted manifest has no provenance")
	}
	profileValue, ok := verification.Provenance.Config[assured.ProvenanceConfigKey]
	if !ok {
		t.Fatalf("trusted manifest has no %q profile", assured.ProvenanceConfigKey)
	}
	profileJSON, err := json.Marshal(profileValue)
	if err != nil {
		t.Fatalf("marshal recorded assured profile: %v", err)
	}
	var profile struct {
		Maritime struct {
			CommandTimeout      time.Duration    `json:"command_timeout_ns"`
			TransportTimeout    time.Duration    `json:"transport_timeout_ns"`
			DeadMan             teleop.ControlID `json:"dead_man"`
			DeadManReactuation  time.Duration    `json:"dead_man_reactuation_ns"`
			LoopWatchdog        time.Duration    `json:"loop_watchdog_ns"`
			ArmStickTolerance   float32          `json:"arm_stick_tolerance"`
			ArmTriggerTolerance float32          `json:"arm_trigger_tolerance"`
		} `json:"maritime"`
		Authority struct {
			EngineeredSafeState struct {
				Name    string          `json:"name"`
				Payload json.RawMessage `json:"payload"`
			} `json:"engineered_safe_state"`
			CommandTTL                   time.Duration `json:"command_ttl_ns"`
			RequireAppliedAcknowledgment bool          `json:"require_applied_acknowledgment"`
			ClockSource                  string        `json:"clock_source"`
		} `json:"authority"`
		Evidence struct {
			FlushEveryEvent     bool          `json:"flush_every_event"`
			SynchronousAudit    bool          `json:"synchronous_audit"`
			PipelineAttestation bool          `json:"pipeline_attestation"`
			RequiredWitness     bool          `json:"required_witness"`
			CheckpointInterval  time.Duration `json:"checkpoint_interval_ns"`
			CheckpointEvery     uint64        `json:"checkpoint_every"`
			WitnessTimeout      time.Duration `json:"witness_timeout_ns"`
			ShutdownTimeout     time.Duration `json:"shutdown_timeout_ns"`
			LivenessInterval    time.Duration `json:"liveness_interval_ns"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(profileJSON, &profile); err != nil {
		t.Fatalf("decode recorded assured profile: %v", err)
	}
	if profile.Maritime.CommandTimeout != expected.CommandTimeout ||
		profile.Maritime.TransportTimeout != expected.TransportTimeout ||
		profile.Maritime.DeadMan != expected.DeadMan ||
		profile.Maritime.DeadManReactuation != expected.DeadManReactuation ||
		profile.Maritime.LoopWatchdog != expected.LoopWatchdog ||
		profile.Maritime.ArmStickTolerance != expected.ArmStickTolerance ||
		profile.Maritime.ArmTriggerTolerance != expected.ArmTriggerTolerance {
		t.Fatalf("recorded strict profile = %+v, config = %+v", profile.Maritime, expected)
	}
	wantSafePayload := `{"marshal_call":1,"propulsion":0,"steering":0}`
	if profile.Authority.EngineeredSafeState.Name != config.Authority.EngineeredSafeState.Name ||
		string(profile.Authority.EngineeredSafeState.Payload) != wantSafePayload ||
		profile.Authority.CommandTTL != config.Authority.CommandTTL ||
		!profile.Authority.RequireAppliedAcknowledgment ||
		profile.Authority.ClockSource != "system" {
		t.Fatalf("recorded authority profile = %+v", profile.Authority)
	}
	if !profile.Evidence.FlushEveryEvent || !profile.Evidence.SynchronousAudit ||
		!profile.Evidence.PipelineAttestation ||
		!profile.Evidence.RequiredWitness ||
		profile.Evidence.CheckpointInterval != config.CheckpointInterval ||
		profile.Evidence.CheckpointEvery != audit.DefaultCheckpointEvery ||
		profile.Evidence.WitnessTimeout != config.WitnessTimeout ||
		profile.Evidence.ShutdownTimeout != config.ShutdownTimeout ||
		profile.Evidence.LivenessInterval != expected.TransportTimeout/2 {
		t.Fatalf("recorded evidence profile = %+v", profile.Evidence)
	}

	commands := actuator.snapshot()
	foundSafe := false
	for _, command := range commands {
		if command.Command.Name != config.Authority.EngineeredSafeState.Name {
			continue
		}
		foundSafe = true
		if string(command.Command.Payload) != wantSafePayload {
			t.Fatalf("actuator safe payload = %s, provenance = %s", command.Command.Payload, profile.Authority.EngineeredSafeState.Payload)
		}
	}
	if !foundSafe {
		t.Fatalf("actuator commands have no engineered safe state: %+v", commands)
	}
	if calls := stateful.callCount(); calls != 1 {
		t.Fatalf("safe-state MarshalJSON calls after verification = %d, want exactly one", calls)
	}
}

func TestAssuredSessionPersistsCausalAuthorityChainAndWitnessesFooter(t *testing.T) {
	store := &syncStore{}
	anchor := &witnessAnchor{}
	actuator := &acceptingActuator{}
	config, public := validConfig(t, store, anchor, actuator)
	config.Maritime.TransportTimeout = 100 * time.Millisecond
	config.Authority.CommandTTL = 50 * time.Millisecond
	session, err := assured.OpenSource(t.Context(), exactSource(false), config)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}

	status := session.EvidenceStatus()
	if status.LocallyDurableEvents != status.AcceptedEvents || status.WitnessedEvents == 0 {
		t.Fatalf("startup evidence status = %+v", status)
	}
	subscription, err := session.Subscribe(teleop.SubscriptionOptions{Buffer: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	livenessCtx, cancelLiveness := context.WithTimeout(t.Context(), time.Second)
	defer cancelLiveness()
	for {
		event, nextErr := subscription.Next(livenessCtx)
		if nextErr != nil {
			t.Fatalf("wait for persisted transport liveness: %v", nextErr)
		}
		liveness, ok := event.(teleop.LivenessEvent)
		if ok && liveness.TransportSilenceVerifiable && liveness.TransportCheckSequence > 0 {
			break
		}
	}
	result, err := session.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{
			Name:    "propulsion.request",
			Payload: map[string]any{"throttle": 0.25},
		},
		Detail: "test command",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Fallback || result.OutcomeEvidenceID == "" {
		t.Fatalf("safe inhibited Apply result = %+v", result)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	finalStatus := session.EvidenceStatus()
	if !finalStatus.Closed || finalStatus.LastWitness == nil ||
		finalStatus.LastWitness.Checkpoint.RecordType != "footer" {
		t.Fatalf("final evidence status = %+v", finalStatus)
	}
	checkpoints := anchor.snapshot()
	if len(checkpoints) < 2 || checkpoints[len(checkpoints)-1].RecordType != "footer" {
		t.Fatalf("witness checkpoints = %+v", checkpoints)
	}
	for _, checkpoint := range checkpoints {
		if err := audit.VerifyCheckpoint(public, checkpoint); err != nil {
			t.Fatalf("verify witnessed %s: %v", checkpoint.RecordType, err)
		}
	}

	records, verification, err := audit.ReadTrusted(bytes.NewReader(store.bytes()), public)
	if err != nil {
		t.Fatalf("ReadTrusted: %v", err)
	}
	if !verification.Complete || !verification.Trusted {
		t.Fatalf("verification = %+v", verification)
	}
	assertPersistedTransportProof(t, records)
	assertAuthorityChain(t, records, result.OutcomeEvidenceID)
	commands := actuator.snapshot()
	if len(commands) < 3 {
		t.Fatalf("actuator commands = %+v", commands)
	}
	for index := 1; index < len(commands); index++ {
		if commands[index].Sequence <= commands[index-1].Sequence {
			t.Fatalf("non-monotonic actuator commands = %+v", commands)
		}
	}
}

func assertPersistedTransportProof(t *testing.T, records []audit.Record) {
	t.Helper()
	for _, persisted := range records {
		if persisted.Kind != teleop.EventLiveness {
			continue
		}
		decoded, err := audit.DecodeEvent(persisted)
		if err != nil {
			t.Fatal(err)
		}
		liveness := decoded.(*teleop.LivenessEvent)
		if liveness.TransportSilenceVerifiable && liveness.TransportCheckSequence > 0 {
			return
		}
	}
	t.Fatal("trusted audit log has no periodic transport-health proof")
}

func assertAuthorityChain(
	t *testing.T,
	records []audit.Record,
	outcomeID safety.EvidenceID,
) {
	t.Helper()
	type node struct {
		event  teleop.CommandEvent
		record safety.EvidenceRecord
	}
	nodes := make(map[string]node)
	for _, persisted := range records {
		if persisted.Kind != teleop.EventCommand {
			continue
		}
		decoded, err := audit.DecodeEvent(persisted)
		if err != nil {
			t.Fatal(err)
		}
		command := decoded.(*teleop.CommandEvent)
		var evidence safety.EvidenceRecord
		if err := json.Unmarshal(command.Payload, &evidence); err != nil {
			continue // Rumble and application command payloads use other schemas.
		}
		if evidence.Kind == "" {
			continue
		}
		nodes[command.Meta.ID.String()] = node{event: *command, record: evidence}
	}
	outcome, ok := nodes[string(outcomeID)]
	if !ok || outcome.record.Kind != safety.EvidenceAcknowledged {
		t.Fatalf("outcome %q missing from nodes", outcomeID)
	}
	sent, ok := nodes[string(outcome.record.ParentID)]
	if !ok || sent.record.Kind != safety.EvidenceSent {
		t.Fatalf("sent parent = %+v, found=%t", sent, ok)
	}
	intent, ok := nodes[string(sent.record.ParentID)]
	if !ok || intent.record.Kind != safety.EvidenceIntent {
		t.Fatalf("intent parent = %+v, found=%t", intent, ok)
	}
	decision, ok := nodes[string(intent.record.ParentID)]
	if !ok || decision.record.Kind != safety.EvidenceDecision {
		t.Fatalf("decision parent = %+v, found=%t", decision, ok)
	}
	for _, child := range []node{outcome, sent, intent} {
		if len(child.event.Meta.Causes) == 0 ||
			child.event.Meta.Causes[len(child.event.Meta.Causes)-1].String() != string(child.record.ParentID) {
			t.Fatalf("audit cause does not carry evidence parent: %+v", child)
		}
	}
}

func TestAssuredSessionRejectsMissingGuarantees(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*assured.Config)
		want   error
	}{
		{
			name:   "command policy",
			mutate: func(config *assured.Config) { config.Authority.Policy = nil },
			want:   assured.ErrInvalidConfig,
		},
		{
			name:   "evidence store",
			mutate: func(config *assured.Config) { config.EvidenceStore = nil },
			want:   assured.ErrInvalidConfig,
		},
		{
			name:   "signer",
			mutate: func(config *assured.Config) { config.Signer = nil },
			want:   assured.ErrInvalidConfig,
		},
		{
			name:   "external anchor",
			mutate: func(config *assured.Config) { config.Anchor = nil },
			want:   assured.ErrInvalidConfig,
		},
		{
			name: "complete provenance",
			mutate: func(config *assured.Config) {
				config.Provenance.Authorization = ""
			},
			want: assured.ErrInvalidConfig,
		},
		{
			name: "bounded actuator lease",
			mutate: func(config *assured.Config) {
				config.Authority.CommandTTL = config.Maritime.CommandTimeout + time.Millisecond
			},
			want: assured.ErrInvalidConfig,
		},
		{
			name: "applied acknowledgement",
			mutate: func(config *assured.Config) {
				config.Authority.RequireAppliedAcknowledgment = false
			},
			want: assured.ErrInvalidConfig,
		},
		{
			name: "custom authority clock",
			mutate: func(config *assured.Config) {
				config.Authority.Now = time.Now
			},
			want: assured.ErrInvalidConfig,
		},
		{
			name: "representable engineered safe state",
			mutate: func(config *assured.Config) {
				config.Authority.EngineeredSafeState.Payload = make(chan struct{})
			},
			want: assured.ErrInvalidConfig,
		},
		{
			name: "reserved assured provenance profile",
			mutate: func(config *assured.Config) {
				config.Provenance.Config[assured.ProvenanceConfigKey] = map[string]any{
					"forged": true,
				}
			},
			want: assured.ErrInvalidConfig,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &syncStore{}
			config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
			test.mutate(&config)
			_, err := assured.OpenSource(t.Context(), exactSource(false), config)
			if !errors.Is(err, test.want) {
				t.Fatalf("OpenSource error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestAssuredSessionRejectsSampledAndUnverifiableSources(t *testing.T) {
	for _, spelling := range []string{"Safety", "Maritime"} {
		t.Run(spelling, func(t *testing.T) { testAssuredRejectsSources(t, spelling) })
	}
}

func testAssuredRejectsSources(t *testing.T, spelling string) {
	t.Helper()
	t.Run("sampled audit grade", func(t *testing.T) {
		store := &syncStore{}
		config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
		if spelling == "Safety" {
			config.Safety, config.Maritime = config.Maritime, safety.MaritimeConfig{}
		}
		source := exactSource(false)
		defer source.Close()
		descriptor := source.Descriptor()
		sampled := testkit.NewFakeSource(teleop.Descriptor{
			ID:         descriptor.ID,
			Capability: teleop.Capabilities{AuditGrade: teleop.AuditSampledState},
		}, 1)
		defer sampled.Close()
		sampled.SetTransportHealth(teleop.TransportHealth{SilenceVerifiable: true})
		_, err := assured.OpenSource(t.Context(), sampled, config)
		if !errors.Is(err, assured.ErrAuditGrade) {
			t.Fatalf("OpenSource error = %v", err)
		}
	})

	t.Run("unverifiable transport", func(t *testing.T) {
		store := &syncStore{}
		config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
		config.Maritime.TransportTimeout = 75 * time.Millisecond
		config.Authority.CommandTTL = 50 * time.Millisecond
		if spelling == "Safety" {
			config.Safety, config.Maritime = config.Maritime, safety.MaritimeConfig{}
		}
		source := exactSource(false)
		source.SetTransportHealth(teleop.TransportHealth{})
		_, err := assured.OpenSource(t.Context(), source, config)
		if !errors.Is(err, assured.ErrTransportUnverifiable) {
			t.Fatalf("OpenSource error = %v", err)
		}
	})
}

type activatingHealthSource struct {
	*testkit.FakeSource
	once sync.Once
}

func (source *activatingHealthSource) Read(ctx context.Context) (teleop.Observation, error) {
	source.once.Do(func() {
		source.SetTransportHealth(teleop.TransportHealth{
			Sequence:          1,
			CheckedAt:         time.Now(),
			Connected:         true,
			SilenceVerifiable: true,
		})
	})
	return source.FakeSource.Read(ctx)
}

func TestOpenSourceAcceptsTransportProofEstablishedAfterReadStarts(t *testing.T) {
	store := &syncStore{}
	config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
	base := exactSource(false)
	base.SetTransportHealth(teleop.TransportHealth{})
	source := &activatingHealthSource{FakeSource: base}
	session, err := assured.OpenSource(t.Context(), source, config)
	if err != nil {
		t.Fatalf("OpenSource with post-open transport proof: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArmRefreshesWatchdogAfterSlowStartupWitness(t *testing.T) {
	store := &syncStore{}
	anchor := &delayedAnchor{witnessAnchor: &witnessAnchor{}, delay: 50 * time.Millisecond}
	config, _ := validConfig(t, store, anchor, &acceptingActuator{})
	config.Maritime.LoopWatchdog = 20 * time.Millisecond
	config.Authority.CommandTTL = 10 * time.Millisecond
	config.CheckpointInterval = time.Second
	source := exactSource(false)
	if err := source.Push(t.Context(), teleop.State{}); err != nil {
		t.Fatal(err)
	}
	session, err := assured.OpenSource(t.Context(), source, config)
	if err != nil {
		t.Fatal(err)
	}
	// Startup witnessing deliberately outlasted LoopWatchdog. Strict Arm's
	// evidenced safe-state preflight must refresh it without exposing Heartbeat.
	if err := session.Arm(t.Context(), "captain ready after witness"); err != nil {
		t.Fatalf("Arm after slow witness: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

type hidingProvider struct{ source teleop.InputSource }

func (provider hidingProvider) Type() teleop.ControllerType { return "assured-test" }

func (provider hidingProvider) Discover(context.Context) ([]teleop.Descriptor, error) {
	return []teleop.Descriptor{provider.source.Descriptor()}, nil
}

func (provider hidingProvider) Open(
	_ context.Context,
	_ teleop.DeviceID,
	options ...teleop.OpenOption,
) (teleop.GameController, error) {
	controller, err := teleop.NewController(provider.source, options...)
	if err != nil {
		return nil, err
	}
	return gameControllerOnly{GameController: controller}, nil
}

type gameControllerOnly struct{ teleop.GameController }

type completeControllerWrapper struct{ *teleop.Controller }

type wrappingProvider struct{ source teleop.InputSource }

func (provider wrappingProvider) Type() teleop.ControllerType { return "assured-wrapper-test" }

func (provider wrappingProvider) Discover(context.Context) ([]teleop.Descriptor, error) {
	return []teleop.Descriptor{provider.source.Descriptor()}, nil
}

func (provider wrappingProvider) Open(
	_ context.Context,
	_ teleop.DeviceID,
	options ...teleop.OpenOption,
) (teleop.GameController, error) {
	controller, err := teleop.NewController(provider.source, options...)
	if err != nil {
		return nil, err
	}
	return &completeControllerWrapper{Controller: controller}, nil
}

type directProvider struct{ source teleop.InputSource }

func (provider directProvider) Type() teleop.ControllerType { return "assured-direct-test" }

func (provider directProvider) Discover(context.Context) ([]teleop.Descriptor, error) {
	return []teleop.Descriptor{provider.source.Descriptor()}, nil
}

func (provider directProvider) Open(
	_ context.Context,
	_ teleop.DeviceID,
	options ...teleop.OpenOption,
) (teleop.GameController, error) {
	return teleop.NewController(provider.source, options...)
}

type selectiveOptionProvider struct {
	source        teleop.InputSource
	omit          int
	override      bool
	overrideClock bool
}

type providerTestClock struct{}

func (providerTestClock) Now() time.Time { return time.Now() }

func (providerTestClock) NewTicker(interval time.Duration) teleop.Ticker {
	return providerTestTicker{Ticker: time.NewTicker(interval)}
}

type providerTestTicker struct{ *time.Ticker }

func (ticker providerTestTicker) C() <-chan time.Time { return ticker.Ticker.C }

func (provider selectiveOptionProvider) Type() teleop.ControllerType {
	return "assured-selective-option-test"
}

func (provider selectiveOptionProvider) Discover(context.Context) ([]teleop.Descriptor, error) {
	return []teleop.Descriptor{provider.source.Descriptor()}, nil
}

func (provider selectiveOptionProvider) Open(
	_ context.Context,
	_ teleop.DeviceID,
	options ...teleop.OpenOption,
) (teleop.GameController, error) {
	selected := make([]teleop.OpenOption, 0, len(options)+1)
	for index, option := range options {
		if index != provider.omit {
			selected = append(selected, option)
		}
	}
	if provider.override {
		// Apply after Assured's seal to model a provider that first accepts the
		// requested policy and then weakens the effective controller.
		selected = append(selected, teleop.WithLiveness(0, 0))
	}
	if provider.overrideClock {
		selected = append(selected, teleop.WithClock(providerTestClock{}))
	}
	return teleop.NewController(provider.source, selected...)
}

type capturingProvider struct {
	source     teleop.InputSource
	controller *teleop.Controller
}

func (provider *capturingProvider) Type() teleop.ControllerType {
	return "assured-capturing-test"
}

func (provider *capturingProvider) Discover(context.Context) ([]teleop.Descriptor, error) {
	return []teleop.Descriptor{provider.source.Descriptor()}, nil
}

func (provider *capturingProvider) Open(
	_ context.Context,
	_ teleop.DeviceID,
	options ...teleop.OpenOption,
) (teleop.GameController, error) {
	controller, err := teleop.NewController(provider.source, options...)
	provider.controller = controller
	return controller, err
}

var errProviderOpen = errors.New("test provider open failed")

type failingOpenProvider struct{}

func (failingOpenProvider) Type() teleop.ControllerType { return "assured-failing-test" }

func (failingOpenProvider) Discover(context.Context) ([]teleop.Descriptor, error) {
	return nil, nil
}

func (failingOpenProvider) Open(
	context.Context,
	teleop.DeviceID,
	...teleop.OpenOption,
) (teleop.GameController, error) {
	return nil, errProviderOpen
}

func TestOpenProviderSupportsConcreteControllerContract(t *testing.T) {
	store := &syncStore{}
	config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
	source := exactSource(false)
	session, err := assured.OpenProvider(
		t.Context(),
		directProvider{source: source},
		source.Descriptor().ID,
		config,
	)
	if err != nil {
		t.Fatalf("OpenProvider: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenProviderRejectsConcreteControllerWithUnattestedPipeline(t *testing.T) {
	for _, test := range []struct {
		name          string
		omit          int
		override      bool
		overrideClock bool
	}{
		{name: "audit sink", omit: 0},
		{name: "synchronous audit", omit: 1},
		{name: "authority processor", omit: 2},
		{name: "liveness", omit: 3},
		{name: "shutdown timeout", omit: 4},
		{name: "attestation seal", omit: 6},
		{name: "post-seal override", omit: -1, override: true},
		{name: "post-seal custom clock", omit: -1, overrideClock: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &syncStore{}
			config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
			// Differ from Controller's default so omission cannot accidentally
			// produce the expected effective shutdown setting.
			config.ShutdownTimeout = 750 * time.Millisecond
			source := exactSource(false)
			_, err := assured.OpenProvider(
				t.Context(),
				selectiveOptionProvider{
					source:        source,
					omit:          test.omit,
					override:      test.override,
					overrideClock: test.overrideClock,
				},
				source.Descriptor().ID,
				config,
			)
			if !errors.Is(err, assured.ErrControllerContract) ||
				!errors.Is(err, teleop.ErrPipelineAttestation) {
				t.Fatalf("OpenProvider error = %v, want controller-contract attestation failure", err)
			}
		})
	}
}

func TestOpenProviderRejectsWrapperWithoutSynchronousEvidenceBarrier(t *testing.T) {
	store := &syncStore{}
	config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
	source := exactSource(false)
	_, err := assured.OpenProvider(
		t.Context(),
		hidingProvider{source: source},
		source.Descriptor().ID,
		config,
	)
	if !errors.Is(err, assured.ErrControllerContract) {
		t.Fatalf("OpenProvider error = %v", err)
	}
}

func TestOpenProviderRejectsStructurallyCompleteControllerWrapper(t *testing.T) {
	store := &syncStore{}
	config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
	source := exactSource(false)
	_, err := assured.OpenProvider(
		t.Context(),
		wrappingProvider{source: source},
		source.Descriptor().ID,
		config,
	)
	if !errors.Is(err, assured.ErrControllerContract) {
		t.Fatalf("OpenProvider error = %v, want exact-controller contract rejection", err)
	}
}

type publicPanicSigner struct{}

func (publicPanicSigner) Public() crypto.PublicKey { panic("signer public key fault") }

func (publicPanicSigner) Sign(
	io.Reader,
	[]byte,
	crypto.SignerOpts,
) ([]byte, error) {
	return nil, errors.New("unexpected Sign call")
}

type panicJSON struct{}

func (panicJSON) MarshalJSON() ([]byte, error) { panic("configuration marshal fault") }

func TestAssuredConfigCallbackPanicsAreReturnedBeforeOwnership(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		mutate func(*assured.Config)
	}{
		{
			name: "signer public key",
			mutate: func(config *assured.Config) {
				config.Signer = publicPanicSigner{}
			},
		},
		{
			name: "provenance JSON",
			mutate: func(config *assured.Config) {
				config.Provenance.Config = map[string]any{"panic": panicJSON{}}
			},
		},
		{
			name: "safe-state JSON",
			mutate: func(config *assured.Config) {
				config.Authority.EngineeredSafeState.Payload = panicJSON{}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := &syncStore{}
			config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
			test.mutate(&config)
			_, err := assured.OpenSource(t.Context(), exactSource(false), config)
			if !errors.Is(err, assured.ErrInvalidConfig) ||
				!errors.Is(err, teleop.ErrCallbackPanic) {
				t.Fatalf("OpenSource error = %v, want invalid-config callback panic", err)
			}
			store.mu.Lock()
			closed := store.closed
			store.mu.Unlock()
			if closed {
				t.Fatal("pre-validation failure incorrectly transferred store ownership")
			}
		})
	}
}

func TestAssuredCloseReportsStorePanicsAndAttemptsClose(t *testing.T) {
	store := newArmablePanicStore()
	config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
	session, err := assured.OpenSource(t.Context(), exactSource(false), config)
	if err != nil {
		t.Fatal(err)
	}
	store.arm()
	if err := session.Close(); !errors.Is(err, teleop.ErrCallbackPanic) {
		t.Fatalf("Close error = %v, want evidence-store callback panic", err)
	}
	if _, closes := store.calls(); closes == 0 {
		t.Fatal("store Close was skipped after Sync panic")
	}
}

func TestAssuredRollbackReportsStorePanicsAndAttemptsClose(t *testing.T) {
	store := newArmablePanicStore()
	config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
	store.arm()
	_, err := assured.OpenProvider(t.Context(), failingOpenProvider{}, "failing:0", config)
	if !errors.Is(err, errProviderOpen) || !errors.Is(err, teleop.ErrCallbackPanic) {
		t.Fatalf("OpenProvider error = %v, want provider and store callback failures", err)
	}
	if _, closes := store.calls(); closes == 0 {
		t.Fatal("rollback skipped store Close after Sync panic")
	}
}

func TestAssuredStartupFailsClosedOnActuatorOrRecorderFailure(t *testing.T) {
	t.Run("actuator", func(t *testing.T) {
		store := &syncStore{}
		actuator := &acceptingActuator{failSend: true}
		config, _ := validConfig(t, store, &witnessAnchor{}, actuator)
		_, err := assured.OpenSource(t.Context(), exactSource(false), config)
		if !errors.Is(err, safety.ErrActuatorUncertain) {
			t.Fatalf("OpenSource error = %v", err)
		}
		commands := actuator.snapshot()
		if len(commands) == 0 {
			t.Fatal("startup made no engineered-safe-state attempt")
		}
		for _, command := range commands {
			if !command.Fallback || command.Command.Name != "vessel.safe" {
				t.Fatalf("unsafe startup command = %+v", command)
			}
		}
	})

	t.Run("recorder", func(t *testing.T) {
		store := &syncStore{failAt: 1}
		actuator := &acceptingActuator{}
		config, _ := validConfig(t, store, &witnessAnchor{}, actuator)
		_, err := assured.OpenSource(t.Context(), exactSource(false), config)
		if !errors.Is(err, errStoreSync) {
			t.Fatalf("OpenSource error = %v", err)
		}
		for _, command := range actuator.snapshot() {
			if !command.Fallback {
				t.Fatalf("recorder failure transmitted requested command = %+v", command)
			}
		}
	})

	t.Run("acknowledgement without application proof", func(t *testing.T) {
		store := &syncStore{}
		actuator := &acceptingActuator{zeroTime: true}
		config, _ := validConfig(t, store, &witnessAnchor{}, actuator)
		_, err := assured.OpenSource(t.Context(), exactSource(false), config)
		if !errors.Is(err, safety.ErrActuatorUncertain) {
			t.Fatalf("OpenSource error = %v", err)
		}
		for _, command := range actuator.snapshot() {
			if !command.Fallback {
				t.Fatalf("unproved acknowledgement transmitted requested command = %+v", command)
			}
		}
	})
}

func TestSetRumbleStopsOutputWhenOutcomeCannotBeEvidenced(t *testing.T) {
	store := &syncStore{}
	config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
	source := exactSource(true)
	session, err := assured.OpenSource(t.Context(), source, config)
	if err != nil {
		t.Fatal(err)
	}
	store.failAfter(2) // attempt persists; outcome sync fails.
	err = session.SetRumble(t.Context(), teleop.Rumble{
		LowFrequency:  0.75,
		HighFrequency: 0.25,
	})
	if !errors.Is(err, errStoreSync) {
		t.Fatalf("SetRumble error = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for source.Rumble() != (teleop.Rumble{}) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := source.Rumble(); got != (teleop.Rumble{}) {
		t.Fatalf("rumble after evidence failure = %+v", got)
	}
	_ = session.Close()
}

type blockingRumbleSource struct {
	*testkit.FakeSource
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (source *blockingRumbleSource) SetRumble(ctx context.Context, rumble teleop.Rumble) error {
	if rumble != (teleop.Rumble{}) {
		source.once.Do(func() { close(source.entered) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-source.release:
		}
	}
	return source.FakeSource.SetRumble(ctx, rumble)
}

func TestCloseSerializesWithInflightRumbleAndLeavesOutputStopped(t *testing.T) {
	store := &syncStore{}
	config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
	source := &blockingRumbleSource{
		FakeSource: exactSource(true),
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	session, err := assured.OpenSource(t.Context(), source, config)
	if err != nil {
		t.Fatal(err)
	}
	rumbleDone := make(chan error, 1)
	go func() {
		rumbleDone <- session.SetRumble(context.Background(), teleop.Rumble{LowFrequency: 1})
	}()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("rumble did not reach source")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- session.Close() }()
	close(source.release)
	if err := <-rumbleDone; err != nil {
		t.Fatalf("SetRumble: %v", err)
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not finish")
	}
	if got := source.Rumble(); got != (teleop.Rumble{}) {
		t.Fatalf("rumble after serialized Close = %+v", got)
	}
}

func TestDonePublishesFinalCloseError(t *testing.T) {
	closeFailure := errors.New("test final store close failed")
	store := &syncStore{closeErr: closeFailure}
	config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
	session, err := assured.OpenSource(t.Context(), exactSource(false), config)
	if err != nil {
		t.Fatal(err)
	}

	// Done is the public completion barrier. Every observer released by it must
	// see the final error, including observers that never called Close and thus
	// do not share sync.Once's separate synchronization edge.
	const observers = 64
	observed := make(chan error, observers)
	for range observers {
		go func() {
			<-session.Done()
			observed <- session.Err()
		}()
	}

	if err := session.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("Close error = %v", err)
	}
	for range observers {
		if err := <-observed; !errors.Is(err, closeFailure) {
			t.Fatalf("Err after Done = %v", err)
		}
	}
}

func TestCloseRetriesOnTerminalStreamWhenControllerStopsDuringAuthorityClose(t *testing.T) {
	store := &syncStore{}
	anchor := &witnessAnchor{}
	actuator := &closeRaceActuator{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	config, public := validConfig(t, store, anchor, actuator)
	source := exactSource(false)
	provider := &capturingProvider{source: source}
	session, err := assured.OpenProvider(t.Context(), provider, source.Descriptor().ID, config)
	if err != nil {
		t.Fatal(err)
	}

	controllerStopped := make(chan struct{})
	go func() {
		<-actuator.entered
		_ = source.Close()
		<-provider.controller.Done()
		close(controllerStopped)
		close(actuator.release)
	}()
	closeErr := session.Close()
	<-controllerStopped
	if closeErr == nil {
		t.Fatal("Close hid the first uncertain authority transaction")
	}

	commands := actuator.snapshot()
	if len(commands) < 3 || !commands[len(commands)-1].Fallback ||
		commands[len(commands)-1].Sequence <= commands[len(commands)-2].Sequence {
		t.Fatalf("terminal retry commands = %+v", commands)
	}
	records, verification, err := audit.ReadTrusted(bytes.NewReader(store.bytes()), public)
	if err != nil {
		t.Fatalf("ReadTrusted terminal retry evidence: %v", err)
	}
	if !verification.Complete || !verification.Trusted {
		t.Fatalf("terminal retry verification = %+v", verification)
	}
	assertTerminalCloseEvidence(t, records)
}

func TestAssuredSessionAutoFinalizesWhenControllerStops(t *testing.T) {
	store := &syncStore{}
	anchor := &witnessAnchor{}
	config, public := validConfig(t, store, anchor, &acceptingActuator{})
	source := exactSource(false)
	session, err := assured.OpenSource(t.Context(), source, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session did not auto-finalize")
	}
	if status := session.EvidenceStatus(); !status.Closed {
		t.Fatalf("auto-finalized status = %+v", status)
	}
	records, verification, err := audit.ReadTrusted(bytes.NewReader(store.bytes()), public)
	if err != nil {
		t.Fatalf("ReadTrusted auto-finalized evidence: %v", err)
	}
	if !verification.Complete || !verification.Trusted {
		t.Fatalf("auto-finalized verification = %+v", verification)
	}
	assertTerminalCloseEvidence(t, records)
}

func assertTerminalCloseEvidence(t *testing.T, records []audit.Record) {
	t.Helper()
	identities := make(map[teleop.EventID]struct{}, len(records))
	var terminalSequences []uint64
	var closeAttempt, closeOutcome, safeOutcome bool
	for _, persisted := range records {
		if _, duplicate := identities[persisted.Header.ID]; duplicate {
			t.Fatalf("duplicate event identity %s", persisted.Header.ID.String())
		}
		identities[persisted.Header.ID] = struct{}{}
		if persisted.Header.ID.Stream != "assured-terminal" {
			continue
		}
		terminalSequences = append(terminalSequences, persisted.Header.ID.Sequence)
		if persisted.Kind != teleop.EventCommand {
			t.Fatalf("terminal stream contains %s", persisted.Kind)
		}
		decoded, err := audit.DecodeEvent(persisted)
		if err != nil {
			t.Fatal(err)
		}
		command := decoded.(*teleop.CommandEvent)
		var evidence safety.EvidenceRecord
		if err := json.Unmarshal(command.Payload, &evidence); err != nil {
			t.Fatalf("decode terminal evidence: %v", err)
		}
		switch {
		case evidence.Kind == safety.EvidenceLifecycleAttempt &&
			evidence.Lifecycle != nil && evidence.Lifecycle.Action == safety.LifecycleClose:
			closeAttempt = true
		case evidence.Kind == safety.EvidenceLifecycleOutcome &&
			evidence.Lifecycle != nil && evidence.Lifecycle.Action == safety.LifecycleClose &&
			evidence.Lifecycle.Accepted:
			closeOutcome = true
		case evidence.Kind == safety.EvidenceAcknowledged &&
			evidence.Actuator != nil && evidence.Actuator.Fallback &&
			evidence.Acknowledgment != nil && evidence.Acknowledgment.Accepted:
			safeOutcome = true
		}
	}
	if len(terminalSequences) == 0 {
		t.Fatal("auto-finalized log has no assured-terminal evidence")
	}
	for index, sequence := range terminalSequences {
		if sequence != uint64(index+1) {
			t.Fatalf("terminal sequences = %v", terminalSequences)
		}
	}
	if !closeAttempt || !closeOutcome || !safeOutcome {
		t.Fatalf(
			"terminal evidence: close attempt=%t outcome=%t safe outcome=%t",
			closeAttempt,
			closeOutcome,
			safeOutcome,
		)
	}
}

func ExampleOpenSource() {
	// Production applications provide an exact-stream source, a sync-capable
	// store, a protected Ed25519 signer, an independently administered Anchor,
	// and an actuator that enforces sequence numbers and command leases. The
	// returned Session exposes no raw output path around safety.Authority.
	fmt.Println("assured sessions require explicit safety and evidence guarantees")
	// Output: assured sessions require explicit safety and evidence guarantees
}
