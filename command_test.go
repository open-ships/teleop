package teleop_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/testkit"
)

func TestRecordCommandPublishesCausalAuditableEvent(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "command"}, 4)
	var output bytes.Buffer
	recorder := audit.NewRecorder(&output)
	controller, err := teleop.NewController(source, teleop.WithAuditSink(recorder))
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 64})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state := teleop.State{}
	state.SetButton(teleop.ButtonFaceSouth, true)
	if err := source.Push(ctx, state); err != nil {
		t.Fatal(err)
	}

	var cause teleop.EventID
	for cause == (teleop.EventID{}) {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if button, ok := event.(teleop.ButtonEvent); ok &&
			button.Button == teleop.ButtonFaceSouth &&
			button.Phase == teleop.PhasePressed {
			cause = button.Meta.ID
		}
	}
	if err := controller.RecordCommand(ctx, teleop.Command{
		Name:       "drive.set",
		Payload:    map[string]any{"forward": 0.5},
		Authorized: true,
		Causes:     []teleop.EventID{cause},
	}); err != nil {
		t.Fatal(err)
	}

	var command teleop.CommandEvent
	for command.Command == "" {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := event.(teleop.CommandEvent); ok {
			command = value
		}
	}
	if command.Command != "drive.set" || !command.Authorized ||
		len(command.Meta.Causes) != 1 || command.Meta.Causes[0] != cause {
		t.Fatalf("command event = %#v", command)
	}
	var payload map[string]float64
	if err := json.Unmarshal(command.Payload, &payload); err != nil || payload["forward"] != 0.5 {
		t.Fatalf("command payload = %q, %v", command.Payload, err)
	}

	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := audit.ReadAll(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatalf("verify command audit: %v", err)
	}
	var decoded bool
	for _, record := range records {
		if record.Kind != teleop.EventCommand {
			continue
		}
		event, err := audit.DecodeEvent(record)
		if err != nil {
			t.Fatal(err)
		}
		value, ok := event.(*teleop.CommandEvent)
		if !ok || value.Command != "drive.set" {
			t.Fatalf("decoded command = %#v", event)
		}
		decoded = true
	}
	if !decoded {
		t.Fatal("command event missing from audit log")
	}
}

func TestRecordCommandRejectsInvalidAndTerminalRequests(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "command-terminal"}, 1)
	controller, err := teleop.NewController(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.RecordCommand(context.Background(), teleop.Command{}); !errors.Is(err, teleop.ErrInvalidState) {
		t.Fatalf("empty command error = %v, want ErrInvalidState", err)
	}
	if err := controller.RecordCommand(context.Background(), teleop.Command{
		Name: "unknown.cause",
		Causes: []teleop.EventID{{
			Stream:   "input",
			Sequence: 1,
		}},
	}); !errors.Is(err, teleop.ErrInvalidState) {
		t.Fatalf("unknown cause error = %v, want ErrInvalidState", err)
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err := controller.RecordCommand(context.Background(), teleop.Command{Name: "after.close"}); !errors.Is(err, teleop.ErrClosed) {
		t.Fatalf("terminal command error = %v, want ErrClosed", err)
	}
}
