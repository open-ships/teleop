package policy_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/policy"
	"github.com/open-ships/teleop/safety"
)

func fixture(t testing.TB) (*policy.Policy, policy.Config, ed25519.PrivateKey, safety.PolicyRequest, policy.Grant) {
	t.Helper()
	key, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := policy.Config{ID: "simulation-only", AuthorityKey: key, MaximumGrantTTL: time.Minute, Rules: map[string]policy.Rule{
		"safe":  {Actuator: "sim", Mode: "stop", Fields: map[string]policy.Limit{"power": {Unit: "fraction", Minimum: 0, Maximum: 0}}, From: []string{"safe", "drive"}},
		"drive": {Actuator: "sim", Mode: "manual", Fields: map[string]policy.Limit{"power": {Unit: "fraction", Minimum: -1, Maximum: 1, MaximumRate: 0.5}}, From: []string{"safe", "drive"}},
	}}
	p, err := policy.New(config)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := teleop.SessionID{1}
	safe := encode(t, "safe", "stop", 0)
	request := safety.PolicyRequest{ControllerSession: session, EvaluatedAt: now, Input: safety.Decision{Permit: true}, Command: encode(t, "drive", "manual", 0.25), PreviousApplied: &safe, PreviouslyAppliedAt: now.Add(-time.Second)}
	grant := policy.Grant{ID: "grant-1", Subject: "operator-1", Session: session, NotBefore: now.Add(-time.Second), ExpiresAt: now.Add(30 * time.Second), Scopes: []policy.Scope{{Command: "drive", Actuator: "sim", Mode: "manual"}}}
	return p, config, private, request, grant
}
func encode(t testing.TB, name, mode string, value float64) safety.EncodedCommand {
	t.Helper()
	encoded, err := json.Marshal(policy.Command{Actuator: "sim", Mode: mode, Values: map[string]policy.Scalar{"power": {Value: value, Unit: "fraction"}}})
	if err != nil {
		t.Fatal(err)
	}
	return safety.EncodedCommand{Name: name, Payload: encoded}
}
func TestPolicyExactGrantEnvelopeAndSnapshot(t *testing.T) {
	p, config, private, request, grant := fixture(t)
	signed, err := policy.SignGrant(private, grant)
	if err != nil {
		t.Fatal(err)
	}
	ctx := policy.WithGrant(t.Context(), signed)
	signed.Grant.Scopes[0].Command = "other"
	signed.Signature[0] ^= 1
	config.Rules["drive"].Fields["power"] = policy.Limit{Unit: "bad"}
	config.AuthorityKey[0] ^= 1
	copy := p.Configuration()
	copy.Rules["drive"].Fields["power"] = policy.Limit{Unit: "bad"}
	decision, err := p.Evaluate(ctx, request)
	if err != nil || !decision.Permit || len(decision.Proof) == 0 || !strings.Contains(decision.PolicyID, ":sha256:") {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}
func TestPolicyDeniesUnprovenCommands(t *testing.T) {
	for _, mutation := range []string{"missing grant", "signature", "session", "expiry", "not before", "scope", "duration", "no baseline", "future baseline", "bad baseline", "slew", "unit", "mode", "actuator", "field", "range", "unknown", "null", "duplicate", "case", "missing value", "nonfinite", "lossy number", "underflow", "transition"} {
		t.Run(mutation, func(t *testing.T) {
			p, config, private, request, grant := fixture(t)
			switch mutation {
			case "session":
				grant.Session = teleop.SessionID{2}
			case "expiry":
				grant.ExpiresAt = request.EvaluatedAt
			case "not before":
				grant.NotBefore = request.EvaluatedAt.Add(time.Second)
			case "scope":
				grant.Scopes[0].Command = "safe"
			case "duration":
				grant.ExpiresAt = request.EvaluatedAt.Add(2 * time.Minute)
			case "no baseline":
				request.PreviousApplied = nil
			case "future baseline":
				request.PreviouslyAppliedAt = request.EvaluatedAt.Add(time.Second)
			case "bad baseline":
				request.PreviousApplied.Payload = json.RawMessage(`{}`)
			case "slew":
				request.Command = encode(t, "drive", "manual", 0.6)
			case "unit":
				request.Command.Payload = json.RawMessage(strings.ReplaceAll(string(request.Command.Payload), "fraction", "percent"))
			case "mode":
				request.Command = encode(t, "drive", "stop", 0.25)
			case "actuator":
				request.Command.Payload = json.RawMessage(strings.ReplaceAll(string(request.Command.Payload), "sim", "wrong"))
			case "field":
				request.Command.Payload = json.RawMessage(strings.ReplaceAll(string(request.Command.Payload), "power", "unknown"))
			case "range":
				request.Command = encode(t, "drive", "manual", 1.01)
			case "unknown":
				request.Command.Name = "unknown"
			case "null":
				request.Command.Payload = json.RawMessage(`{"actuator":"sim","mode":"manual","values":{"power":{"unit":"fraction","value":null}}}`)
			case "duplicate":
				request.Command.Payload = json.RawMessage(`{"actuator":"sim","mode":"manual","values":{"power":{"unit":"fraction","value":0,"v\u0061lue":0.25}}}`)
			case "case":
				request.Command.Payload = json.RawMessage(strings.ReplaceAll(string(request.Command.Payload), "value", "Value"))
			case "missing value":
				request.Command.Payload = json.RawMessage(`{"actuator":"sim","mode":"manual","values":{"power":{"unit":"fraction"}}}`)
			case "nonfinite":
				request.Command.Payload = json.RawMessage(strings.ReplaceAll(string(request.Command.Payload), "0.25", "1e999"))
			case "lossy number":
				request.Command.Payload = json.RawMessage(strings.ReplaceAll(string(request.Command.Payload), "0.25", "0.25000000000000001"))
			case "underflow":
				request.Command.Payload = json.RawMessage(strings.ReplaceAll(string(request.Command.Payload), "0.25", "1e-400"))
			case "transition":
				rule := config.Rules["drive"]
				rule.From = []string{"drive"}
				config.Rules["drive"] = rule
				var err error
				p, err = policy.New(config)
				if err != nil {
					t.Fatal(err)
				}
			}
			signed, err := policy.SignGrant(private, grant)
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "signature" {
				signed.Signature[0] ^= 1
			}
			var ctx context.Context = t.Context()
			if mutation != "missing grant" {
				ctx = policy.WithGrant(ctx, signed)
			}
			decision, err := p.Evaluate(ctx, request)
			if err == nil || decision.Permit {
				t.Fatalf("unsafe permission: %+v %v", decision, err)
			}
		})
	}
}
func TestPolicyRejectsInvalidConfiguration(t *testing.T) {
	for _, mutation := range []string{"empty", "key", "ttl", "range", "rate", "nonfinite", "transition", "baseline units"} {
		t.Run(mutation, func(t *testing.T) {
			_, config, _, _, _ := fixture(t)
			rule := config.Rules["drive"]
			limit := rule.Fields["power"]
			switch mutation {
			case "empty":
				config.ID = ""
			case "key":
				config.AuthorityKey = nil
			case "ttl":
				config.MaximumGrantTTL = 0
			case "range":
				limit.Minimum = 2
			case "rate":
				limit.MaximumRate = -1
			case "nonfinite":
				limit.Maximum = math.Inf(1)
			case "transition":
				rule.From = []string{"unknown"}
			case "baseline units":
				limit.Unit = "other"
			}
			rule.Fields["power"] = limit
			config.Rules["drive"] = rule
			if _, err := policy.New(config); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"actuator":"sim","mode":"manual","values":{"power":{"unit":"fraction","value":0}}}`))
	f.Add([]byte(`{"values":null}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		command, err := policy.Decode(raw)
		if err == nil {
			for _, scalar := range command.Values {
				if math.IsNaN(scalar.Value) || math.IsInf(scalar.Value, 0) {
					t.Fatal("nonfinite scalar")
				}
			}
		}
	})
}

func BenchmarkPolicyEvaluate(b *testing.B) {
	p, _, private, request, grant := fixture(b)
	signed, err := policy.SignGrant(private, grant)
	if err != nil {
		b.Fatal(err)
	}
	ctx := policy.WithGrant(b.Context(), signed)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if decision, err := p.Evaluate(ctx, request); err != nil || !decision.Permit {
			b.Fatalf("decision=%+v err=%v", decision, err)
		}
	}
}
