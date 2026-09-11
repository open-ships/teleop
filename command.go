package teleop

import (
	"context"
	"encoding/json"
	"errors"
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
//
// This is an audit record, not an execution request: recording never sends a
// command or evaluates a policy. Authorized is the application's assertion,
// not a permission established by teleop. The optional safety.Command is the
// separate intent type accepted by Safety Authority.
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
	response chan commandResult
}

type commandResult struct {
	id  EventID
	err error
}

// RecordCommand publishes a CommandEvent to every subscription and audit sink.
//
// Queue admission does not wait for space or sink callbacks. Payload JSON
// serialization runs synchronously in the caller, including any custom
// MarshalJSON method; callers must bound that work. A full queue returns
// ErrPipelineOverflow immediately; a safety-critical caller
// should treat that error as a fault and inhibit output, because the command
// it just issued is not in the record.
func (c *Controller) RecordCommand(ctx context.Context, command Command) error {
	request, err := c.prepareCommand(ctx, command, false)
	if err != nil {
		return err
	}

	// A caller may use a deferred controller solely for audit-backed command
	// recording. Starting here preserves the method's promise to publish rather
	// than leaving an accepted request stranded until some unrelated Snapshot or
	// Subscribe call.
	c.start()
	accepted, open, _ := c.tryEnqueueCommand(request)
	if accepted {
		return nil
	}
	if !open {
		return c.closedError()
	}
	return fmt.Errorf("%w: command queue is full", ErrPipelineOverflow)
}

// RecordCommandSync publishes a CommandEvent and waits until every attached
// sink callback has completed for it. Because sink queues are FIFO, success
// also establishes that each sink completed every event admitted before this
// command. A sink still defines its own durability semantics; for example,
// audit.Recorder returns only after its local store has been synchronized.
//
// Once the request enters the controller queue, cancellation cannot retract
// it. A caller that stops waiting receives ErrCommandPublicationUncertain
// joined with its context error; the command may finish publication later.
// A processor failure after the command event itself was recorded and exposed
// carries the same marker. Treat either case as indeterminate and fail safe.
//
// This method is intentionally available on the concrete Controller without
// expanding GameController. Safety integrations can opt into the stronger
// contract without breaking third-party GameController implementations.
func (c *Controller) RecordCommandSync(ctx context.Context, command Command) (EventID, error) {
	request, err := c.prepareCommand(ctx, command, true)
	if err != nil {
		return EventID{}, err
	}

	c.start()
	for {
		accepted, open, space := c.tryEnqueueCommand(request)
		if accepted {
			break
		}
		if !open {
			return EventID{}, c.closedError()
		}
		select {
		case <-space:
		case <-c.commandClosed:
			return EventID{}, c.closedError()
		case <-ctx.Done():
			return EventID{}, ctx.Err()
		case <-c.done:
			return EventID{}, c.closedError()
		}
	}
	return c.waitForCommandResult(ctx, request.response)
}

func (c *Controller) waitForCommandResult(
	ctx context.Context,
	response <-chan commandResult,
) (EventID, error) {
	select {
	case result := <-response:
		return result.id, result.err
	case <-ctx.Done():
		// Completion may have become ready with cancellation. Preserve the exact
		// outcome whenever it is already buffered before declaring uncertainty.
		select {
		case result := <-response:
			return result.id, result.err
		default:
		}
		return EventID{}, errors.Join(ErrCommandPublicationUncertain, ctx.Err())
	case <-c.done:
		// A buffered response may have raced with terminal completion. Prefer
		// the exact command outcome when it is already available.
		select {
		case result := <-response:
			return result.id, result.err
		default:
			return EventID{}, errors.Join(
				ErrCommandPublicationUncertain,
				c.closedError(),
			)
		}
	}
}

// tryEnqueueCommand makes queue admission indivisible from the terminal
// admission gate. terminate closes that gate under the same mutex before it
// drains, so accepted commands cannot arrive after the final drain.
func (c *Controller) tryEnqueueCommand(
	request commandRequest,
) (accepted bool, open bool, space <-chan struct{}) {
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	if !c.commandAccepting {
		return false, false, nil
	}
	select {
	case c.external <- request:
		return true, true, nil
	default:
		return false, true, c.commandSpace
	}
}

func (c *Controller) prepareCommand(
	ctx context.Context,
	command Command,
	synchronous bool,
) (commandRequest, error) {
	if ctx == nil {
		return commandRequest{}, fmt.Errorf("%w: nil command context", ErrInvalidState)
	}
	if err := ctx.Err(); err != nil {
		return commandRequest{}, err
	}
	if command.Name == "" {
		return commandRequest{}, fmt.Errorf("%w: command name is empty", ErrInvalidState)
	}
	if err := c.validateCommandCauses(command.Causes); err != nil {
		return commandRequest{}, err
	}
	var payload json.RawMessage
	if command.Payload != nil {
		encoded, err := json.Marshal(command.Payload)
		if err != nil {
			return commandRequest{}, fmt.Errorf("marshal command payload: %w", err)
		}
		payload = encoded
	}
	request := commandRequest{
		command:  command,
		payload:  payload,
		issuedAt: c.clock.Now(),
	}
	if synchronous {
		request.response = make(chan commandResult, 1)
	}
	request.command.Causes = slices.Clone(command.Causes)
	request.command.Payload = nil
	return request, nil
}

func (c *Controller) closedError() error {
	if err := c.Err(); err != nil {
		return err
	}
	return ErrClosed
}

func (c *Controller) validateCommandCauses(causes []EventID) error {
	if len(causes) == 0 {
		return nil
	}
	c.publishedMu.RLock()
	defer c.publishedMu.RUnlock()
	for _, cause := range causes {
		if cause.Session != c.session ||
			!c.published.Contains(cause.Stream, cause.Sequence) {
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
	event := CommandEvent{
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
	}
	var err error
	if request.response == nil {
		err = c.publish(event)
	} else {
		err = c.publishSynchronous(event)
		request.response <- commandResult{id: event.Meta.ID, err: err}
	}
	return err
}
