package safety_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/safety"
	"github.com/open-ships/teleop/testkit"
)

type authorityTestSource struct {
	mu      sync.Mutex
	session teleop.SessionID
	state   teleop.State
	meta    teleop.StateMeta
	now     time.Duration
	done    chan struct{}
	caps    teleop.Capabilities
}

func newAuthorityTestSource() *authorityTestSource {
	var session teleop.SessionID
	session[0] = 0xa7
	return &authorityTestSource{
		session: session,
		meta: teleop.StateMeta{
			Sequence:                    1,
			Connected:                   true,
			TransportSilenceVerifiable:  true,
			TransportCheckSequence:      1,
			LastTransportCheckMonotonic: 0,
		},
		done: make(chan struct{}),
	}
}

func (source *authorityTestSource) SnapshotWithMeta() (teleop.State, teleop.StateMeta) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.state.Clone(), source.meta
}

func (source *authorityTestSource) Monotonic() time.Duration {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.now
}

func (source *authorityTestSource) Done() <-chan struct{} { return source.done }

func (source *authorityTestSource) Session() teleop.SessionID { return source.session }

func (source *authorityTestSource) Capabilities() teleop.Capabilities {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.caps.Clone()
}

func (source *authorityTestSource) setState(state teleop.State) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.state = state.Clone()
	source.meta.Sequence++
	source.meta.ReceivedMonotonic = source.now
	source.meta.LastTransportCheckMonotonic = source.now
	source.meta.TransportCheckSequence++
}

type memoryEvidence struct {
	mu      sync.Mutex
	records []safety.EvidenceRecord
	fail    bool
}

type blockingEvidence struct {
	base    memoryEvidence
	block   func(safety.EvidenceRecord) bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type oneShotClockBarrier struct {
	enabled atomic.Bool
	entered chan struct{}
	release chan struct{}
	exited  chan struct{}
}

func newOneShotClockBarrier() *oneShotClockBarrier {
	return &oneShotClockBarrier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		exited:  make(chan struct{}),
	}
}

func (clock *oneShotClockBarrier) Now() time.Time {
	if clock.enabled.CompareAndSwap(true, false) {
		close(clock.entered)
		<-clock.release
		close(clock.exited)
	}
	return time.Now()
}

func (evidence *blockingEvidence) Commit(
	ctx context.Context,
	record safety.EvidenceRecord,
) (safety.EvidenceID, error) {
	if evidence.block != nil && evidence.block(record) {
		evidence.once.Do(func() { close(evidence.entered) })
		<-evidence.release // Deliberately model an adapter that ignores ctx.
	}
	return evidence.base.Commit(ctx, record)
}

func (evidence *memoryEvidence) Commit(
	ctx context.Context,
	record safety.EvidenceRecord,
) (safety.EvidenceID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	evidence.mu.Lock()
	defer evidence.mu.Unlock()
	if evidence.fail {
		return "", errors.New("evidence medium unavailable")
	}
	evidence.records = append(evidence.records, record.Clone())
	return safety.EvidenceID(fmt.Sprintf("evidence-%03d", len(evidence.records))), nil
}

func (evidence *memoryEvidence) setFailure(fail bool) {
	evidence.mu.Lock()
	evidence.fail = fail
	evidence.mu.Unlock()
}

func (evidence *memoryEvidence) snapshot() []safety.EvidenceRecord {
	evidence.mu.Lock()
	defer evidence.mu.Unlock()
	result := make([]safety.EvidenceRecord, len(evidence.records))
	for index, record := range evidence.records {
		result[index] = record.Clone()
	}
	return result
}

type actuatorStep uint8

const (
	actuatorAccept actuatorStep = iota
	actuatorSendFailure
	actuatorAwaitFailure
	actuatorReject
	actuatorAwaitCancellation
	actuatorAcceptWithoutApplied
	actuatorSendPanic
	actuatorAwaitPanic
)

// fakeActuator is the deterministic second adapter at the Actuator seam. It
// verifies Authority behavior independently of ActuatorFunc.
type fakeActuator struct {
	mu       sync.Mutex
	steps    []actuatorStep
	commands []safety.ActuatorCommand
	notify   chan struct{}
}

func newFakeActuator() *fakeActuator {
	return &fakeActuator{notify: make(chan struct{}, 64)}
}

func (actuator *fakeActuator) enqueue(steps ...actuatorStep) {
	actuator.mu.Lock()
	actuator.steps = append(actuator.steps, steps...)
	actuator.mu.Unlock()
}

func (actuator *fakeActuator) Send(
	_ context.Context,
	command safety.ActuatorCommand,
) (safety.ActuatorReceipt, error) {
	actuator.mu.Lock()
	step := actuatorAccept
	if len(actuator.steps) > 0 {
		step = actuator.steps[0]
		actuator.steps = actuator.steps[1:]
	}
	actuator.commands = append(actuator.commands, command.Clone())
	actuator.mu.Unlock()
	select {
	case actuator.notify <- struct{}{}:
	default:
	}
	if step == actuatorSendFailure {
		return nil, errors.New("transport write failed")
	}
	if step == actuatorSendPanic {
		panic("test actuator send panic")
	}
	return safety.ReceiptFunc(func(ctx context.Context) (safety.ActuatorAcknowledgment, error) {
		switch step {
		case actuatorAwaitFailure:
			return safety.ActuatorAcknowledgment{}, context.DeadlineExceeded
		case actuatorAwaitCancellation:
			<-ctx.Done()
			return safety.ActuatorAcknowledgment{}, ctx.Err()
		case actuatorReject:
			return safety.ActuatorAcknowledgment{
				ControllerSession: command.ControllerSession,
				Sequence:          command.Sequence,
				Detail:            "actuator interlock open",
			}, nil
		case actuatorAcceptWithoutApplied:
			return safety.ActuatorAcknowledgment{
				ControllerSession: command.ControllerSession,
				Sequence:          command.Sequence,
				Accepted:          true,
			}, nil
		case actuatorAwaitPanic:
			panic("test actuator receipt panic")
		default:
			return safety.ActuatorAcknowledgment{
				ControllerSession: command.ControllerSession,
				Sequence:          command.Sequence,
				Accepted:          true,
				AppliedAt:         command.IssuedAt,
			}, nil
		}
	}), nil
}

func (actuator *fakeActuator) snapshot() []safety.ActuatorCommand {
	actuator.mu.Lock()
	defer actuator.mu.Unlock()
	result := make([]safety.ActuatorCommand, len(actuator.commands))
	for index, command := range actuator.commands {
		result[index] = command.Clone()
	}
	return result
}

func newTestAuthority(
	t *testing.T,
	evidence *memoryEvidence,
	actuator safety.Actuator,
) (*safety.Authority, *authorityTestSource) {
	t.Helper()
	authority, err := safety.NewAuthority(
		safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{
				Name:    "vessel.safe",
				Payload: map[string]any{"thrust": 0, "brake": true},
			},
			CommandTTL:                   time.Second,
			RequireAppliedAcknowledgment: true,
		},
		evidence,
		actuator,
		safety.WithCommandTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	source := newAuthorityTestSource()
	if err := authority.Bind(t.Context(), source); err != nil {
		t.Fatalf("bind authority: %v", err)
	}
	return authority, source
}

func TestAuthorityDurablyChainsDecisionIntentSendAndAcknowledgment(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, _ := newTestAuthority(t, evidence, actuator)
	if err := authority.Arm(t.Context(), "captain took control"); err != nil {
		t.Fatal(err)
	}

	var causeSession teleop.SessionID
	causeSession[0] = 0x42
	cause := teleop.EventID{Session: causeSession, Stream: "action", Sequence: 9}
	result, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{
			Name:    "propulsion.set",
			Payload: map[string]any{"port": 0.25, "starboard": 0.25},
		},
		Causes: []teleop.EventID{cause},
		Detail: "helm loop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Decision.Permit || result.Fallback || result.Acknowledgment == nil {
		t.Fatalf("apply result = %+v", result)
	}
	if result.DecisionEvidenceID == "" || result.IntentEvidenceID == "" ||
		result.OutcomeEvidenceID == "" {
		t.Fatalf("evidence identities = %+v", result)
	}

	records := evidence.snapshot()
	if len(records) < 4 {
		t.Fatalf("evidence records = %d", len(records))
	}
	wantKinds := []safety.EvidenceKind{
		safety.EvidenceDecision,
		safety.EvidenceIntent,
		safety.EvidenceSent,
		safety.EvidenceAcknowledged,
	}
	got := records[len(records)-4:]
	for index, want := range wantKinds {
		if got[index].Kind != want {
			t.Fatalf("record %d kind = %q, want %q", index, got[index].Kind, want)
		}
	}
	if got[1].ParentID != result.DecisionEvidenceID ||
		got[2].ParentID != result.IntentEvidenceID ||
		got[3].DecisionID != result.DecisionEvidenceID {
		t.Fatalf("causal evidence chain = %+v", got)
	}
	for _, record := range got {
		if !slices.Contains(record.Causes, cause) && record.Kind != safety.EvidenceDecision {
			t.Fatalf("record %q lost controller cause", record.Kind)
		}
	}

	commands := actuator.snapshot()
	command := commands[len(commands)-1]
	if command.Fallback || command.Sequence != result.CommandSequence ||
		command.DecisionID != result.DecisionEvidenceID ||
		command.IntentID != result.IntentEvidenceID || command.TTL <= 0 {
		t.Fatalf("actuator command = %+v", command)
	}
}

func TestAuthorityAutomaticallyCausesLiveEvidenceFromExactObservation(t *testing.T) {
	tests := []struct {
		name       string
		causes     func(teleop.EventID, teleop.EventID) []teleop.EventID
		wantCauses func(teleop.EventID, teleop.EventID) []teleop.EventID
	}{
		{
			name: "append after caller cause",
			causes: func(_ teleop.EventID, caller teleop.EventID) []teleop.EventID {
				return []teleop.EventID{caller}
			},
			wantCauses: func(exact, caller teleop.EventID) []teleop.EventID {
				return []teleop.EventID{caller, exact}
			},
		},
		{
			name: "deduplicate caller exact cause",
			causes: func(exact, caller teleop.EventID) []teleop.EventID {
				return []teleop.EventID{exact, caller}
			},
			wantCauses: func(exact, caller teleop.EventID) []teleop.EventID {
				return []teleop.EventID{exact, caller}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := &memoryEvidence{}
			actuator := newFakeActuator()
			authority, source := newTestAuthority(t, evidence, actuator)
			if err := authority.Arm(t.Context(), "ready"); err != nil {
				t.Fatal(err)
			}
			_, meta := source.SnapshotWithMeta()
			exact := teleop.EventID{
				Session:  source.Session(),
				Stream:   "input",
				Sequence: meta.Sequence,
			}
			caller := teleop.EventID{
				Session:  source.Session(),
				Stream:   "action",
				Sequence: 7,
			}
			baseline := len(evidence.snapshot())
			result, err := authority.Apply(t.Context(), safety.ApplyRequest{
				Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.4},
				Causes: test.causes(exact, caller),
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Decision.Permit || result.Decision.InputSequence != exact.Sequence {
				t.Fatalf("live decision = %+v", result.Decision)
			}
			got := evidence.snapshot()[baseline:]
			if len(got) != 4 {
				t.Fatalf("Apply evidence records = %d, want 4", len(got))
			}
			want := test.wantCauses(exact, caller)
			for _, record := range got {
				if !slices.Equal(record.Causes, want) {
					t.Fatalf("%s causes = %+v, want %+v", record.Kind, record.Causes, want)
				}
			}
		})
	}
}

func TestAuthorityPreservesExactObservationCauseOnActuatorFailure(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, source := newTestAuthority(t, evidence, actuator)
	if err := authority.Arm(t.Context(), "ready"); err != nil {
		t.Fatal(err)
	}
	actuator.enqueue(actuatorSendFailure, actuatorAccept)
	_, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.4},
	})
	if !errors.Is(err, safety.ErrActuatorUncertain) {
		t.Fatalf("Apply error = %v", err)
	}
	_, meta := source.SnapshotWithMeta()
	exact := teleop.EventID{
		Session:  source.Session(),
		Stream:   "input",
		Sequence: meta.Sequence,
	}
	for _, record := range evidence.snapshot() {
		if record.Kind != safety.EvidenceFailure || record.Applied == nil ||
			record.Applied.Name != "propulsion.live" {
			continue
		}
		if !slices.Contains(record.Causes, exact) {
			t.Fatalf("failure evidence omitted exact observation: %+v", record)
		}
		return
	}
	t.Fatal("requested-command failure evidence not found")
}

func TestAuthorityAutomaticObservationCauseIsPublishedAndAccepted(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{
		ID:      "authority:auto-cause",
		Name:    "Authority automatic cause",
		Backend: "authority-test",
	}, 16)
	actuator := newFakeActuator()
	var controller *teleop.Controller
	evidence := safety.EvidenceFunc(func(
		ctx context.Context,
		record safety.EvidenceRecord,
	) (safety.EvidenceID, error) {
		id, err := controller.RecordCommandSync(ctx, teleop.Command{
			Name:    string(record.Kind),
			Payload: record.Clone(),
			Causes:  record.Causes,
		})
		return safety.EvidenceID(id.String()), err
	})
	authority, err := safety.NewAuthority(
		safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
			CommandTTL:          time.Second,
		},
		evidence,
		actuator,
		safety.WithCommandTimeout(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller, err = teleop.NewController(
		source,
		teleop.WithProcessor(authority.Processor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if err := authority.Bind(t.Context(), controller); err != nil {
		t.Fatal(err)
	}
	if err := source.Push(t.Context(), teleop.State{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	var observationSequence uint64
	for time.Now().Before(deadline) {
		_, meta := controller.SnapshotWithMeta()
		if meta.Sequence > 0 && !meta.Synthetic {
			observationSequence = meta.Sequence
			break
		}
		time.Sleep(time.Millisecond)
	}
	if observationSequence == 0 {
		t.Fatal("physical observation was not published")
	}
	if err := authority.Arm(t.Context(), "ready"); err != nil {
		t.Fatal(err)
	}
	result, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.4},
	})
	if err != nil {
		t.Fatalf("Apply with automatic published cause: %v", err)
	}
	if !result.Decision.Permit || result.Decision.InputSequence != observationSequence ||
		result.Fallback {
		t.Fatalf("Apply result = %+v", result)
	}
	if err := authority.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityNeverSendsRequestedCommandAfterEvidenceFailure(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, _ := newTestAuthority(t, evidence, actuator)
	if err := authority.Arm(t.Context(), "ready"); err != nil {
		t.Fatal(err)
	}
	baseline := len(actuator.snapshot())
	evidence.setFailure(true)

	result, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.8},
	})
	if !errors.Is(err, safety.ErrEvidence) {
		t.Fatalf("Apply error = %v, want ErrEvidence", err)
	}
	commands := actuator.snapshot()[baseline:]
	if len(commands) != 1 || !commands[0].Fallback ||
		commands[0].Command.Name != "vessel.safe" {
		t.Fatalf("commands after evidence failure = %+v", commands)
	}
	if !result.Fallback {
		t.Fatalf("result = %+v, want fallback", result)
	}
}

func TestFallbackSentEvidenceRetainsIssuanceChronology(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, _ := newTestAuthority(t, evidence, actuator)
	result, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{Name: "must.be.inhibited"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Fallback {
		t.Fatalf("unarmed Apply result = %+v", result)
	}
	for _, record := range evidence.snapshot() {
		if record.Kind != safety.EvidenceSent ||
			record.CommandSequence != result.CommandSequence {
			continue
		}
		if record.Actuator == nil ||
			!record.RecordedAt.Equal(record.Actuator.IssuedAt) {
			t.Fatalf("fallback Sent chronology = %+v", record)
		}
		return
	}
	t.Fatal("fallback Sent evidence not found")
}

type panickingJSONPayload struct{}

func (panickingJSONPayload) MarshalJSON() ([]byte, error) {
	panic("test JSON marshaler panic")
}

func TestAuthorityRecoversCallerAndAdapterPanicsIntoSafeFallback(t *testing.T) {
	t.Run("safe-state payload", func(t *testing.T) {
		_, err := safety.NewAuthority(
			safety.AuthorityConfig{
				EngineeredSafeState: safety.VesselCommand{
					Name:    "vessel.safe",
					Payload: panickingJSONPayload{},
				},
				CommandTTL: time.Second,
			},
			&memoryEvidence{},
			newFakeActuator(),
		)
		if !errors.Is(err, safety.ErrInvalidAuthority) ||
			!errors.Is(err, teleop.ErrCallbackPanic) {
			t.Fatalf("NewAuthority error = %v", err)
		}
	})

	t.Run("intent payload", func(t *testing.T) {
		evidence := &memoryEvidence{}
		actuator := newFakeActuator()
		authority, _ := newTestAuthority(t, evidence, actuator)
		if err := authority.Arm(t.Context(), "ready"); err != nil {
			t.Fatal(err)
		}
		baseline := len(actuator.snapshot())
		result, err := authority.Apply(t.Context(), safety.ApplyRequest{
			Intent: safety.VesselCommand{
				Name:    "propulsion.live",
				Payload: panickingJSONPayload{},
			},
		})
		if !errors.Is(err, safety.ErrInvalidAuthority) ||
			!errors.Is(err, teleop.ErrCallbackPanic) {
			t.Fatalf("Apply error = %v", err)
		}
		commands := actuator.snapshot()[baseline:]
		if len(commands) != 1 || !commands[0].Fallback || !result.Fallback {
			t.Fatalf("commands after payload panic = %+v, result=%+v", commands, result)
		}
		foundFailureDetail := false
		for _, record := range evidence.snapshot() {
			if strings.Contains(record.Detail, "invalid command intent \"propulsion.live\"") &&
				strings.Contains(record.Detail, "payload panic") {
				foundFailureDetail = true
			}
		}
		if !foundFailureDetail {
			t.Fatal("invalid payload failure was not preserved in fallback evidence")
		}
	})

	t.Run("evidence", func(t *testing.T) {
		base := &memoryEvidence{}
		panicNow := false
		evidence := safety.EvidenceFunc(func(
			ctx context.Context,
			record safety.EvidenceRecord,
		) (safety.EvidenceID, error) {
			if panicNow {
				panic("test evidence panic")
			}
			return base.Commit(ctx, record)
		})
		actuator := newFakeActuator()
		authority, err := safety.NewAuthority(safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
			CommandTTL:          50 * time.Millisecond,
		}, evidence, actuator)
		if err != nil {
			t.Fatal(err)
		}
		if err := authority.Bind(t.Context(), newAuthorityTestSource()); err != nil {
			t.Fatal(err)
		}
		if err := authority.Arm(t.Context(), "ready"); err != nil {
			t.Fatal(err)
		}
		baseline := len(actuator.snapshot())
		panicNow = true
		result, err := authority.Apply(t.Context(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 1},
		})
		if !errors.Is(err, safety.ErrEvidence) ||
			!errors.Is(err, teleop.ErrCallbackPanic) {
			t.Fatalf("Apply error = %v", err)
		}
		commands := actuator.snapshot()[baseline:]
		if len(commands) != 1 || !commands[0].Fallback || !result.Fallback {
			t.Fatalf("commands after evidence panic = %+v, result=%+v", commands, result)
		}
	})

	t.Run("clock", func(t *testing.T) {
		panicNow := false
		actuator := newFakeActuator()
		authority, err := safety.NewAuthority(safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
			CommandTTL:          50 * time.Millisecond,
			Now: func() time.Time {
				if panicNow {
					panic("test clock panic")
				}
				return time.Now()
			},
		}, &memoryEvidence{}, actuator)
		if err != nil {
			t.Fatal(err)
		}
		if err := authority.Bind(t.Context(), newAuthorityTestSource()); err != nil {
			t.Fatal(err)
		}
		if err := authority.Arm(t.Context(), "ready"); err != nil {
			t.Fatal(err)
		}
		baseline := len(actuator.snapshot())
		panicNow = true
		result, err := authority.Apply(t.Context(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 1},
		})
		if !errors.Is(err, safety.ErrAuthorityClock) ||
			!errors.Is(err, teleop.ErrCallbackPanic) {
			t.Fatalf("Apply error = %v", err)
		}
		commands := actuator.snapshot()[baseline:]
		if len(commands) != 1 || !commands[0].Fallback || !result.Fallback {
			t.Fatalf("commands after clock panic = %+v, result=%+v", commands, result)
		}
	})
}

func TestAuthorityBoundsAdaptersThatIgnoreContext(t *testing.T) {
	const ttl = 20 * time.Millisecond
	newBound := func(t *testing.T, evidence safety.Evidence, actuator safety.Actuator) *safety.Authority {
		t.Helper()
		authority, err := safety.NewAuthority(safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
			CommandTTL:          ttl,
		}, evidence, actuator)
		if err != nil {
			t.Fatal(err)
		}
		if err := authority.Bind(t.Context(), newAuthorityTestSource()); err != nil {
			t.Fatal(err)
		}
		if err := authority.Arm(t.Context(), "ready"); err != nil {
			t.Fatal(err)
		}
		return authority
	}
	assertBounded := func(t *testing.T, started time.Time, err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("Apply error = %v, want %v", err, want)
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("adapter ignored context for %s", elapsed)
		}
	}

	t.Run("evidence commit", func(t *testing.T) {
		base := &memoryEvidence{}
		block := false
		var blockedCalls atomic.Int32
		release := make(chan struct{})
		evidence := safety.EvidenceFunc(func(
			ctx context.Context,
			record safety.EvidenceRecord,
		) (safety.EvidenceID, error) {
			if block {
				blockedCalls.Add(1)
				<-release // deliberately ignore ctx
			}
			return base.Commit(ctx, record)
		})
		authority := newBound(t, evidence, newFakeActuator())
		block = true
		started := time.Now()
		_, err := authority.Apply(t.Context(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live"},
		})
		assertBounded(t, started, err, safety.ErrEvidence)
		if got := blockedCalls.Load(); got != 1 {
			t.Fatalf("blocked evidence workers = %d, want 1", got)
		}
		close(release)
	})

	t.Run("actuator send", func(t *testing.T) {
		base := newFakeActuator()
		block := false
		var blockedCalls atomic.Int32
		release := make(chan struct{})
		actuator := safety.ActuatorFunc(func(
			ctx context.Context,
			command safety.ActuatorCommand,
		) (safety.ActuatorReceipt, error) {
			if block {
				blockedCalls.Add(1)
				<-release // deliberately ignore ctx
			}
			return base.Send(ctx, command)
		})
		authority := newBound(t, &memoryEvidence{}, actuator)
		block = true
		started := time.Now()
		_, err := authority.Apply(t.Context(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live"},
		})
		assertBounded(t, started, err, safety.ErrActuatorTimeout)
		if got := blockedCalls.Load(); got != 2 {
			t.Fatalf("blocked send workers = %d, want live + fallback", got)
		}
		close(release)
	})

	t.Run("receipt await", func(t *testing.T) {
		base := newFakeActuator()
		block := false
		var blockedCalls atomic.Int32
		release := make(chan struct{})
		actuator := safety.ActuatorFunc(func(
			ctx context.Context,
			command safety.ActuatorCommand,
		) (safety.ActuatorReceipt, error) {
			receipt, err := base.Send(ctx, command)
			if err != nil || !block {
				return receipt, err
			}
			return safety.ReceiptFunc(func(context.Context) (
				safety.ActuatorAcknowledgment,
				error,
			) {
				blockedCalls.Add(1)
				<-release // deliberately ignore ctx
				return safety.ActuatorAcknowledgment{}, errors.New("late ack")
			}), nil
		})
		authority := newBound(t, &memoryEvidence{}, actuator)
		block = true
		started := time.Now()
		_, err := authority.Apply(t.Context(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live"},
		})
		assertBounded(t, started, err, safety.ErrActuatorTimeout)
		if got := blockedCalls.Load(); got != 2 {
			t.Fatalf("blocked receipt workers = %d, want live + fallback", got)
		}
		close(release)
	})

	t.Run("clock", func(t *testing.T) {
		block := false
		var blockedCalls atomic.Int32
		release := make(chan struct{})
		actuator := newFakeActuator()
		authority, err := safety.NewAuthority(safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
			CommandTTL:          ttl,
			Now: func() time.Time {
				if block {
					blockedCalls.Add(1)
					<-release // deliberately has no context to honor
				}
				return time.Now()
			},
		}, &memoryEvidence{}, actuator)
		if err != nil {
			t.Fatal(err)
		}
		if err := authority.Bind(t.Context(), newAuthorityTestSource()); err != nil {
			t.Fatal(err)
		}
		if err := authority.Arm(t.Context(), "ready"); err != nil {
			t.Fatal(err)
		}
		baseline := len(actuator.snapshot())
		block = true
		started := time.Now()
		_, err = authority.Apply(t.Context(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live"},
		})
		assertBounded(t, started, err, safety.ErrAuthorityClock)
		if got := blockedCalls.Load(); got != 1 {
			t.Fatalf("blocked clock workers = %d, want 1", got)
		}
		commands := actuator.snapshot()[baseline:]
		if len(commands) != 1 || !commands[0].Fallback ||
			commands[0].Command.Name != "vessel.safe" {
			t.Fatalf("blocked clock suppressed safe Send: %+v", commands)
		}
		close(release)
	})
}

func TestAuthorityActuatorFailuresSynchronouslyApplyNewerFallback(t *testing.T) {
	tests := []struct {
		name     string
		step     actuatorStep
		wantKind safety.EvidenceKind
		wantErr  error
	}{
		{name: "send", step: actuatorSendFailure, wantKind: safety.EvidenceFailure, wantErr: safety.ErrActuatorUncertain},
		{name: "send panic", step: actuatorSendPanic, wantKind: safety.EvidenceFailure, wantErr: teleop.ErrCallbackPanic},
		{name: "timeout", step: actuatorAwaitFailure, wantKind: safety.EvidenceTimeout, wantErr: safety.ErrActuatorTimeout},
		{name: "receipt panic", step: actuatorAwaitPanic, wantKind: safety.EvidenceFailure, wantErr: teleop.ErrCallbackPanic},
		{name: "rejected", step: actuatorReject, wantKind: safety.EvidenceRejected, wantErr: safety.ErrActuatorRejected},
		{name: "missing applied acknowledgement", step: actuatorAcceptWithoutApplied, wantKind: safety.EvidenceFailure, wantErr: safety.ErrActuatorUncertain},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := &memoryEvidence{}
			actuator := newFakeActuator()
			authority, _ := newTestAuthority(t, evidence, actuator)
			if err := authority.Arm(t.Context(), "ready"); err != nil {
				t.Fatal(err)
			}
			baseline := len(actuator.snapshot())
			actuator.enqueue(test.step, actuatorAccept)

			result, err := authority.Apply(t.Context(), safety.ApplyRequest{
				Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.6},
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Apply error = %v, want %v", err, test.wantErr)
			}
			commands := actuator.snapshot()[baseline:]
			if len(commands) != 2 {
				t.Fatalf("command attempts = %d, want requested + fallback", len(commands))
			}
			if commands[0].Fallback || !commands[1].Fallback ||
				commands[1].Sequence <= commands[0].Sequence {
				t.Fatalf("command ordering = %+v", commands)
			}
			if result.FallbackSequence != commands[1].Sequence || !result.FallbackAcknowledged {
				t.Fatalf("fallback result = %+v", result)
			}
			found := false
			for _, record := range evidence.snapshot() {
				found = found || record.Kind == test.wantKind
			}
			if !found {
				t.Fatalf("evidence does not contain %q", test.wantKind)
			}
		})
	}
}

func TestAuthorityRechecksInterlocksAfterIntentBarrierBeforeSend(t *testing.T) {
	base := &memoryEvidence{}
	intentEntered := make(chan struct{})
	releaseIntent := make(chan struct{})
	blockIntent := false
	var once sync.Once
	evidence := safety.EvidenceFunc(func(
		ctx context.Context,
		record safety.EvidenceRecord,
	) (safety.EvidenceID, error) {
		if blockIntent && record.Kind == safety.EvidenceIntent && record.Applied != nil &&
			record.Applied.Name == "propulsion.live" {
			once.Do(func() { close(intentEntered) })
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-releaseIntent:
			}
		}
		return base.Commit(ctx, record)
	})
	actuator := newFakeActuator()
	authority, err := safety.NewAuthority(
		safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
			CommandTTL:          time.Second,
		},
		evidence,
		actuator,
		safety.WithCommandTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Bind(t.Context(), newAuthorityTestSource()); err != nil {
		t.Fatal(err)
	}
	if err := authority.Arm(t.Context(), "ready"); err != nil {
		t.Fatal(err)
	}
	baseline := len(actuator.snapshot())
	blockIntent = true

	applyDone := make(chan error, 1)
	go func() {
		_, applyErr := authority.Apply(context.Background(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.8},
		})
		applyDone <- applyErr
	}()
	select {
	case <-intentEntered:
	case <-time.After(time.Second):
		t.Fatal("requested intent did not reach durability barrier")
	}

	// Model a canonical backend-loss event processed while the requested intent
	// waits behind that earlier event's durable sink barrier.
	authority.Processor().Process(teleop.GapEvent{Source: "input", Reason: "backend loss"})
	close(releaseIntent)
	select {
	case err := <-applyDone:
		if !errors.Is(err, safety.ErrAuthorityRevoked) {
			t.Fatalf("Apply error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Apply did not finish")
	}

	commands := actuator.snapshot()[baseline:]
	if len(commands) != 1 || !commands[0].Fallback ||
		commands[0].Command.Name != "vessel.safe" {
		t.Fatalf("stale permit reached actuator: %+v", commands)
	}
}

func TestAuthorityRejectsChangedInputEpochAfterIntentBarrier(t *testing.T) {
	base := &memoryEvidence{}
	intentEntered := make(chan struct{})
	releaseIntent := make(chan struct{})
	blockIntent := false
	var once sync.Once
	evidence := safety.EvidenceFunc(func(
		ctx context.Context,
		record safety.EvidenceRecord,
	) (safety.EvidenceID, error) {
		if blockIntent && record.Kind == safety.EvidenceIntent && record.Applied != nil &&
			record.Applied.Name == "propulsion.live" {
			once.Do(func() { close(intentEntered) })
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-releaseIntent:
			}
		}
		return base.Commit(ctx, record)
	})
	actuator := newFakeActuator()
	authority, err := safety.NewAuthority(
		safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
			CommandTTL:          time.Second,
		},
		evidence,
		actuator,
		safety.WithCommandTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	source := newAuthorityTestSource()
	if err := authority.Bind(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err := authority.Arm(t.Context(), "ready"); err != nil {
		t.Fatal(err)
	}
	baseline := len(actuator.snapshot())
	blockIntent = true

	applyDone := make(chan error, 1)
	go func() {
		_, applyErr := authority.Apply(context.Background(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.8},
		})
		applyDone <- applyErr
	}()
	select {
	case <-intentEntered:
	case <-time.After(time.Second):
		t.Fatal("requested intent did not reach durability barrier")
	}

	// The newer snapshot is still permitted, but it no longer corresponds to
	// the command derived from the earlier decision.
	source.setState(teleop.State{LeftStick: teleop.Stick{X: -0.75}})
	close(releaseIntent)
	select {
	case err := <-applyDone:
		if !errors.Is(err, safety.ErrAuthorityRevoked) {
			t.Fatalf("Apply error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Apply did not finish")
	}

	commands := actuator.snapshot()[baseline:]
	if len(commands) != 1 || !commands[0].Fallback ||
		commands[0].Command.Name != "vessel.safe" {
		t.Fatalf("stale input epoch reached actuator: %+v", commands)
	}
}

func TestProcessorFaultCancelsInflightLiveApplyAndSendsNewerSafeState(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, _ := newTestAuthority(t, evidence, actuator)
	if err := authority.Arm(t.Context(), "ready"); err != nil {
		t.Fatal(err)
	}
	baseline := len(actuator.snapshot())
	actuator.enqueue(actuatorAwaitCancellation, actuatorAccept)

	type applyOutcome struct {
		result safety.ApplyResult
		err    error
	}
	applyDone := make(chan applyOutcome, 1)
	go func() {
		result, err := authority.Apply(context.Background(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.8},
		})
		applyDone <- applyOutcome{result: result, err: err}
	}()
	deadline := time.Now().Add(time.Second)
	for len(actuator.snapshot()) < baseline+1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(actuator.snapshot()) < baseline+1 {
		t.Fatal("live command did not reach actuator receipt wait")
	}

	// This fault lands after the final in-process pre-Send check. Processor must
	// cancel only the live operation so Apply can use its independent safe path.
	authority.Processor().Process(teleop.GapEvent{
		Source: "input",
		Reason: "backend loss after live send",
	})
	select {
	case outcome := <-applyDone:
		if !errors.Is(outcome.err, safety.ErrActuatorUncertain) {
			t.Fatalf("Apply error = %v", outcome.err)
		}
		commands := actuator.snapshot()[baseline:]
		if len(commands) != 2 || commands[0].Fallback || !commands[1].Fallback ||
			commands[1].Sequence <= commands[0].Sequence {
			t.Fatalf("processor fault commands = %+v", commands)
		}
		if outcome.result.FallbackSequence != commands[1].Sequence ||
			!outcome.result.FallbackAcknowledged {
			t.Fatalf("processor fault result = %+v", outcome.result)
		}
	case <-time.After(time.Second):
		t.Fatal("processor fault did not cancel in-flight live Apply")
	}
}

func TestNewerPermittedInputCancelsInflightStaleLiveApply(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, source := newTestAuthority(t, evidence, actuator)
	if err := authority.Arm(t.Context(), "ready"); err != nil {
		t.Fatal(err)
	}
	baseline := len(actuator.snapshot())
	actuator.enqueue(actuatorAwaitCancellation, actuatorAccept)

	type applyOutcome struct {
		result safety.ApplyResult
		err    error
	}
	applyDone := make(chan applyOutcome, 1)
	go func() {
		result, err := authority.Apply(context.Background(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.8},
		})
		applyDone <- applyOutcome{result: result, err: err}
	}()
	deadline := time.Now().Add(time.Second)
	for len(actuator.snapshot()) < baseline+1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(actuator.snapshot()) < baseline+1 {
		t.Fatal("live command did not reach actuator receipt wait")
	}

	newState := teleop.State{LeftStick: teleop.Stick{X: 0.6}}
	source.setState(newState)
	authority.Processor().Process(teleop.ObservationEvent{Current: newState})
	select {
	case outcome := <-applyDone:
		if !errors.Is(outcome.err, safety.ErrAuthorityRevoked) {
			t.Fatalf("Apply error = %v", outcome.err)
		}
		commands := actuator.snapshot()[baseline:]
		if len(commands) != 2 || commands[0].Fallback || !commands[1].Fallback ||
			commands[1].Sequence <= commands[0].Sequence {
			t.Fatalf("newer permitted input commands = %+v", commands)
		}
		if outcome.result.FallbackSequence != commands[1].Sequence ||
			!outcome.result.FallbackAcknowledged {
			t.Fatalf("newer permitted input result = %+v", outcome.result)
		}
	case <-time.After(time.Second):
		t.Fatal("newer permitted input did not cancel stale live Apply")
	}
}

func TestEmergencyStopCancelsInflightAndSerializesSafeState(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, _ := newTestAuthority(t, evidence, actuator)
	if err := authority.Arm(t.Context(), "ready"); err != nil {
		t.Fatal(err)
	}
	baseline := len(actuator.snapshot())
	actuator.enqueue(actuatorAwaitCancellation, actuatorAccept, actuatorAccept)

	applyDone := make(chan error, 1)
	go func() {
		_, err := authority.Apply(context.Background(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 1},
		})
		applyDone <- err
	}()
	select {
	case <-actuator.notify:
		// The bootstrap notification may still be buffered.
	default:
	}
	deadline := time.Now().Add(time.Second)
	for len(actuator.snapshot()) < baseline+1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- authority.EmergencyStop(context.Background(), "person overboard")
	}()
	select {
	case err := <-applyDone:
		if !errors.Is(err, safety.ErrActuatorUncertain) {
			t.Fatalf("Apply error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight Apply was not canceled")
	}
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("EmergencyStop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("EmergencyStop did not serialize a safe command")
	}

	commands := actuator.snapshot()[baseline:]
	if len(commands) < 3 || !commands[len(commands)-1].Fallback {
		t.Fatalf("commands after emergency stop = %+v", commands)
	}
	for index := 1; index < len(commands); index++ {
		if commands[index].Sequence <= commands[index-1].Sequence {
			t.Fatalf("non-monotonic command sequence = %+v", commands)
		}
	}
}

func TestSafeFallbackGetsFreshActuationBudgetAfterBlockedEvidence(t *testing.T) {
	tests := []struct {
		name  string
		block func(safety.EvidenceRecord) bool
		run   func(*safety.Authority, *fakeActuator) error
		want  error
	}{
		{
			name: "disarm",
			block: func(record safety.EvidenceRecord) bool {
				return record.Kind == safety.EvidenceLifecycleAttempt && record.Lifecycle != nil &&
					record.Lifecycle.Action == safety.LifecycleDisarm
			},
			run: func(authority *safety.Authority, _ *fakeActuator) error {
				return authority.Disarm(context.Background(), "blocked evidence")
			},
			want: safety.ErrEvidence,
		},
		{
			name: "close",
			block: func(record safety.EvidenceRecord) bool {
				return record.Kind == safety.EvidenceLifecycleAttempt && record.Lifecycle != nil &&
					record.Lifecycle.Action == safety.LifecycleClose
			},
			run: func(authority *safety.Authority, _ *fakeActuator) error {
				return authority.Close(context.Background())
			},
			want: safety.ErrEvidence,
		},
		{
			name: "uncertain requested command",
			block: func(record safety.EvidenceRecord) bool {
				return record.Kind == safety.EvidenceFailure && record.Applied != nil &&
					record.Applied.Name == "propulsion.live"
			},
			run: func(authority *safety.Authority, actuator *fakeActuator) error {
				actuator.enqueue(actuatorSendFailure, actuatorAccept)
				_, err := authority.Apply(context.Background(), safety.ApplyRequest{
					Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.9},
				})
				return err
			},
			want: safety.ErrActuatorUncertain,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := &blockingEvidence{
				block:   test.block,
				entered: make(chan struct{}),
				release: make(chan struct{}),
			}
			actuator := newFakeActuator()
			authority, err := safety.NewAuthority(
				safety.AuthorityConfig{
					EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
					CommandTTL:          25 * time.Millisecond,
				},
				evidence,
				actuator,
				safety.WithCommandTimeout(time.Second),
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := authority.Bind(t.Context(), newAuthorityTestSource()); err != nil {
				t.Fatal(err)
			}
			if err := authority.Arm(t.Context(), "ready"); err != nil {
				t.Fatal(err)
			}
			baseline := actuator.snapshot()
			operationDone := make(chan error, 1)
			go func() { operationDone <- test.run(authority, actuator) }()
			select {
			case <-evidence.entered:
			case <-time.After(time.Second):
				t.Fatal("operation did not enter blocking evidence callback")
			}
			select {
			case operationErr := <-operationDone:
				if !errors.Is(operationErr, test.want) {
					t.Fatalf("operation error = %v, want %v", operationErr, test.want)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("blocked evidence consumed the safe actuation budget")
			}

			commands := actuator.snapshot()
			if len(commands) <= len(baseline) {
				t.Fatalf("no command after blocked evidence: %+v", commands)
			}
			last := commands[len(commands)-1]
			if !last.Fallback || last.Command.Name != "vessel.safe" ||
				last.Sequence <= baseline[len(baseline)-1].Sequence {
				t.Fatalf("last command is not a newer safe fallback: %+v", commands)
			}
			close(evidence.release)
		})
	}
}

func TestLaterSafetyLifecycleDominatesEarlierPermissiveOperation(t *testing.T) {
	t.Run("emergency stop after blocked reset", func(t *testing.T) {
		evidence := &blockingEvidence{
			entered: make(chan struct{}),
			release: make(chan struct{}),
			block: func(record safety.EvidenceRecord) bool {
				return record.Kind == safety.EvidenceLifecycleAttempt && record.Lifecycle != nil &&
					record.Lifecycle.Action == safety.LifecycleReset
			},
		}
		clock := newOneShotClockBarrier()
		actuator := newFakeActuator()
		authority, err := safety.NewAuthority(
			safety.AuthorityConfig{
				EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
				CommandTTL:          time.Second,
				Now:                 clock.Now,
			},
			evidence,
			actuator,
			safety.WithCommandTimeout(10*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := authority.Bind(t.Context(), newAuthorityTestSource()); err != nil {
			t.Fatal(err)
		}
		if err := authority.EmergencyStop(t.Context(), "initial stop"); err != nil {
			t.Fatal(err)
		}

		resetDone := make(chan error, 1)
		go func() { resetDone <- authority.Reset(context.Background(), "earlier reset") }()
		select {
		case <-evidence.entered:
		case <-time.After(time.Second):
			t.Fatal("Reset did not hold the lifecycle serializer")
		}
		clock.enabled.Store(true)
		stopDone := make(chan error, 1)
		go func() {
			stopDone <- authority.EmergencyStop(context.Background(), "later stop")
		}()
		select {
		case <-clock.entered:
		case <-time.After(time.Second):
			t.Fatal("later EmergencyStop did not tighten before waiting")
		}
		close(clock.release)
		<-clock.exited
		close(evidence.release)
		if err := <-resetDone; !errors.Is(err, safety.ErrAuthorityRevoked) {
			t.Fatalf("superseded Reset error = %v", err)
		}
		if err := <-stopDone; err != nil {
			t.Fatalf("EmergencyStop: %v", err)
		}

		result, err := authority.Apply(t.Context(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Decision.Permit || !result.Fallback ||
			!slices.Contains(result.Decision.Reasons, safety.ReasonEmergencyStop) {
			t.Fatalf("earlier Reset overwrote later EmergencyStop: %+v", result)
		}
	})

	t.Run("disarm after blocked arm", func(t *testing.T) {
		evidence := &blockingEvidence{
			entered: make(chan struct{}),
			release: make(chan struct{}),
			block: func(record safety.EvidenceRecord) bool {
				return record.Kind == safety.EvidenceLifecycleAttempt && record.Lifecycle != nil &&
					record.Lifecycle.Action == safety.LifecycleArm
			},
		}
		clock := newOneShotClockBarrier()
		actuator := newFakeActuator()
		authority, err := safety.NewAuthority(
			safety.AuthorityConfig{
				EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
				CommandTTL:          time.Second,
				Now:                 clock.Now,
			},
			evidence,
			actuator,
			safety.WithCommandTimeout(10*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := authority.Bind(t.Context(), newAuthorityTestSource()); err != nil {
			t.Fatal(err)
		}

		armDone := make(chan error, 1)
		go func() { armDone <- authority.Arm(context.Background(), "earlier arm") }()
		select {
		case <-evidence.entered:
		case <-time.After(time.Second):
			t.Fatal("Arm did not hold the lifecycle serializer")
		}
		clock.enabled.Store(true)
		disarmDone := make(chan error, 1)
		go func() { disarmDone <- authority.Disarm(context.Background(), "later disarm") }()
		select {
		case <-clock.entered:
		case <-time.After(time.Second):
			t.Fatal("later Disarm did not tighten before waiting")
		}
		close(clock.release)
		<-clock.exited
		close(evidence.release)
		if err := <-armDone; !errors.Is(err, safety.ErrAuthorityRevoked) {
			t.Fatalf("superseded Arm error = %v", err)
		}
		if err := <-disarmDone; err != nil {
			t.Fatalf("Disarm: %v", err)
		}

		result, err := authority.Apply(t.Context(), safety.ApplyRequest{
			Intent: safety.VesselCommand{Name: "propulsion.live"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Decision.Permit || !result.Fallback {
			t.Fatalf("earlier Arm overwrote later Disarm: %+v", result)
		}
	})
}

func TestCloseAttemptsSafeStateWithoutEvidenceAndRejectsLaterApply(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, _ := newTestAuthority(t, evidence, actuator)
	baseline := len(actuator.snapshot())
	evidence.setFailure(true)

	if err := authority.Close(t.Context()); !errors.Is(err, safety.ErrEvidence) {
		t.Fatalf("Close error = %v, want evidence failure after safe attempt", err)
	}
	commands := actuator.snapshot()[baseline:]
	if len(commands) != 1 || !commands[0].Fallback || commands[0].Command.Name != "vessel.safe" {
		t.Fatalf("close commands = %+v", commands)
	}
	before := len(actuator.snapshot())
	_, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{Name: "must.not.send", Payload: true},
	})
	if !errors.Is(err, safety.ErrAuthorityClosed) {
		t.Fatalf("Apply after Close error = %v", err)
	}
	if after := len(actuator.snapshot()); after != before {
		t.Fatalf("Apply after Close sent %d commands", after-before)
	}
	_, err = authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{Payload: make(chan struct{})},
	})
	if !errors.Is(err, safety.ErrAuthorityClosed) {
		t.Fatalf("malformed Apply after Close error = %v", err)
	}
	if after := len(actuator.snapshot()); after != before {
		t.Fatalf("malformed Apply after Close sent %d commands", after-before)
	}
}

func TestBindFailureAfterGuardAttachmentIsTerminal(t *testing.T) {
	t.Run("lifecycle outcome evidence", func(t *testing.T) {
		var commits int
		evidence := safety.EvidenceFunc(func(
			_ context.Context,
			_ safety.EvidenceRecord,
		) (safety.EvidenceID, error) {
			commits++
			if commits == 2 {
				return "", errors.New("bind outcome storage failed")
			}
			return safety.EvidenceID(fmt.Sprintf("bind-%d", commits)), nil
		})
		actuator := newFakeActuator()
		authority, err := safety.NewAuthority(safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
			CommandTTL:          time.Second,
		}, evidence, actuator)
		if err != nil {
			t.Fatal(err)
		}
		if err := authority.Bind(t.Context(), newAuthorityTestSource()); !errors.Is(err, safety.ErrEvidence) {
			t.Fatalf("Bind error = %v", err)
		}
		assertTerminalAuthority(t, authority, actuator)
	})

	t.Run("bootstrap safe state", func(t *testing.T) {
		actuator := newFakeActuator()
		actuator.enqueue(actuatorSendFailure)
		authority, err := safety.NewAuthority(safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
			CommandTTL:          time.Second,
		}, &memoryEvidence{}, actuator)
		if err != nil {
			t.Fatal(err)
		}
		if err := authority.Bind(t.Context(), newAuthorityTestSource()); !errors.Is(err, safety.ErrActuatorUncertain) {
			t.Fatalf("Bind error = %v", err)
		}
		assertTerminalAuthority(t, authority, actuator)
	})
}

func assertTerminalAuthority(
	t *testing.T,
	authority *safety.Authority,
	actuator *fakeActuator,
) {
	t.Helper()
	before := len(actuator.snapshot())
	if err := authority.Arm(t.Context(), "must stay closed"); !errors.Is(err, safety.ErrAuthorityClosed) {
		t.Fatalf("Arm after failed Bind = %v", err)
	}
	_, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{Name: "must.not.send"},
	})
	if !errors.Is(err, safety.ErrAuthorityClosed) {
		t.Fatalf("Apply after failed Bind = %v", err)
	}
	if after := len(actuator.snapshot()); after != before {
		t.Fatalf("failed Bind remained reusable: sent %d later commands", after-before)
	}
}

func TestFailedRepeatedArmRevokesStandingAuthorityAndRetriesSafeState(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, err := safety.NewAuthority(
		safety.AuthorityConfig{
			EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
			CommandTTL:          time.Second,
		},
		evidence,
		actuator,
		safety.WithCommandTimeout(10*time.Second),
		safety.WithLoopWatchdog(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	source := newAuthorityTestSource()
	if err := authority.Bind(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err := authority.Arm(t.Context(), "initial arm"); err != nil {
		t.Fatal(err)
	}
	baseline := len(actuator.snapshot())
	actuator.enqueue(actuatorSendFailure, actuatorAccept)

	if err := authority.Arm(t.Context(), "repeat arm"); !errors.Is(err, safety.ErrActuatorUncertain) {
		t.Fatalf("repeated Arm error = %v", err)
	}
	commands := actuator.snapshot()[baseline:]
	if len(commands) != 2 || !commands[0].Fallback || !commands[1].Fallback ||
		commands[1].Sequence <= commands[0].Sequence {
		t.Fatalf("preflight failure did not retry newer safe state: %+v", commands)
	}

	result, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.Permit || !result.Fallback {
		t.Fatalf("failed re-Arm preserved authority: %+v", result)
	}
}

func TestRefusedRepeatedArmForNonNeutralControlsRevokesAuthority(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, source := newTestAuthority(t, evidence, actuator)
	if err := authority.Arm(t.Context(), "initial arm"); err != nil {
		t.Fatal(err)
	}
	source.setState(teleop.State{LeftStick: teleop.Stick{X: 0.8}})
	baseline := len(actuator.snapshot())
	if err := authority.Arm(t.Context(), "unsafe repeat arm"); !errors.Is(err, safety.ErrUnsafeToArm) {
		t.Fatalf("repeated Arm error = %v", err)
	}
	commands := actuator.snapshot()[baseline:]
	if len(commands) != 1 || !commands[0].Fallback {
		t.Fatalf("refused re-Arm safe commands = %+v", commands)
	}

	result, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.Permit || !result.Fallback {
		t.Fatalf("refused re-Arm preserved authority: %+v", result)
	}
}

func TestAuthorityRecordsRefusedAndRepeatedLifecycleActions(t *testing.T) {
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, _ := newTestAuthority(t, evidence, actuator)
	if err := authority.EmergencyStop(t.Context(), "first stop"); err != nil {
		t.Fatal(err)
	}
	if err := authority.EmergencyStop(t.Context(), "repeat stop"); err != nil {
		t.Fatal(err)
	}
	if err := authority.Arm(t.Context(), "refused arm"); err == nil {
		t.Fatal("Arm during emergency stop succeeded")
	}

	records := evidence.snapshot()
	var (
		stopAttempts int
		refusedArm   bool
	)
	for _, record := range records {
		if record.Kind == safety.EvidenceLifecycleAttempt && record.Lifecycle != nil &&
			record.Lifecycle.Action == safety.LifecycleEmergencyStop {
			stopAttempts++
		}
		if record.Kind == safety.EvidenceLifecycleOutcome && record.Lifecycle != nil &&
			record.Lifecycle.Action == safety.LifecycleArm && !record.Lifecycle.Accepted &&
			record.Error != "" {
			refusedArm = true
		}
	}
	if stopAttempts != 2 || !refusedArm {
		t.Fatalf("lifecycle evidence: repeated stops=%d refused arm=%t", stopAttempts, refusedArm)
	}
}

func TestAuthorityWithMaritimeGuardBootstrapsWatchdogAfterSafeAck(t *testing.T) {
	const deadMan = teleop.ButtonBumperRight
	config := safety.DefaultMaritimeConfig(deadMan)
	config.CommandTimeout = time.Second
	config.TransportTimeout = time.Second
	config.LoopWatchdog = time.Second
	guard, err := safety.NewMaritime(config)
	if err != nil {
		t.Fatal(err)
	}
	evidence := &memoryEvidence{}
	actuator := newFakeActuator()
	authority, err := safety.NewAuthorityWithGuard(safety.AuthorityConfig{
		EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe", Payload: 0},
		CommandTTL:          time.Second,
	}, guard, evidence, actuator)
	if err != nil {
		t.Fatal(err)
	}
	source := newAuthorityTestSource()
	source.caps = teleop.Capabilities{Controls: []teleop.ControlDescriptor{{
		ID: deadMan, Kind: teleop.ControlButton,
	}}}
	if err := authority.Bind(t.Context(), source); err != nil {
		t.Fatalf("strict Bind bootstrap: %v", err)
	}
	// Model a slow startup witness or operator handoff. Transport proof remains
	// fresh, but the watchdog seeded by Bind has expired before Arm begins.
	source.mu.Lock()
	source.now = 2 * time.Second
	source.meta.ReceivedMonotonic = source.now
	source.meta.LastTransportCheckMonotonic = source.now
	source.meta.TransportCheckSequence++
	source.mu.Unlock()
	if err := authority.Arm(t.Context(), "neutral and ready"); err != nil {
		t.Fatalf("strict Arm after stale Bind heartbeat: %v", err)
	}

	held := teleop.State{}
	held.SetButton(deadMan, true)
	source.mu.Lock()
	source.now += time.Nanosecond
	source.mu.Unlock()
	source.setState(held)
	authority.Processor().Process(teleop.ButtonEvent{
		Meta: teleop.Header{
			ID:                teleop.EventID{Session: source.Session(), Stream: "input", Sequence: 2},
			Monotonic:         source.Monotonic(),
			ReceivedMonotonic: source.Monotonic(),
		},
		Button:  deadMan,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})
	result, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.VesselCommand{Name: "propulsion.live", Payload: 0.2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Decision.Permit || result.Fallback {
		t.Fatalf("strict result = %+v", result)
	}
}

func TestAuthorityFunctionAdapters(t *testing.T) {
	var (
		records  []safety.EvidenceRecord
		commands []safety.ActuatorCommand
	)
	evidence := safety.EvidenceFunc(func(
		_ context.Context,
		record safety.EvidenceRecord,
	) (safety.EvidenceID, error) {
		records = append(records, record.Clone())
		return safety.EvidenceID(fmt.Sprintf("fn-%d", len(records))), nil
	})
	actuator := safety.ActuatorFunc(func(
		_ context.Context,
		command safety.ActuatorCommand,
	) (safety.ActuatorReceipt, error) {
		commands = append(commands, command.Clone())
		return safety.ReceiptFunc(func(context.Context) (safety.ActuatorAcknowledgment, error) {
			return safety.ActuatorAcknowledgment{
				ControllerSession: command.ControllerSession,
				Sequence:          command.Sequence,
				Accepted:          true,
			}, nil
		}), nil
	})
	authority, err := safety.NewAuthority(safety.AuthorityConfig{
		EngineeredSafeState: safety.VesselCommand{Name: "vessel.safe"},
		CommandTTL:          time.Second,
	}, evidence, actuator)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Bind(t.Context(), newAuthorityTestSource()); err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 || len(commands) != 1 || !commands[0].Fallback {
		t.Fatalf("function adapters: records=%d commands=%+v", len(records), commands)
	}
}
