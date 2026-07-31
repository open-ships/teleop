package teleop

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Command describes a command an application issued to the system under
// control, so it can be recorded alongside the input that caused it.
//
// A log of input alone leaves the causal chain incomplete. Reconstructing an
// incident requires knowing what the machine was told to do, which is a
// function of the application's own mapping from input to actuation, not of
// the controller state teleop observes.
type Command struct {
	// Name identifies the command in an application-defined vocabulary.
	Name string
	// Payload is marshaled to JSON and recorded verbatim. A nil payload
	// records the name alone.
	Payload any
	// Authorized reports whether a safety gate permitted this command at the
	// instant it was issued. Recording an unauthorized command is meaningful:
	// it shows the application computed one and the gate inhibited it.
	Authorized bool
	// Reason explains an unauthorized or degraded command.
	Reason string
	// Causes links the command to the input events that produced it. Every
	// cause must already have been published.
	Causes []EventID
}

// commandRequest carries a command from an application goroutine to the run
// loop. Event identity is allocated during publication rather than here, so
// concurrent callers cannot produce out-of-order sequence numbers.
type commandRequest struct {
	command  Command
	payload  json.RawMessage
	issuedAt time.Time
}

// RecordCommand publishes a CommandEvent to every subscription and audit sink.
//
// It never blocks the caller. A full queue returns ErrPipelineOverflow
// immediately rather than stalling a control loop; a safety-critical caller
// should treat that error as a fault and inhibit output, because the command
// it just issued is not in the record.
func (c *Controller) RecordCommand(ctx context.Context, command Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if command.Name == "" {
		return fmt.Errorf("%w: command name is empty", ErrInvalidState)
	}
	if err := c.validateCommandCauses(command.Causes); err != nil {
		return err
	}
	var payload json.RawMessage
	if command.Payload != nil {
		encoded, err := json.Marshal(command.Payload)
		if err != nil {
			return fmt.Errorf("marshal command payload: %w", err)
		}
		payload = encoded
	}
	request := commandRequest{
		command:  command,
		payload:  payload,
		issuedAt: c.clock.Now(),
	}
	request.command.Causes = slices.Clone(command.Causes)
	request.command.Payload = nil

	// A caller may use a deferred controller solely for audit-backed command
	// recording. Starting here preserves the method's promise to publish rather
	// than leaving an accepted request stranded until some unrelated Snapshot or
	// Subscribe call.
	c.start()
	select {
	case <-c.done:
		if err := c.Err(); err != nil {
			return err
		}
		return ErrClosed
	default:
	}
	select {
	case c.external <- request:
		return nil
	case <-c.done:
		if err := c.Err(); err != nil {
			return err
		}
		return ErrClosed
	default:
		return fmt.Errorf("%w: command queue is full", ErrPipelineOverflow)
	}
}

func (c *Controller) validateCommandCauses(causes []EventID) error {
	if len(causes) == 0 {
		return nil
	}
	c.knownMu.RLock()
	defer c.knownMu.RUnlock()
	for _, cause := range causes {
		if _, ok := c.known[cause]; !ok {
			return fmt.Errorf(
				"%w: command cause %s/%s/%d has not been published",
				ErrInvalidState,
				cause.Session,
				cause.Stream,
				cause.Sequence,
			)
		}
	}
	return nil
}

func (c *Controller) handleCommand(request commandRequest) error {
	return c.publish(CommandEvent{
		Meta: c.nextHeaderAt(
			"command",
			request.issuedAt,
			request.issuedAt,
			0,
			request.command.Causes,
			false,
		),
		Command:    request.command.Name,
		Payload:    request.payload,
		Authorized: request.command.Authorized,
		Reason:     request.command.Reason,
	})
}
