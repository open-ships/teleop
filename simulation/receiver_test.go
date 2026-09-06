package simulation_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/policy"
	"github.com/open-ships/teleop/safety"
	"github.com/open-ships/teleop/simulation"
)

type fixture struct {
	r          *simulation.Receiver
	config     simulation.ReceiverConfig
	sender     ed25519.PrivateKey
	authorizer ed25519.PrivateKey
	grant      policy.SignedGrant
	session    teleop.SessionID
}

func command(t *testing.T, name, mode string, value float64) safety.EncodedCommand {
	t.Helper()
	raw, err := json.Marshal(policy.Command{Actuator: "sim", Mode: mode, Values: map[string]policy.Scalar{"power": {Value: value, Unit: "fraction"}}})
	if err != nil {
		t.Fatal(err)
	}
	return safety.EncodedCommand{Name: name, Payload: raw}
}
func setup(t *testing.T) fixture {
	t.Helper()
	senderPublic, sender, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authPublic, auth, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.New(policy.Config{ID: "simulation-test", AuthorityKey: authPublic, MaximumGrantTTL: time.Minute, Rules: map[string]policy.Rule{
		"safe":  {Actuator: "sim", Mode: "stop", Fields: map[string]policy.Limit{"power": {Unit: "fraction", Minimum: 0, Maximum: 0}}, From: []string{"safe", "drive"}},
		"drive": {Actuator: "sim", Mode: "manual", Fields: map[string]policy.Limit{"power": {Unit: "fraction", Minimum: -1, Maximum: 1, MaximumRate: 1}}, From: []string{"safe", "drive"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	config := simulation.ReceiverConfig{Start: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC), SenderKey: senderPublic, Policy: p, SafeState: command(t, "safe", "stop", 0), MaximumLease: time.Second, MaximumClockSkew: 10 * time.Millisecond}
	r, err := simulation.NewReceiver(config)
	if err != nil {
		t.Fatal(err)
	}
	session := teleop.SessionID{1}
	grant, err := policy.SignGrant(auth, policy.Grant{ID: "test-grant", Subject: "test-operator", Session: session, NotBefore: config.Start, ExpiresAt: config.Start.Add(30 * time.Second), Scopes: []policy.Scope{{Command: "drive", Actuator: "sim", Mode: "manual"}}})
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{r: r, config: config, sender: sender, authorizer: auth, grant: grant, session: session}
	if _, err := r.Receive(t.Context(), f.message(t, 1, true, 0)); err != nil {
		t.Fatal(err)
	}
	if err := r.Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f fixture) message(t *testing.T, sequence uint64, fallback bool, value float64) simulation.SignedCommand {
	t.Helper()
	selected := command(t, "drive", "manual", value)
	if fallback {
		selected = f.config.SafeState.Clone()
	}
	now := f.r.Now()
	envelope := safety.ActuatorCommand{ControllerSession: f.session, Sequence: sequence, Command: selected, IssuedAt: now, ExpiresAt: now.Add(time.Second), TTL: time.Second, Fallback: fallback, DecisionID: "decision", IntentID: "intent"}
	message, err := simulation.SignCommand(f.sender, f.r.Epoch(), envelope, &f.grant)
	if err != nil {
		t.Fatal(err)
	}
	return message
}
func resign(t *testing.T, f fixture, message simulation.SignedCommand) simulation.SignedCommand {
	t.Helper()
	signed, err := simulation.SignCommand(f.sender, message.Epoch, message.Envelope, message.Grant)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
func TestReceiverExpiresWithoutSenderAndDoesNotRestartLeaseOnReceipt(t *testing.T) {
	f := setup(t)
	message := f.message(t, 2, false, 0.5)
	if err := f.r.Advance(500 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ack, err := f.r.Receive(t.Context(), message)
	if err != nil || !ack.Accepted {
		t.Fatalf("ack=%+v err=%v", ack, err)
	}
	deadline := message.Envelope.ExpiresAt.Add(-f.config.MaximumClockSkew)
	if !f.r.Snapshot().ExpiresAt.Equal(deadline) {
		t.Fatal("receipt extended original deadline")
	}
	if err := f.r.Advance(490 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	state := f.r.Snapshot()
	if !state.Fallback || state.Command.Name != "safe" || !state.AppliedAt.Equal(deadline) {
		t.Fatalf("expiry failed: %+v", state)
	}
	if _, err := f.r.Receive(t.Context(), message); err == nil {
		t.Fatal("replay restarted expired command")
	}
}
func TestReceiverUntrustedOldAndForeignPacketsCannotReplaceNewerOutput(t *testing.T) {
	for _, fault := range []string{"signature", "tampered bytes", "epoch", "old", "duplicate", "foreign session", "expired", "future", "oversized lease", "ttl mismatch"} {
		t.Run(fault, func(t *testing.T) {
			f := setup(t)
			if _, err := f.r.Receive(t.Context(), f.message(t, 3, false, 0.25)); err != nil {
				t.Fatal(err)
			}
			before := f.r.Snapshot()
			message := f.message(t, 4, false, 0.25)
			switch fault {
			case "signature":
				message.Signature[0] ^= 1
			case "tampered bytes":
				message.Envelope.Command.Payload = append([]byte(" "), message.Envelope.Command.Payload...)
			case "epoch":
				message.Epoch[0] ^= 1
				message = resign(t, f, message)
			case "old":
				message.Envelope.Sequence = 2
				message = resign(t, f, message)
			case "duplicate":
				message.Envelope.Sequence = 3
				message = resign(t, f, message)
			case "foreign session":
				message.Envelope.ControllerSession = teleop.SessionID{2}
				message = resign(t, f, message)
			case "expired":
				message.Envelope.IssuedAt = before.Now.Add(-time.Second)
				message.Envelope.ExpiresAt = before.Now
				message = resign(t, f, message)
			case "future":
				message.Envelope.IssuedAt = before.Now.Add(time.Second)
				message.Envelope.ExpiresAt = before.Now.Add(2 * time.Second)
				message = resign(t, f, message)
			case "oversized lease":
				message.Envelope.TTL = 2 * time.Second
				message.Envelope.ExpiresAt = before.Now.Add(2 * time.Second)
				message = resign(t, f, message)
			case "ttl mismatch":
				message.Envelope.TTL = time.Millisecond
				message = resign(t, f, message)
			}
			if _, err := f.r.Receive(t.Context(), message); err == nil {
				t.Fatal("invalid packet accepted")
			}
			switch fault {
			case "expired", "future", "oversized lease", "ttl mismatch":
				state := f.r.Snapshot()
				if !state.Fallback || state.HighestSequence != 4 {
					t.Fatal("authenticated newer lease fault did not supersede older output")
				}
				if _, err := f.r.Receive(t.Context(), f.message(t, 3, false, 0.25)); err == nil {
					t.Fatal("older intent resurrected after newer rejected lease")
				}
			default:
				if !reflect.DeepEqual(before, f.r.Snapshot()) {
					t.Fatal("untrusted or old packet changed newer output")
				}
			}
		})
	}
}
func TestAuthenticatedNewerPolicyViolationsForceSafeAndConsumeSequence(t *testing.T) {
	for _, fault := range []string{"range", "rate", "grant", "grant expiry", "evidence", "false fallback"} {
		t.Run(fault, func(t *testing.T) {
			f := setup(t)
			if _, err := f.r.Receive(t.Context(), f.message(t, 2, false, 0.25)); err != nil {
				t.Fatal(err)
			}
			message := f.message(t, 3, false, 0.25)
			switch fault {
			case "range":
				message.Envelope.Command = command(t, "drive", "manual", 2)
			case "rate":
				message.Envelope.Command = command(t, "drive", "manual", 0.5)
			case "grant":
				message.Grant.Signature[0] ^= 1
			case "grant expiry":
				grant := message.Grant.Grant
				grant.ExpiresAt = f.r.Now().Add(500 * time.Millisecond)
				signed, err := policy.SignGrant(f.authorizer, grant)
				if err != nil {
					t.Fatal(err)
				}
				message.Grant = &signed
			case "evidence":
				message.Envelope.IntentID = ""
			case "false fallback":
				message.Envelope.Fallback = true
			}
			message = resign(t, f, message)
			if _, err := f.r.Receive(t.Context(), message); err == nil {
				t.Fatal("unsafe command accepted")
			}
			state := f.r.Snapshot()
			if !state.Fallback || state.HighestSequence != 3 {
				t.Fatalf("fault did not force safe: %+v", state)
			}
			if _, err := f.r.Receive(t.Context(), f.message(t, 2, false, 0)); err == nil {
				t.Fatal("older live packet resurrected")
			}
			if _, err := f.r.Receive(t.Context(), f.message(t, 4, true, 0)); err != nil {
				t.Fatal("higher safe command refused", err)
			}
		})
	}
}
func TestReceiverRestartRequiresFreshEpochAndSafeBootstrap(t *testing.T) {
	f := setup(t)
	old := f.message(t, 2, false, 0.25)
	restarted, err := simulation.NewReceiver(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Receive(t.Context(), old); err == nil {
		t.Fatal("old boot packet replayed")
	}
	f.r = restarted
	if _, err := restarted.Receive(t.Context(), f.message(t, 1, false, 0)); err == nil {
		t.Fatal("live session bootstrap accepted")
	}
	if _, err := restarted.Receive(t.Context(), f.message(t, 1, true, 0)); err != nil {
		t.Fatal(err)
	}
	before := restarted.Snapshot()
	if err := restarted.Advance(-time.Second); err == nil || !reflect.DeepEqual(before, restarted.Snapshot()) {
		t.Fatal("simulation clock moved backward")
	}
}
func TestReferenceAdapterCarriesGrantAndReportsOnlySimulation(t *testing.T) {
	f := setup(t)
	adapter, err := f.r.Actuator(f.sender)
	if err != nil {
		t.Fatal(err)
	}
	ctx := policy.WithGrant(t.Context(), f.grant)
	receipt, err := adapter.Send(ctx, f.message(t, 2, false, 0.25).Envelope)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := receipt.Await(ctx)
	if err != nil || !ack.Accepted || ack.AppliedAt.IsZero() {
		t.Fatalf("ack=%+v err=%v", ack, err)
	}
}
