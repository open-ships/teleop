package teleop_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/testkit"
)

func TestCommandAdmissionClosesBeforeTerminalDrainCompletes(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *teleop.Controller) (teleop.EventID, error)
	}{
		{
			name: "asynchronous",
			call: func(ctx context.Context, controller *teleop.Controller) (teleop.EventID, error) {
				return teleop.EventID{}, controller.RecordCommand(
					ctx,
					teleop.Command{Name: "late.async"},
				)
			},
		},
		{
			name: "synchronous",
			call: func(ctx context.Context, controller *teleop.Controller) (teleop.EventID, error) {
				return controller.RecordCommandSync(
					ctx,
					teleop.Command{Name: "late.sync"},
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sink := newTerminalCommandGateSink()
			source := testkit.NewFakeSource(
				teleop.Descriptor{ID: teleop.DeviceID("command-shutdown-" + test.name)},
				1,
			)
			controller, err := teleop.NewController(
				source,
				teleop.WithAuditSink(sink),
				teleop.WithSynchronousAudit(),
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				sink.unblock()
				_ = controller.Close()
			})

			closeResult := make(chan error, 1)
			go func() { closeResult <- controller.Close() }()
			select {
			case <-sink.entered:
			case <-time.After(time.Second):
				t.Fatal("controller did not reach post-drain terminal publication")
			}

			commandCtx, cancelCommand := context.WithTimeout(t.Context(), time.Second)
			id, recordErr := test.call(commandCtx, controller)
			cancelCommand()
			if id != (teleop.EventID{}) {
				t.Fatalf("rejected command ID = %v, want zero", id)
			}
			if !errors.Is(recordErr, teleop.ErrClosed) {
				t.Fatalf("post-drain command error = %v, want ErrClosed", recordErr)
			}
			if errors.Is(recordErr, teleop.ErrCommandPublicationUncertain) {
				t.Fatalf("command rejected before admission was marked uncertain: %v", recordErr)
			}

			sink.unblock()
			select {
			case closeErr := <-closeResult:
				if closeErr != nil {
					t.Fatal(closeErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("controller close did not finish")
			}
			if commands := sink.commandNames(); len(commands) != 0 {
				t.Fatalf("post-drain command reached sink: %v", commands)
			}
		})
	}
}

type terminalCommandGateSink struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once

	mu       sync.Mutex
	commands []string
}

func newTerminalCommandGateSink() *terminalCommandGateSink {
	return &terminalCommandGateSink{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (sink *terminalCommandGateSink) Record(ctx context.Context, event teleop.Event) error {
	if command, ok := event.(teleop.CommandEvent); ok {
		sink.mu.Lock()
		sink.commands = append(sink.commands, command.Command)
		sink.mu.Unlock()
	}
	observation, ok := event.(teleop.ObservationEvent)
	if !ok || observation.Native.Format != "teleop.synthetic.disconnect" {
		return nil
	}
	sink.enteredOnce.Do(func() { close(sink.entered) })
	select {
	case <-sink.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sink *terminalCommandGateSink) unblock() {
	sink.releaseOnce.Do(func() { close(sink.release) })
}

func (sink *terminalCommandGateSink) commandNames() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]string(nil), sink.commands...)
}
