package safety

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-ships/teleop"
)

var ErrCommandPolicy = errors.New("teleop/safety: command policy refused intent")

// PolicyRequest binds command semantics to the controller session and the
// most recently acknowledged actuator command. The latter is still an adapter
// assertion; physical applications need independent system feedback.
type PolicyRequest struct {
	ControllerSession   teleop.SessionID
	EvaluatedAt         time.Time
	Command             EncodedCommand
	Input               Decision
	PreviousApplied     *EncodedCommand
	PreviouslyAppliedAt time.Time
}

func (authority *Authority) evaluatePolicy(ctx context.Context, requested EncodedCommand, input Decision) (PolicyDecision, error) {
	now, err := authority.currentTime(ctx)
	if err != nil {
		return PolicyDecision{}, errors.Join(ErrCommandPolicy, err)
	}
	if !authority.previousLiveExpiry.IsZero() && !now.Before(authority.previousLiveExpiry) {
		// Receiver expiry may have changed output to its safe state. Do not
		// invent feedback: a new acknowledged fallback must establish baseline.
		authority.previousApplied = nil
		authority.previouslyAppliedAt = time.Time{}
	}
	decision, err := callCommandPolicy(ctx, authority.policy, authority.policyCall, PolicyRequest{
		ControllerSession: authority.session, EvaluatedAt: now, Command: requested,
		Input: input, PreviousApplied: authority.previousApplied, PreviouslyAppliedAt: authority.previouslyAppliedAt,
	})
	if err != nil {
		decision.Permit = false
		return decision, errors.Join(ErrCommandPolicy, err)
	}
	now, err = authority.currentTime(ctx)
	if err != nil {
		decision.Permit = false
		return decision, errors.Join(ErrCommandPolicy, err)
	}
	if !decision.Permit || decision.PolicyID == "" || decision.Subject == "" || decision.Authorization == "" || !decision.ExpiresAt.After(now) {
		decision.Permit = false
		return decision, fmt.Errorf("%w: missing permission, identity or unexpired grant", ErrCommandPolicy)
	}
	return decision, nil
}

func (authority *Authority) recordPolicyDecision(ctx context.Context, input Decision, causes []teleop.EventID, requested EncodedCommand, policy PolicyDecision, detail string) (EvidenceID, error) {
	value := cloneAuthorityDecision(input)
	kind := EvidenceDecision
	if !policy.Permit {
		kind = EvidenceRejected
	} // terminal refusal, no future live intent.
	return authority.commit(ctx, EvidenceRecord{Kind: kind, Causes: causes,
		Decision: &value, Policy: &policy, Requested: commandPointer(requested), Detail: detail})
}

func (request PolicyRequest) Clone() PolicyRequest {
	request.Command = request.Command.Clone()
	request.Input = cloneAuthorityDecision(request.Input)
	if request.PreviousApplied != nil {
		command := request.PreviousApplied.Clone()
		request.PreviousApplied = &command
	}
	return request
}

// PolicyDecision identifies the policy and authenticated grant that permit
// exact command bytes. ExpiresAt limits the resulting actuator lease as well
// as the policy decision; expiry cannot be extended by evidence latency.
type PolicyDecision struct {
	Permit        bool            `json:"permit"`
	PolicyID      string          `json:"policy_id"`
	Subject       string          `json:"subject"`
	Authorization string          `json:"authorization"`
	ExpiresAt     time.Time       `json:"expires_at"`
	Detail        string          `json:"detail,omitempty"`
	Proof         json.RawMessage `json:"proof,omitempty"`
}

func (decision PolicyDecision) Clone() PolicyDecision {
	decision.Proof = append(json.RawMessage(nil), decision.Proof...)
	return decision
}

// CommandPolicy validates actuator identity, mapping, units, envelopes, mode,
// slew and authenticated operator permission. It must fail closed on unknown
// commands or missing proof. The policy package supplies a strict scalar
// command implementation with independently signed, session-bound grants.
type CommandPolicy interface {
	Evaluate(context.Context, PolicyRequest) (PolicyDecision, error)
}

type CommandPolicyFunc func(context.Context, PolicyRequest) (PolicyDecision, error)

func (fn CommandPolicyFunc) Evaluate(ctx context.Context, request PolicyRequest) (PolicyDecision, error) {
	if fn == nil {
		return PolicyDecision{}, ErrCommandPolicy
	}
	return fn(ctx, request.Clone())
}

func callCommandPolicy(ctx context.Context, policy CommandPolicy, gate chan struct{}, request PolicyRequest) (PolicyDecision, error) {
	if err := ctx.Err(); err != nil {
		return PolicyDecision{}, err
	}
	if !acquireCallbackGate(gate) {
		return PolicyDecision{}, fmt.Errorf("%w: previous callback is still outstanding", ErrCommandPolicy)
	}
	type outcome struct {
		decision PolicyDecision
		err      error
	}
	result := make(chan outcome, 1)
	go func() {
		value := outcome{}
		defer func() {
			if panicValue := recover(); panicValue != nil {
				value.err = fmt.Errorf("%w: command policy: %v", teleop.ErrCallbackPanic, panicValue)
			}
			releaseCallbackGate(gate)
			result <- value
		}()
		value.decision, value.err = policy.Evaluate(ctx, request.Clone())
		value.decision = value.decision.Clone()
	}()
	select {
	case value := <-result:
		return value.decision, value.err
	case <-ctx.Done():
		return PolicyDecision{}, ctx.Err()
	}
}
