package assured

import (
	"fmt"
	"testing"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/safety"
)

func TestEvidenceBridgeReleasesCompletedChainIdentities(t *testing.T) {
	t.Parallel()

	bridge := &evidenceBridge{identities: make(map[safety.EvidenceID]teleop.EventID)}
	const chains = 10_000
	for index := range chains {
		prefix := fmt.Sprintf("chain-%d", index)
		decision := safety.EvidenceID(prefix + "-decision")
		intent := safety.EvidenceID(prefix + "-intent")
		sent := safety.EvidenceID(prefix + "-sent")
		outcome := safety.EvidenceID(prefix + "-outcome")
		eventID := teleop.EventID{Sequence: uint64(index + 1)}

		bridge.advanceIdentity(safety.EvidenceDecision, "", decision, eventID)
		bridge.advanceIdentity(safety.EvidenceIntent, decision, intent, eventID)
		bridge.advanceIdentity(safety.EvidenceSent, intent, sent, eventID)
		bridge.advanceIdentity(safety.EvidenceAcknowledged, sent, outcome, eventID)

		attempt := safety.EvidenceID(prefix + "-lifecycle-attempt")
		lifecycleOutcome := safety.EvidenceID(prefix + "-lifecycle-outcome")
		bridge.advanceIdentity(safety.EvidenceLifecycleAttempt, "", attempt, eventID)
		bridge.advanceIdentity(
			safety.EvidenceLifecycleOutcome,
			attempt,
			lifecycleOutcome,
			eventID,
		)
	}
	if retained := len(bridge.identities); retained != 0 {
		t.Fatalf("completed evidence chains retained %d identities", retained)
	}
}

func TestEvidenceBridgeRetainsOnlyFutureParents(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		kind safety.EvidenceKind
		want bool
	}{
		{kind: safety.EvidenceLifecycleAttempt, want: true},
		{kind: safety.EvidenceDecision, want: true},
		{kind: safety.EvidenceIntent, want: true},
		{kind: safety.EvidenceSent, want: true},
		{kind: safety.EvidenceLifecycleOutcome},
		{kind: safety.EvidenceAcknowledged},
		{kind: safety.EvidenceRejected},
		{kind: safety.EvidenceTimeout},
		{kind: safety.EvidenceFailure},
	} {
		if got := evidenceCanBeParent(test.kind); got != test.want {
			t.Errorf("evidenceCanBeParent(%q) = %t, want %t", test.kind, got, test.want)
		}
	}
}
