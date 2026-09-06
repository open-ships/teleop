package audit_test

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/action"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/testkit"
)

// Ordinary game input plus auditing needs no safety/assured dependency,
// actuator, command policy, dead-man, exact-stream source, signer or witness.
// A bytes.Buffer is a test store; it supplies no crash durability or custody.
func TestSampledControllerActionsAndAuditWithoutActuation(t *testing.T) {
	var output bytes.Buffer
	recorder := audit.NewRecorder(&output)
	source := testkit.NewFakeSource(teleop.Descriptor{
		Capability: teleop.Capabilities{
			AuditGrade: teleop.AuditSampledState,
			Controls:   []teleop.ControlDescriptor{{ID: teleop.ButtonFaceSouth, Kind: teleop.ControlButton}},
		},
	}, 8)
	controller, err := teleop.NewController(source,
		teleop.WithDeferredStart(), teleop.WithAuditSink(recorder),
		teleop.WithProcessor(action.New(action.OnButton("jump", teleop.ButtonFaceSouth, teleop.PhasePressed))),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = controller.Close()
		_ = recorder.Close()
	})
	sub, err := controller.Subscribe(teleop.SubscriptionOptions{Delivery: teleop.DeliveryLossless})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if controller.Capabilities().AuditGrade != teleop.AuditSampledState {
		t.Fatal("controller changed the sampled source's audit grade")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := source.Push(ctx, teleop.State{}); err != nil {
		t.Fatal(err)
	}
	state := teleop.State{}
	state.SetButton(teleop.ButtonFaceSouth, true)
	if err := source.Push(ctx, state); err != nil {
		t.Fatal(err)
	}
	var jump action.Event
	for {
		event, err := sub.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := event.(action.Event); ok && value.Action == "jump" {
			jump = value
			break
		}
	}
	// The game owns execution. RecordCommandSync records its assertion only.
	commandID, err := controller.RecordCommandSync(ctx, teleop.Command{
		Name: "player.jump", Payload: map[string]any{"player": "player-7"},
		Authorized: true, Causes: []teleop.EventID{jump.Header().ID},
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
	records, verification, err := audit.Read(bytes.NewReader(output.Bytes()), audit.VerifyOptions{RequireFooter: true})
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Complete || !verification.Integrity || verification.Trusted || verification.Signed {
		t.Fatalf("ordinary audit guarantees misreported: %+v", verification)
	}
	var foundAction, foundCommand bool
	for _, record := range records {
		event, err := audit.DecodeEvent(record)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.(type) {
		case *action.Event:
			if value.Header().ID == jump.Header().ID && value.Action == "jump" {
				foundAction = true
			}
		case *teleop.CommandEvent:
			if value.Header().ID == commandID && value.Command == "player.jump" && slices.Contains(value.Header().Causes, jump.Header().ID) {
				foundCommand = true
			}
		}
	}
	if !foundAction || !foundCommand {
		t.Fatalf("replay lost action or causal command: action=%t command=%t", foundAction, foundCommand)
	}
}
