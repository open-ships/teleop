package assured_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	actions "github.com/open-ships/teleop/action"
	"github.com/open-ships/teleop/assured"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/safety"
)

func TestSessionSafetyCallsOutliveCallerCancellation(t *testing.T) {
	for _, action := range []safety.LifecycleAction{safety.LifecycleEmergencyStop, safety.LifecycleDisarm} {
		for _, expired := range []bool{false, true} {
			for _, inflight := range []bool{false, true} {
				name := string(action)
				if expired {
					name += "/expired"
				} else {
					name += "/canceled"
				}
				if inflight {
					name += "/inflight"
				}
				t.Run(name, func(t *testing.T) { testSessionSafetyContext(t, action, expired, inflight) })
			}
		}
	}
}

func testSessionSafetyContext(t *testing.T, action safety.LifecycleAction, expired, inflight bool) {
	t.Helper()
	store, anchor, actuator := &syncStore{}, &witnessAnchor{}, &acceptingActuator{}
	var block atomic.Bool
	awaiting := make(chan struct{})
	adapter := safety.ActuatorFunc(func(ctx context.Context, command safety.ActuatorCommand) (safety.ActuatorReceipt, error) {
		receipt, err := actuator.Send(ctx, command)
		if !command.Fallback && block.CompareAndSwap(true, false) {
			return safety.ReceiptFunc(func(ctx context.Context) (safety.ActuatorAcknowledgment, error) {
				close(awaiting)
				<-ctx.Done()
				return safety.ActuatorAcknowledgment{}, ctx.Err()
			}), nil
		}
		return receipt, err
	})
	config, public := validConfig(t, store, anchor, adapter)
	config.Authority.CommandTTL = 400 * time.Millisecond
	config.Maritime.CommandTimeout = 2 * time.Second
	config.Maritime.TransportTimeout = 2 * time.Second
	config.Maritime.LoopWatchdog = 2 * time.Second
	config.Maritime.DeadManReactuation = 5 * time.Second
	config.Processors = []teleop.Processor{actionMapperForStopTest()}
	source := exactSource(false)
	if err := source.Push(t.Context(), teleop.State{}); err != nil {
		t.Fatal(err)
	}
	session, err := assured.OpenSource(t.Context(), source, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := session.Arm(t.Context(), "test ready"); err != nil {
		t.Fatal(err)
	}
	sub, err := session.Subscribe(teleop.SubscriptionOptions{Buffer: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	state := teleop.State{}
	state.SetButton(teleop.ButtonBumperRight, true)
	if err := source.Push(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		event, err := sub.Next(wait)
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := event.(actions.Event); ok && value.Action == "engaged" {
			break
		}
	}
	request := safety.ApplyRequest{Intent: safety.Command{Name: "drive", Payload: map[string]float64{"power": 0.5}}}
	before, err := session.Apply(t.Context(), request)
	if err != nil || before.Fallback {
		t.Fatalf("live setup: %+v, %v", before, err)
	}
	var applyDone chan error
	if inflight {
		block.Store(true)
		applyDone = make(chan error, 1)
		go func() { _, err := session.Apply(t.Context(), request); applyDone <- err }()
		select {
		case <-awaiting:
		case <-wait.Done():
			t.Fatal("live Await did not start")
		}
	}
	count := len(actuator.snapshot())
	stopCtx, stop := context.WithCancel(t.Context())
	stop()
	if expired {
		stopCtx, stop = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer stop()
	}
	if action == safety.LifecycleEmergencyStop {
		err = session.EmergencyStop(stopCtx, "requested stop")
	} else {
		err = session.Disarm(stopCtx, "requested stop")
	}
	if err != nil {
		t.Fatalf("stop inherited caller cancellation: %v", err)
	}
	if inflight {
		select {
		case err := <-applyDone:
			if err == nil {
				t.Fatal("in-flight command survived stop")
			}
		case <-wait.Done():
			t.Fatal("stop did not cancel in-flight Apply")
		}
	}
	commands := actuator.snapshot()
	if len(commands) <= count {
		t.Fatal("stop did not send a safe command")
	}
	for _, command := range commands[count:] {
		if !command.Fallback || command.Sequence <= before.CommandSequence {
			t.Fatalf("stop command: %+v", command)
		}
	}
	after, err := session.Apply(t.Context(), request)
	if err != nil || after.Decision.Permit || !after.Fallback {
		t.Fatalf("output after stop: %+v, %v", after, err)
	}
	if action == safety.LifecycleEmergencyStop && !slices.Contains(after.Decision.Reasons, safety.ReasonEmergencyStop) {
		t.Fatal("emergency stop did not latch")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	records, _, err := audit.ReadTrusted(bytes.NewReader(store.bytes()), public)
	if err != nil {
		t.Fatal(err)
	}
	var attempted, completed bool
	for _, record := range records {
		if record.Kind != teleop.EventCommand {
			continue
		}
		var event teleop.CommandEvent
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			t.Fatal(err)
		}
		var evidence safety.EvidenceRecord
		if err := json.Unmarshal(event.Payload, &evidence); err != nil {
			t.Fatal(err)
		}
		if evidence.Lifecycle == nil || evidence.Lifecycle.Action != action {
			continue
		}
		attempted = attempted || evidence.Kind == safety.EvidenceLifecycleAttempt
		completed = completed || (evidence.Kind == safety.EvidenceLifecycleOutcome && evidence.Lifecycle.Accepted)
	}
	if !attempted || !completed {
		t.Fatalf("stop evidence: attempted=%t completed=%t", attempted, completed)
	}
	if err := session.EmergencyStop(context.Background(), "after close"); !errors.Is(err, assured.ErrSessionClosed) {
		t.Fatalf("stop after close: %v", err)
	}
}

func actionMapperForStopTest() *actions.Mapper {
	return actions.New(actions.OnButton("engaged", teleop.ButtonBumperRight, teleop.PhasePressed))
}
