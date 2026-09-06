package simulation_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/policy"
	"github.com/open-ships/teleop/safety"
	"github.com/open-ships/teleop/simulation"
)

func ExampleReceiver() {
	// Ephemeral TEST keys and invented simulator limits, never vessel defaults.
	issuerKey, issuer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	senderKey, sender, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	limits, err := policy.New(policy.Config{ID: "example-simulation", AuthorityKey: issuerKey, MaximumGrantTTL: time.Minute, Rules: map[string]policy.Rule{
		"safe":  {Actuator: "sim", Mode: "stop", Fields: map[string]policy.Limit{"power": {Unit: "fraction", Minimum: 0, Maximum: 0}}, From: []string{"safe", "drive"}},
		"drive": {Actuator: "sim", Mode: "manual", Fields: map[string]policy.Limit{"power": {Unit: "fraction", Minimum: -1, Maximum: 1}}, From: []string{"safe", "drive"}},
	}})
	if err != nil {
		panic(err)
	}
	encode := func(mode string, power float64) json.RawMessage {
		raw, err := json.Marshal(policy.Command{Actuator: "sim", Mode: mode, Values: map[string]policy.Scalar{"power": {Value: power, Unit: "fraction"}}})
		if err != nil {
			panic(err)
		}
		return raw
	}
	start := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	safe := safety.EncodedCommand{Name: "safe", Payload: encode("stop", 0)}
	r, err := simulation.NewReceiver(simulation.ReceiverConfig{Start: start, SenderKey: senderKey, Policy: limits, SafeState: safe, MaximumLease: time.Second})
	if err != nil {
		panic(err)
	}
	adapter, err := r.Actuator(sender)
	if err != nil {
		panic(err)
	}
	session := teleop.SessionID{1}
	grant, err := policy.SignGrant(issuer, policy.Grant{ID: "example-grant", Subject: "simulated-operator", Session: session, NotBefore: start, ExpiresAt: start.Add(time.Minute), Scopes: []policy.Scope{{Command: "drive", Actuator: "sim", Mode: "manual"}}})
	if err != nil {
		panic(err)
	}
	ctx := policy.WithGrant(context.Background(), grant)
	send := func(sequence uint64, command safety.EncodedCommand, fallback bool) {
		receipt, err := adapter.Send(ctx, safety.ActuatorCommand{ControllerSession: session, Sequence: sequence, Command: command, IssuedAt: r.Now(), ExpiresAt: r.Now().Add(time.Second), TTL: time.Second, Fallback: fallback, DecisionID: "simulation-decision", IntentID: "simulation-intent"})
		if err != nil {
			panic(err)
		}
		ack, err := receipt.Await(ctx)
		if err != nil || !ack.Accepted {
			panic("simulation command not acknowledged")
		}
	}
	// These IDs are simulation markers, not durable Assured evidence. The
	// Assured integration test demonstrates the complete evidenced Apply path.
	send(1, safe, true)
	send(2, safety.EncodedCommand{Name: "drive", Payload: encode("manual", 0.25)}, false)
	fmt.Println("before expiry:", r.Snapshot().Command.Name)
	if err := r.Advance(time.Second); err != nil {
		panic(err)
	}
	fmt.Println("without another sender packet:", r.Snapshot().Command.Name)
	// Output:
	// before expiry: drive
	// without another sender packet: safe
}
