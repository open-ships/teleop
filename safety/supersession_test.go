package safety_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/safety"
)

func TestOrdinaryInputSupersessionBeforeSendPreservesStandingArm(t *testing.T) {
	for _, stage := range []safety.EvidenceKind{safety.EvidenceDecision, safety.EvidenceIntent} {
		for _, change := range []string{"unchanged observation", "steering change", "dead-man release"} {
			t.Run(string(stage)+"/"+change, func(t *testing.T) {
				var enabled atomic.Bool
				entered, release := make(chan struct{}), make(chan struct{})
				base := &memoryEvidence{}
				evidence := safety.EvidenceFunc(func(ctx context.Context, record safety.EvidenceRecord) (safety.EvidenceID, error) {
					if record.Kind == stage && enabled.CompareAndSwap(true, false) {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return "", ctx.Err()
						}
					}
					return base.Commit(ctx, record)
				})
				actuator := newFakeActuator()
				source := newAuthorityTestSource()
				source.caps = teleop.Capabilities{AuditGrade: teleop.AuditExactBackendStream, Controls: []teleop.ControlDescriptor{{ID: teleop.ButtonBumperLeft, Kind: teleop.ControlButton}}}
				profile := safety.DefaultMaritimeConfig(teleop.ButtonBumperLeft)
				profile.CommandTimeout, profile.TransportTimeout, profile.LoopWatchdog = time.Second, time.Second, time.Second
				guard, err := safety.NewMaritime(profile)
				if err != nil {
					t.Fatal(err)
				}
				authority, err := safety.NewAuthorityWithGuard(safety.AuthorityConfig{
					EngineeredSafeState: safety.VesselCommand{Name: "safe"}, CommandTTL: time.Second,
					RequireAppliedAcknowledgment: true,
				}, guard, evidence, actuator)
				if err != nil {
					t.Fatal(err)
				}
				if err := authority.Bind(t.Context(), source); err != nil {
					t.Fatal(err)
				}
				if err := authority.Arm(t.Context(), "ready"); err != nil {
					t.Fatal(err)
				}
				observe := func(held bool, steering float32) {
					source.mu.Lock()
					source.now += time.Millisecond
					source.mu.Unlock()
					state := teleop.State{LeftStick: teleop.Stick{X: steering}}
					state.SetButton(teleop.ButtonBumperLeft, held)
					source.setState(state)
					_, meta := source.SnapshotWithMeta()
					header := teleop.Header{ID: teleop.EventID{Session: source.Session(), Stream: "input", Sequence: meta.Sequence}, ReceivedMonotonic: meta.ReceivedMonotonic}
					authority.Processor().Process(teleop.ObservationEvent{Meta: header, Current: state})
					authority.Processor().Process(teleop.ButtonEvent{Meta: header, Button: teleop.ButtonBumperLeft, Pressed: held})
				}
				observe(true, 0)
				baseline := len(actuator.snapshot())
				enabled.Store(true)
				type outcome struct {
					result safety.ApplyResult
					err    error
				}
				done := make(chan outcome, 1)
				request := safety.ApplyRequest{Intent: safety.VesselCommand{Name: "live", Payload: 0.4}}
				go func() { result, err := authority.Apply(t.Context(), request); done <- outcome{result, err} }()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("evidence barrier not entered")
				}
				switch change {
				case "unchanged observation":
					observe(true, 0)
				case "steering change":
					observe(true, 0.5)
				case "dead-man release":
					observe(false, 0)
				}
				close(release)
				var first outcome
				select {
				case first = <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("Apply did not finish")
				}
				if !errors.Is(first.err, safety.ErrIntentSuperseded) || errors.Is(first.err, safety.ErrEvidence) || !first.result.FallbackAcknowledged {
					t.Fatalf("supersession result=%+v err=%v", first.result, first.err)
				}
				finishedUnsent := false
				for _, record := range base.snapshot() {
					if record.Kind == safety.EvidenceFailure && record.ParentID == first.result.IntentEvidenceID && record.Detail == "intent not transmitted" {
						finishedUnsent = true
					}
				}
				if !finishedUnsent || first.result.OutcomeEvidenceID == "" {
					t.Fatal("superseded intent has no terminal evidence")
				}
				for _, command := range actuator.snapshot()[baseline:] {
					if !command.Fallback {
						t.Fatal("superseded intent reached the actuator")
					}
				}
				if change == "dead-man release" {
					observe(true, 0)
				}
				next, err := authority.Apply(t.Context(), request)
				if err != nil || !next.Decision.Permit || next.Fallback {
					t.Fatalf("ordinary update lost standing arm: %+v err=%v", next, err)
				}
			})
		}
	}
}

func TestUncertainTransmittedCommandStillRequiresRearm(t *testing.T) {
	evidence, actuator := &memoryEvidence{}, newFakeActuator()
	authority, _ := newTestAuthority(t, evidence, actuator)
	if err := authority.Arm(t.Context(), "ready"); err != nil {
		t.Fatal(err)
	}
	actuator.enqueue(actuatorAwaitFailure, actuatorAccept)
	request := safety.ApplyRequest{Intent: safety.VesselCommand{Name: "live"}}
	if _, err := authority.Apply(t.Context(), request); !errors.Is(err, safety.ErrActuatorTimeout) {
		t.Fatalf("uncertain command err=%v", err)
	}
	next, err := authority.Apply(t.Context(), request)
	if err != nil || next.Decision.Permit || !next.Decision.Has(safety.ReasonNotArmed) {
		t.Fatalf("uncertainty restored standing arm: %+v err=%v", next, err)
	}
}
