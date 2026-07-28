package safety_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/safety"
	"github.com/open-ships/teleop/testkit"
)

const deadMan = teleop.ButtonBumperRight

func descriptor() teleop.Descriptor {
	return teleop.Descriptor{
		ID:      "fake:0",
		Backend: "testkit",
		Capability: teleop.Capabilities{
			Controls: []teleop.ControlDescriptor{
				{ID: deadMan, Kind: teleop.ControlButton},
			},
		},
	}
}

// waitForState blocks until the controller's snapshot satisfies want. The
// input stream carries connection and capability events too, so a sequence
// number is not a reliable signal that a specific observation was processed.
func waitForState(
	t *testing.T,
	controller *teleop.Controller,
	description string,
	want func(teleop.State) bool,
) teleop.StateMeta {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, meta := controller.SnapshotWithMeta()
		if want(state) {
			return meta
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("controller never reached state: %s", description)
	return teleop.StateMeta{}
}

func deadManHeld(state teleop.State) bool  { return state.Button(deadMan) }
func deadManClear(state teleop.State) bool { return !state.Button(deadMan) }

// TestGuardedSessionIsFullyRecorded exercises the whole chain: a guard gates
// output, the application records the command it issued, and the signed audit
// log reproduces the decision, the command, and the causal link between them.
func TestGuardedSessionIsFullyRecorded(t *testing.T) {
	public, private, err := audit.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	provenance := audit.CaptureProvenance()
	provenance.Application = "integration-test"
	provenance.Operator = "operator-1"
	provenance.Config = map[string]any{"command_timeout_ms": 500}

	log := &bytes.Buffer{}
	recorder := audit.NewRecorder(
		log,
		audit.WithSigner(private),
		audit.WithProvenance(provenance),
		audit.WithCheckpoints(0, 5),
	)

	source := testkit.NewFakeSource(descriptor(), 16)
	guard := safety.New(
		safety.WithCommandTimeout(500*time.Millisecond),
		safety.WithDeadMan(deadMan),
		safety.WithLoopWatchdog(time.Second),
	)
	controller, err := teleop.NewController(
		source,
		teleop.WithProcessor(guard),
		teleop.WithAuditSink(recorder),
	)
	if err != nil {
		t.Fatal(err)
	}
	guard.Bind(controller)

	var held teleop.State
	held.SetButton(deadMan, true)
	if err := source.Push(t.Context(), held); err != nil {
		t.Fatal(err)
	}
	waitForState(t, controller, "dead-man held", deadManHeld)

	guard.Heartbeat()
	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}
	decision := guard.Evaluate()
	if !decision.Permit {
		t.Fatalf("guard must permit output: %v", decision.Reasons)
	}

	_, meta := controller.SnapshotWithMeta()
	err = controller.RecordCommand(t.Context(), teleop.Command{
		Name:       "thrust.set",
		Payload:    map[string]any{"newtons": 120},
		Authorized: decision.Permit,
		Causes: []teleop.EventID{{
			Session:  controller.Session(),
			Stream:   "input",
			Sequence: meta.Sequence,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Release the dead-man control and record the inhibited command that the
	// application still computed, which is what shows the gate did its job.
	if err := source.Push(t.Context(), teleop.State{}); err != nil {
		t.Fatal(err)
	}
	waitForState(t, controller, "dead-man released", deadManClear)
	guard.Heartbeat()
	inhibited := guard.Evaluate()
	if inhibited.Permit {
		t.Fatal("releasing the dead-man control must inhibit output")
	}
	err = controller.RecordCommand(t.Context(), teleop.Command{
		Name:       "thrust.set",
		Payload:    map[string]any{"newtons": 0},
		Authorized: false,
		Reason:     string(safety.ReasonDeadManReleased),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	records, verification, err := audit.Read(log, audit.VerifyOptions{
		RequireFooter:    true,
		RequireSignature: true,
		PublicKey:        public,
	})
	if err != nil {
		t.Fatalf("verify recorded session: %v", err)
	}
	if !verification.Trusted {
		t.Fatal("session must verify against the trusted key")
	}
	if verification.Provenance == nil ||
		verification.Provenance.Application != "integration-test" {
		t.Fatalf("provenance = %+v", verification.Provenance)
	}
	if verification.Session != controller.Session() {
		t.Fatal("manifest session must match the controller session")
	}

	var (
		decisions []safety.Event
		commands  []teleop.CommandEvent
		monotonic bool
		known     = make(map[teleop.EventID]struct{})
	)
	for _, record := range records {
		known[record.Header.ID] = struct{}{}
		if record.Header.Monotonic > 0 {
			monotonic = true
		}
		switch record.Kind {
		case safety.EventDecision:
			var event safety.Event
			if err := json.Unmarshal(record.Payload, &event); err != nil {
				t.Fatal(err)
			}
			decisions = append(decisions, event)
		case teleop.EventCommand:
			var event teleop.CommandEvent
			if err := json.Unmarshal(record.Payload, &event); err != nil {
				t.Fatal(err)
			}
			commands = append(commands, event)
		}
	}

	if !monotonic {
		t.Fatal("recorded headers must carry monotonic readings")
	}
	if len(decisions) < 2 {
		t.Fatalf("expected safety transitions in the log, got %d", len(decisions))
	}
	if len(commands) != 2 {
		t.Fatalf("expected 2 recorded commands, got %d", len(commands))
	}
	if !commands[0].Authorized {
		t.Fatal("the first command must be recorded as authorized")
	}
	if commands[1].Authorized {
		t.Fatal("the second command must be recorded as inhibited")
	}
	if commands[1].Reason != string(safety.ReasonDeadManReleased) {
		t.Fatalf("inhibit reason = %q", commands[1].Reason)
	}

	// The authorized command must be traceable back to the observation that
	// produced it; a command with no cause is not reconstructable.
	if len(commands[0].Meta.Causes) == 0 {
		t.Fatal("the authorized command must record its cause")
	}
	for _, cause := range commands[0].Meta.Causes {
		if _, ok := known[cause]; !ok {
			t.Fatalf("cause %v is not present in the log", cause)
		}
	}

	// A transition to inhibited must be recorded, not merely implied by the
	// absence of an authorization.
	inhibitedRecorded := false
	for _, event := range decisions {
		if !event.Permit {
			inhibitedRecorded = true
		}
	}
	if !inhibitedRecorded {
		t.Fatal("an inhibiting transition must appear in the log")
	}
}

// TestGuardTripsWithoutApplicationInvolvement proves the guard inhibits on its
// own timer: an application that stops calling Evaluate still has its stale
// authorization revoked and recorded.
func TestGuardTripsWithoutApplicationInvolvement(t *testing.T) {
	log := &bytes.Buffer{}
	recorder := audit.NewRecorder(log)
	source := testkit.NewFakeSource(descriptor(), 16)
	guard := safety.New(safety.WithCommandTimeout(30 * time.Millisecond))

	controller, err := teleop.NewController(
		source,
		teleop.WithProcessor(guard),
		teleop.WithAuditSink(recorder),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	guard.Bind(controller)

	if err := source.Push(t.Context(), teleop.State{}); err != nil {
		t.Fatal(err)
	}
	waitForState(t, controller, "first observation", func(teleop.State) bool {
		_, meta := controller.SnapshotWithMeta()
		return meta.Sequence > 0 && !meta.Synthetic
	})
	if err := guard.Arm(); err != nil {
		t.Fatal(err)
	}
	if !guard.Evaluate().Permit {
		t.Fatal("guard must permit immediately after arming on fresh input")
	}

	// Deliver nothing further and never call Evaluate again. The controller's
	// advance ticker must drive the guard to a safe state on its own.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if guard.State() == safety.StateSafe {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("guard did not trip on its own timer")
}
