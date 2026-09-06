package safety_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-ships/teleop/safety"
)

func policyAuthority(t *testing.T, policy safety.CommandPolicy, evidence safety.Evidence, actuator safety.Actuator) *safety.Authority {
	t.Helper()
	authority, err := safety.NewAuthority(safety.AuthorityConfig{EngineeredSafeState: safety.VesselCommand{Name: "safe"}, CommandTTL: 200 * time.Millisecond, RequireAppliedAcknowledgment: true, Policy: policy}, evidence, actuator, safety.WithCommandTimeout(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Bind(t.Context(), newAuthorityTestSource()); err != nil {
		t.Fatal(err)
	}
	if err := authority.Arm(t.Context(), "ready"); err != nil {
		t.Fatal(err)
	}
	return authority
}
func TestAuthorityPolicyDenialPanicAndTimeoutNeverSendLive(t *testing.T) {
	for _, fault := range []string{"deny", "expired", "missing identity", "panic", "timeout"} {
		t.Run(fault, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			policy := safety.CommandPolicyFunc(func(ctx context.Context, request safety.PolicyRequest) (safety.PolicyDecision, error) {
				value := safety.PolicyDecision{Permit: true, PolicyID: "test", Subject: "operator", Authorization: "grant", ExpiresAt: time.Now().Add(time.Hour)}
				switch fault {
				case "deny":
					value.Permit = false
				case "expired":
					value.ExpiresAt = time.Now()
				case "missing identity":
					value.Subject = ""
				case "panic":
					panic("test")
				case "timeout":
					<-release
				}
				return value, nil
			})
			evidence, actuator := &memoryEvidence{}, newFakeActuator()
			authority := policyAuthority(t, policy, evidence, actuator)
			result, err := authority.Apply(t.Context(), safety.ApplyRequest{Intent: safety.VesselCommand{Name: "live"}})
			if !errors.Is(err, safety.ErrCommandPolicy) || !result.Fallback || result.Acknowledgment == nil || !result.Acknowledgment.Accepted {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			for _, command := range actuator.snapshot() {
				if !command.Fallback {
					t.Fatal("policy fault sent live command")
				}
			}
			found := false
			for _, record := range evidence.snapshot() {
				if record.Policy != nil {
					found = true
					if record.Policy.Permit {
						t.Fatal("refused policy recorded as permitted")
					}
				}
			}
			if !found {
				t.Fatal("policy failure not recorded")
			}
			next, err := authority.Apply(t.Context(), safety.ApplyRequest{Intent: safety.VesselCommand{Name: "live"}})
			if err != nil || !next.Fallback {
				t.Fatalf("denial failed to inhibit: %+v %v", next, err)
			}
		})
	}
}
func TestAuthorityPolicyBindsExactCommandAndShortensLease(t *testing.T) {
	expires := time.Now().Add(150 * time.Millisecond)
	policy := safety.CommandPolicyFunc(func(ctx context.Context, request safety.PolicyRequest) (safety.PolicyDecision, error) {
		if request.Command.Name != "live" || string(request.Command.Payload) != `{"power":0.25}` || request.ControllerSession == ([16]byte{}) || request.PreviousApplied == nil {
			t.Error("policy missing exact request or baseline")
		}
		request.Command.Payload[0] = '!'
		return safety.PolicyDecision{Permit: true, PolicyID: "test", Subject: "operator", Authorization: "grant", ExpiresAt: expires}, nil
	})
	evidence, actuator := &memoryEvidence{}, newFakeActuator()
	authority := policyAuthority(t, policy, evidence, actuator)
	result, err := authority.Apply(t.Context(), safety.ApplyRequest{Intent: safety.VesselCommand{Name: "live", Payload: map[string]any{"power": 0.25}}})
	if err != nil || result.Policy == nil || !result.Policy.Permit {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	commands := actuator.snapshot()
	sent := commands[len(commands)-1]
	if sent.ExpiresAt.After(expires) || string(sent.Command.Payload) != `{"power":0.25}` {
		t.Fatalf("policy bypassed: %+v", sent)
	}
	for _, record := range evidence.snapshot() {
		if record.Policy != nil && (record.Requested == nil || string(record.Requested.Payload) != string(sent.Command.Payload)) {
			t.Fatal("policy evidence does not bind sent bytes")
		}
	}
}

func TestGrantExpiryDuringDecisionEvidenceNeverReachesLiveSend(t *testing.T) {
	policy := safety.CommandPolicyFunc(func(ctx context.Context, request safety.PolicyRequest) (safety.PolicyDecision, error) {
		return safety.PolicyDecision{Permit: true, PolicyID: "test", Subject: "operator", Authorization: "grant", ExpiresAt: time.Now().Add(40 * time.Millisecond)}, nil
	})
	base := &memoryEvidence{}
	evidence := safety.EvidenceFunc(func(ctx context.Context, record safety.EvidenceRecord) (safety.EvidenceID, error) {
		if record.Policy != nil {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return base.Commit(ctx, record)
	})
	actuator := newFakeActuator()
	authority := policyAuthority(t, policy, evidence, actuator)
	result, err := authority.Apply(t.Context(), safety.ApplyRequest{Intent: safety.VesselCommand{Name: "live"}})
	if !errors.Is(err, safety.ErrEvidence) || !result.Fallback || result.Acknowledgment == nil || !result.Acknowledgment.Accepted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, command := range actuator.snapshot() {
		if !command.Fallback {
			t.Fatal("expired grant reached live send")
		}
	}
}
