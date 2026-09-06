// Package simulation provides a deterministic receiver reference model, NOT a
// hardware driver or independent watchdog. Advance is its only clock driver.
// Installed receivers must enforce these checks and expiry independently of
// the controller process, with authenticated physical feedback and hardware
// emergency stopping. A successful model receipt is simulated state only.
package simulation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/policy"
	"github.com/open-ships/teleop/safety"
)

var ErrRejected = errors.New("teleop/simulation: receiver rejected command")

type ReceiverConfig struct {
	Start        time.Time
	SenderKey    ed25519.PublicKey
	Policy       *policy.Policy
	SafeState    safety.EncodedCommand
	MaximumLease time.Duration
	// MaximumClockSkew is deducted from wall-clock expiry, never added to TTL.
	// Zero asserts synchronized clocks in this model, not in a deployment.
	MaximumClockSkew time.Duration
}

// SignedCommand binds exact command/lease/evidence IDs, a receiver boot epoch,
// and the independent operator grant under the authenticated sender key.
type SignedCommand struct {
	Epoch     [32]byte               `json:"epoch"`
	Envelope  safety.ActuatorCommand `json:"envelope"`
	Grant     *policy.SignedGrant    `json:"grant,omitempty"`
	Signature []byte                 `json:"signature"`
}

func signedBytes(message SignedCommand) ([]byte, error) {
	message.Signature = nil
	// RawMessage marshaling normalizes JSON whitespace/escaping. Authenticate
	// payload bytes separately as base64 so even those changes are detectable.
	payload := []byte(message.Envelope.Command.Payload)
	message.Envelope.Command.Payload = nil
	encoded, err := json.Marshal(struct {
		Message SignedCommand `json:"message"`
		Payload []byte        `json:"payload_bytes"`
	}{message, payload})
	if err != nil {
		return nil, err
	}
	if len(encoded) > 128<<10 {
		return nil, errors.New("simulation: command too large")
	}
	return append([]byte("teleop/receiver-model/v1\x00"), encoded...), nil
}

func SignCommand(key ed25519.PrivateKey, epoch [32]byte, command safety.ActuatorCommand, grant *policy.SignedGrant) (SignedCommand, error) {
	if len(key) != ed25519.PrivateKeySize {
		return SignedCommand{}, errors.New("simulation: invalid signing key")
	}
	message := SignedCommand{Epoch: epoch, Envelope: command.Clone()}
	if grant != nil {
		copy, _ := policy.GrantFromContext(policy.WithGrant(context.Background(), *grant))
		message.Grant = &copy
	}
	encoded, err := signedBytes(message)
	if err != nil {
		return SignedCommand{}, err
	}
	message.Signature = ed25519.Sign(key, encoded)
	return message, nil
}

type ReceiverState struct {
	Now             time.Time
	Session         teleop.SessionID
	HighestSequence uint64
	Command         safety.EncodedCommand
	Fallback        bool
	AppliedAt       time.Time
	ExpiresAt       time.Time
	Reason          string
}

type Receiver struct {
	mu     sync.Mutex
	config ReceiverConfig
	epoch  [32]byte
	state  ReceiverState
}

func NewReceiver(config ReceiverConfig) (*Receiver, error) {
	if config.Start.IsZero() || len(config.SenderKey) != ed25519.PublicKeySize || config.Policy == nil || config.MaximumLease <= 0 || config.MaximumClockSkew < 0 || config.MaximumClockSkew >= config.MaximumLease {
		return nil, errors.New("simulation: invalid receiver configuration")
	}
	config.SenderKey = slices.Clone(config.SenderKey)
	config.SafeState = config.SafeState.Clone()
	if err := config.Policy.ValidateEnvelope(config.SafeState); err != nil {
		return nil, fmt.Errorf("simulation: safe envelope: %w", err)
	}
	r := &Receiver{config: config, state: ReceiverState{Now: config.Start}}
	if _, err := rand.Read(r.epoch[:]); err != nil {
		return nil, err
	}
	r.safeAt(config.Start, "receiver boot")
	return r, nil
}

// Epoch must reach the sender through an authenticated channel. A fresh epoch
// prevents an old session's valid packets from replaying after receiver restart.
func (r *Receiver) Epoch() [32]byte { return r.epoch }

func (r *Receiver) Snapshot() ReceiverState {
	r.mu.Lock()
	defer r.mu.Unlock()
	value := r.state
	value.Command = value.Command.Clone()
	return value
}
func (r *Receiver) Now() time.Time { return r.Snapshot().Now }

// Advance drives monotonic simulation time. Expiry changes simulated output at
// the deadline even if no subsequent sender packet arrives. Never use a clock
// driven by this method as a production safety/watchdog claim.
func (r *Receiver) Advance(elapsed time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if elapsed < 0 {
		return errors.New("simulation: clock cannot move backward")
	}
	next := r.state.Now.Add(elapsed)
	if !r.state.ExpiresAt.IsZero() && !next.Before(r.state.ExpiresAt) {
		r.safeAt(r.state.ExpiresAt, "lease expired")
	}
	r.state.Now = next
	return nil
}

func (r *Receiver) safeAt(at time.Time, reason string) {
	r.state.Command = r.config.SafeState.Clone()
	r.state.Fallback = true
	r.state.AppliedAt, r.state.ExpiresAt, r.state.Reason = at, time.Time{}, reason
}

// Receive authenticates before touching state, enforces a single boot-session
// owner, rejects replay/reordering, and independently validates live policy.
// Authentic newer violations consume sequence and force simulated safe state;
// unauthenticated, foreign-session and old packets cannot revoke newer output.
func (r *Receiver) Receive(ctx context.Context, message SignedCommand) (safety.ActuatorAcknowledgment, error) {
	if err := ctx.Err(); err != nil {
		return safety.ActuatorAcknowledgment{}, err
	}
	// Freeze before verification/use so no retained pointer can alter admitted
	// bytes. Concurrent mutation during a call remains a caller data race.
	message.Envelope = message.Envelope.Clone()
	message.Signature = slices.Clone(message.Signature)
	if message.Grant != nil {
		grant, _ := policy.GrantFromContext(policy.WithGrant(context.Background(), *message.Grant))
		message.Grant = &grant
	}
	bytesToVerify, err := signedBytes(message)
	if err != nil || message.Epoch != r.epoch || !ed25519.Verify(r.config.SenderKey, bytesToVerify, message.Signature) {
		return safety.ActuatorAcknowledgment{}, ErrRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return safety.ActuatorAcknowledgment{}, err
	}
	command, now := message.Envelope, r.state.Now
	ack := safety.ActuatorAcknowledgment{ControllerSession: command.ControllerSession, Sequence: command.Sequence}
	reject := func(detail string) (safety.ActuatorAcknowledgment, error) {
		ack.Detail = detail
		return ack, fmt.Errorf("%w: %s", ErrRejected, detail)
	}
	if command.ControllerSession == (teleop.SessionID{}) || command.Sequence == 0 || command.Sequence <= r.state.HighestSequence {
		return reject("invalid session or sequence")
	}
	if r.state.Session != (teleop.SessionID{}) && command.ControllerSession != r.state.Session {
		return reject("receiver already owned by another session")
	}
	if r.state.Session == (teleop.SessionID{}) && (!command.Fallback || command.Sequence != 1) {
		return reject("new session requires sequence-one safe bootstrap")
	}
	r.state.HighestSequence = command.Sequence
	if command.TTL <= 0 || command.TTL > r.config.MaximumLease || command.ExpiresAt.Sub(command.IssuedAt) != command.TTL || command.IssuedAt.After(now.Add(r.config.MaximumClockSkew)) || !command.ExpiresAt.After(now.Add(r.config.MaximumClockSkew)) {
		r.safeAt(now, "invalid or expired newer lease")
		return reject("invalid or expired lease")
	}
	if command.Fallback {
		if command.Command.Name != r.config.SafeState.Name || !bytes.Equal(command.Command.Payload, r.config.SafeState.Payload) {
			r.safeAt(now, "invalid safe command")
			return reject("fallback does not match approved safe bytes")
		}
		r.state.Session = command.ControllerSession
		r.safeAt(now, "acknowledged safe state")
	} else {
		if command.DecisionID == "" || command.IntentID == "" || message.Grant == nil {
			r.safeAt(now, "missing live proof")
			return reject("missing evidence identity or operator grant")
		}
		policyContext := policy.WithGrant(ctx, *message.Grant)
		baseline := r.state.Command.Clone()
		decision, policyErr := r.config.Policy.Evaluate(policyContext, safety.PolicyRequest{ControllerSession: command.ControllerSession, EvaluatedAt: now, Input: safety.Decision{Permit: true}, Command: command.Command.Clone(), PreviousApplied: &baseline, PreviouslyAppliedAt: r.state.AppliedAt})
		if policyErr != nil || !decision.Permit || command.ExpiresAt.After(decision.ExpiresAt) {
			r.safeAt(now, "live policy refused")
			return reject("live command outside independently checked policy/grant")
		}
		r.state.Command = command.Command.Clone()
		r.state.Fallback = false
		r.state.AppliedAt = now
		r.state.ExpiresAt = command.ExpiresAt.Add(-r.config.MaximumClockSkew)
		r.state.Reason = "simulated live output"
	}
	ack.Accepted, ack.AppliedAt, ack.Detail = true, now, "simulated receiver state; not physical feedback"
	return ack, nil
}

// Actuator returns an in-process, authenticated reference adapter for Authority
// integration tests. It has no network or hardware side effects. Real adapters
// need equivalent checks on the receiving hardware, not solely in this client.
func (r *Receiver) Actuator(key ed25519.PrivateKey) (safety.Actuator, error) {
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(key.Public().(ed25519.PublicKey), r.config.SenderKey) {
		return nil, errors.New("simulation: sender key does not match receiver trust")
	}
	key = slices.Clone(key)
	return safety.ActuatorFunc(func(ctx context.Context, command safety.ActuatorCommand) (safety.ActuatorReceipt, error) {
		grant, ok := policy.GrantFromContext(ctx)
		var credential *policy.SignedGrant
		if ok {
			credential = &grant
		}
		message, err := SignCommand(key, r.epoch, command, credential)
		if err != nil {
			return nil, err
		}
		ack, err := r.Receive(ctx, message)
		if err != nil {
			return nil, err
		}
		return safety.ReceiptFunc(func(ctx context.Context) (safety.ActuatorAcknowledgment, error) {
			if err := ctx.Err(); err != nil {
				return safety.ActuatorAcknowledgment{}, err
			}
			return ack, nil
		}), nil
	}), nil
}
