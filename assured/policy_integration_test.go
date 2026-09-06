package assured_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/action"
	"github.com/open-ships/teleop/assured"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/policy"
	"github.com/open-ships/teleop/safety"
	"github.com/open-ships/teleop/simulation"
)

func TestAssuredLivePolicyEvidenceAndIndependentReceiver(t *testing.T) {
	for _, fault := range []string{"none", "wrong units", "bad grant", "missing grant", "superseded input"} {
		t.Run(fault, func(t *testing.T) {
			issuerKey, issuer, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			senderKey, sender, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			p, err := policy.New(policy.Config{ID: "assured-simulation-only", AuthorityKey: issuerKey, MaximumGrantTTL: time.Minute, Rules: map[string]policy.Rule{
				"safe":  {Actuator: "sim", Mode: "stop", Fields: map[string]policy.Limit{"power": {Unit: "fraction", Minimum: 0, Maximum: 0}}, From: []string{"safe", "drive"}},
				"drive": {Actuator: "sim", Mode: "manual", Fields: map[string]policy.Limit{"power": {Unit: "fraction", Minimum: -1, Maximum: 1}}, From: []string{"safe", "drive"}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			safe := policy.Command{Actuator: "sim", Mode: "stop", Values: map[string]policy.Scalar{"power": {Value: 0, Unit: "fraction"}}}
			safeBytes, err := json.Marshal(safe)
			if err != nil {
				t.Fatal(err)
			}
			receiver, err := simulation.NewReceiver(simulation.ReceiverConfig{Start: time.Now(), SenderKey: senderKey, Policy: p, SafeState: safety.EncodedCommand{Name: "safe", Payload: safeBytes}, MaximumLease: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			modelAdapter, err := receiver.Actuator(sender)
			if err != nil {
				t.Fatal(err)
			}
			// Explicit test-only clock driver: this does not install a production
			// watchdog or weaken Assured's sealed system-clock requirements.
			adapter := safety.ActuatorFunc(func(ctx context.Context, command safety.ActuatorCommand) (safety.ActuatorReceipt, error) {
				if elapsed := command.IssuedAt.Sub(receiver.Now()); elapsed > 0 {
					if err := receiver.Advance(elapsed); err != nil {
						return nil, err
					}
				}
				return modelAdapter.Send(ctx, command)
			})
			store, anchor := &syncStore{}, &witnessAnchor{}
			config, public := validConfig(t, store, anchor, adapter)
			config.Safety, config.Maritime = config.Maritime, safety.MaritimeConfig{}
			config.Authority.Policy = p
			entered, release := make(chan struct{}), make(chan struct{})
			var blocked atomic.Bool
			if fault == "superseded input" {
				config.Authority.Policy = safety.CommandPolicyFunc(func(ctx context.Context, request safety.PolicyRequest) (safety.PolicyDecision, error) {
					decision, err := p.Evaluate(ctx, request)
					if blocked.CompareAndSwap(false, true) {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return decision, ctx.Err()
						}
					}
					return decision, err
				})
			}
			config.Authority.EngineeredSafeState = safety.Command{Name: "safe", Payload: safe}
			config.Provenance.Config["command_policy"] = p.Configuration()
			config.Processors = []teleop.Processor{action.New(action.OnButton("ready", teleop.ButtonBumperRight, teleop.PhasePressed))}
			source := exactSource(false)
			if err := source.Push(t.Context(), teleop.State{}); err != nil {
				t.Fatal(err)
			}
			session, err := assured.OpenSource(t.Context(), source, config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := session.Arm(t.Context(), "test operator ready"); err != nil {
				t.Fatal(err)
			}
			sub, err := session.Subscribe(teleop.SubscriptionOptions{Delivery: teleop.DeliveryLossless})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			state := teleop.State{}
			state.SetButton(teleop.ButtonBumperRight, true)
			if err := source.Push(t.Context(), state); err != nil {
				t.Fatal(err)
			}
			wait, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			for {
				event, err := sub.Next(wait)
				if err != nil {
					t.Fatal(err)
				}
				if event, ok := event.(action.Event); ok && event.Action == "ready" {
					break
				}
			}
			now := time.Now()
			grant, err := policy.SignGrant(issuer, policy.Grant{ID: "live-test", Subject: "test-operator", Session: session.ID(), NotBefore: now.Add(-time.Second), ExpiresAt: now.Add(10 * time.Second), Scopes: []policy.Scope{{Command: "drive", Actuator: "sim", Mode: "manual"}}})
			if err != nil {
				t.Fatal(err)
			}
			if fault == "bad grant" {
				grant.Signature[0] ^= 1
			}
			ctx := t.Context()
			if fault != "missing grant" {
				ctx = policy.WithGrant(ctx, grant)
			}
			unit := "fraction"
			if fault == "wrong units" {
				unit = "percent"
			}
			request := safety.ApplyRequest{Intent: safety.Command{Name: "drive", Payload: policy.Command{Actuator: "sim", Mode: "manual", Values: map[string]policy.Scalar{"power": {Value: 0.25, Unit: unit}}}}}
			var result safety.ApplyResult
			if fault == "superseded input" {
				type outcome struct {
					result safety.ApplyResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() { result, err := session.Apply(ctx, request); done <- outcome{result, err} }()
				select {
				case <-entered:
				case <-wait.Done():
					t.Fatal("policy evaluation did not start")
				}
				if err := source.Push(t.Context(), state); err != nil {
					t.Fatal(err)
				}
				for {
					event, err := sub.Next(wait)
					if err != nil {
						t.Fatal(err)
					}
					if event.Kind() == teleop.EventObservation {
						break
					}
				}
				close(release)
				select {
				case value := <-done:
					result, err = value.result, value.err
				case <-wait.Done():
					t.Fatal("superseded Apply did not finish")
				}
				if !errors.Is(err, safety.ErrIntentSuperseded) || !result.FallbackAcknowledged || !receiver.Snapshot().Fallback {
					t.Fatalf("supersession result=%+v err=%v", result, err)
				}
				// Recompute current intent without re-arming; both exact input
				// causality and standing authority must survive the safe fallback.
				result, err = session.Apply(ctx, request)
			} else {
				result, err = session.Apply(ctx, request)
			}
			permitted := fault == "none" || fault == "superseded input"
			if permitted {
				if err != nil || result.Fallback || result.Policy == nil || !result.Policy.Permit || receiver.Snapshot().Fallback {
					t.Fatalf("live integration result=%+v err=%v", result, err)
				}
			} else if !errors.Is(err, safety.ErrCommandPolicy) || !result.Fallback || !receiver.Snapshot().Fallback {
				t.Fatalf("denied integration result=%+v err=%v", result, err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			records, _, err := audit.Read(bytes.NewReader(store.bytes()), audit.VerifyOptions{PublicKey: public, RequireFooter: true, RequireWitness: true, Witnesses: anchor.snapshot()})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, record := range records {
				if record.Kind != teleop.EventCommand {
					continue
				}
				event, err := audit.DecodeEvent(record)
				if err != nil {
					t.Fatal(err)
				}
				command := event.(*teleop.CommandEvent)
				var evidence safety.EvidenceRecord
				if err := json.Unmarshal(command.Payload, &evidence); err != nil {
					t.Fatal(err)
				}
				if evidence.Policy != nil {
					found = true
					if evidence.Requested == nil || evidence.Policy.Permit != permitted {
						t.Fatal("policy evidence contradicts outcome")
					}
				}
			}
			if !found {
				t.Fatal("policy decision missing from trusted witnessed evidence")
			}
		})
	}
}
