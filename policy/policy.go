// Package policy implements default-deny scalar command envelopes and
// independently signed, session-bound operator grants. Limits must come from
// an installation hazard analysis; this package supplies no vessel defaults.
package policy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/internal/strictjson"
	"github.com/open-ships/teleop/safety"
)

const maximumPayload = 64 << 10

type Scalar struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}
type Command struct {
	Actuator string            `json:"actuator"`
	Mode     string            `json:"mode"`
	Values   map[string]Scalar `json:"values"`
}
type Limit struct {
	Unit    string  `json:"unit"`
	Minimum float64 `json:"minimum"`
	Maximum float64 `json:"maximum"`
	// MaximumRate is units/second. Zero explicitly disables slew enforcement.
	MaximumRate float64 `json:"maximum_rate"`
}
type Rule struct {
	Actuator string           `json:"actuator"`
	Mode     string           `json:"mode"`
	Fields   map[string]Limit `json:"fields"`
	// From lists allowed previous command names, including self-transitions.
	// Every live command requires a known, validated acknowledged baseline.
	From []string `json:"from"`
}
type Config struct {
	ID              string            `json:"id"`
	AuthorityKey    ed25519.PublicKey `json:"authority_key"`
	MaximumGrantTTL time.Duration     `json:"maximum_grant_ttl"`
	Rules           map[string]Rule   `json:"rules"`
}
type Policy struct {
	config   Config
	identity string
}

func New(config Config) (*Policy, error) {
	if config.ID == "" || len(config.AuthorityKey) != ed25519.PublicKeySize || config.MaximumGrantTTL <= 0 || len(config.Rules) == 0 || len(config.Rules) > 256 {
		return nil, errors.New("policy: invalid identity, key, grant TTL or rules")
	}
	// JSON snapshot also refuses non-finite configuration values and isolates
	// all caller-owned maps/slices. No mutable configuration is retained.
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maximumPayload {
		return nil, errors.New("policy: configuration too large")
	}
	var frozen Config
	if err := json.Unmarshal(encoded, &frozen); err != nil {
		return nil, err
	}
	for name, rule := range frozen.Rules {
		if name == "" || rule.Actuator == "" || rule.Mode == "" || len(rule.Fields) == 0 || len(rule.Fields) > 64 || len(rule.From) == 0 {
			return nil, fmt.Errorf("policy: incomplete rule %q", name)
		}
		for field, limit := range rule.Fields {
			if field == "" || limit.Unit == "" || limit.Minimum > limit.Maximum || limit.MaximumRate < 0 || !finite(limit.Minimum) || !finite(limit.Maximum) || !finite(limit.MaximumRate) {
				return nil, fmt.Errorf("policy: invalid limit %q/%q", name, field)
			}
		}
		for _, from := range rule.From {
			previous, ok := frozen.Rules[from]
			if !ok || previous.Actuator != rule.Actuator {
				return nil, fmt.Errorf("policy: unknown or cross-actuator transition %q -> %q", from, name)
			}
			for field, limit := range rule.Fields {
				if limit.MaximumRate > 0 {
					baseline, ok := previous.Fields[field]
					if !ok || baseline.Unit != limit.Unit {
						return nil, fmt.Errorf("policy: missing rate baseline %q/%q", from, field)
					}
				}
			}
		}
	}
	digest := sha256.Sum256(encoded)
	return &Policy{config: frozen, identity: config.ID + ":sha256:" + hex.EncodeToString(digest[:])}, nil
}

// Configuration returns the exact immutable policy for provenance retention.
func (policy *Policy) Configuration() Config {
	encoded, _ := json.Marshal(policy.config)
	var copy Config
	_ = json.Unmarshal(encoded, &copy)
	return copy
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// Decode rejects ambiguous fields, omitted/null scalars, duplicates and excess
// nesting before typed decoding; JSON field names are strictly case-sensitive.
// Scalar numbers must use the canonical JSON representation produced for a
// float64 by encoding/json. This rejects silent integer rounding, underflow,
// and extra digits that would authorize different bytes as the same value.
func Decode(encoded []byte) (Command, error) {
	var command Command
	if err := strictjson.Validate(encoded, maximumPayload); err != nil {
		return command, err
	}
	fields, err := strictjson.Object(encoded, "actuator", "mode", "values")
	if err != nil {
		return command, err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(fields["values"], &values); err != nil {
		return command, err
	}
	if len(values) == 0 || len(values) > 64 {
		return command, errors.New("policy: invalid scalar count")
	}
	for _, scalar := range values {
		fields, err := strictjson.Object(scalar, "unit", "value")
		if err != nil {
			return command, err
		}
		var numeric float64
		if err := json.Unmarshal(fields["value"], &numeric); err != nil {
			return command, err
		}
		canonical, err := json.Marshal(numeric)
		if err != nil || !bytes.Equal(bytes.TrimSpace(fields["value"]), canonical) {
			return command, errors.New("policy: scalar must be canonical finite float64 JSON")
		}
	}
	if err := json.Unmarshal(encoded, &command); err != nil {
		return Command{}, err
	}
	return command, nil
}

func (policy *Policy) validate(command safety.EncodedCommand) (Command, Rule, error) {
	rule, ok := policy.config.Rules[command.Name]
	if !ok {
		return Command{}, Rule{}, errors.New("unknown command")
	}
	decoded, err := Decode(command.Payload)
	if err != nil {
		return Command{}, Rule{}, err
	}
	if decoded.Actuator != rule.Actuator || decoded.Mode != rule.Mode || len(decoded.Values) != len(rule.Fields) {
		return Command{}, Rule{}, errors.New("actuator, mode or scalar set mismatch")
	}
	for name, limit := range rule.Fields {
		value, ok := decoded.Values[name]
		if !ok || value.Unit != limit.Unit || !finite(value.Value) || value.Value < limit.Minimum || value.Value > limit.Maximum {
			return Command{}, Rule{}, fmt.Errorf("scalar %q outside envelope", name)
		}
	}
	return decoded, rule, nil
}

// ValidateEnvelope checks static identity, units and ranges, including safe
// states. It does not grant live authorization or bypass transition/slew checks.
func (policy *Policy) ValidateEnvelope(command safety.EncodedCommand) error {
	_, _, err := policy.validate(command)
	return err
}

func (policy *Policy) Evaluate(ctx context.Context, request safety.PolicyRequest) (safety.PolicyDecision, error) {
	decision := safety.PolicyDecision{PolicyID: policy.identity}
	deny := func(err error) (safety.PolicyDecision, error) {
		decision.Detail = err.Error()
		return decision, errors.Join(safety.ErrCommandPolicy, err)
	}
	if err := ctx.Err(); err != nil {
		return deny(err)
	}
	if !request.Input.Permit || request.EvaluatedAt.IsZero() || request.ControllerSession == (teleop.SessionID{}) {
		return deny(errors.New("missing interlock or session/time identity"))
	}
	command, rule, err := policy.validate(request.Command)
	if err != nil {
		return deny(err)
	}
	grant, ok := ctx.Value(grantContextKey{}).(SignedGrant)
	if !ok {
		return deny(errors.New("operator grant missing"))
	}
	decision.Subject, decision.Authorization, decision.ExpiresAt = grant.Grant.Subject, grant.Grant.ID, grant.Grant.ExpiresAt
	decision.Proof, err = json.Marshal(grant)
	if err != nil {
		return deny(err)
	}
	if err := policy.verifyGrant(grant, request.ControllerSession, request.EvaluatedAt, Scope{Command: request.Command.Name, Actuator: command.Actuator, Mode: command.Mode}); err != nil {
		return deny(err)
	}
	if request.PreviousApplied == nil || request.PreviouslyAppliedAt.IsZero() || request.PreviouslyAppliedAt.After(request.EvaluatedAt) {
		return deny(errors.New("acknowledged baseline missing or from the future"))
	}
	previous, _, err := policy.validate(*request.PreviousApplied)
	if err != nil {
		return deny(fmt.Errorf("invalid baseline: %w", err))
	}
	if !slices.Contains(rule.From, request.PreviousApplied.Name) {
		return deny(errors.New("mode/command transition forbidden"))
	}
	seconds := request.EvaluatedAt.Sub(request.PreviouslyAppliedAt).Seconds()
	for field, limit := range rule.Fields {
		if limit.MaximumRate == 0 {
			continue
		}
		baseline, ok := previous.Values[field]
		if !ok || baseline.Unit != limit.Unit || math.Abs(command.Values[field].Value-baseline.Value)/limit.MaximumRate > seconds {
			return deny(fmt.Errorf("scalar %q exceeds slew limit", field))
		}
	}
	decision.Permit = true
	return decision, nil
}
