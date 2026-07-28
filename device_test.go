package teleop

import "testing"

func TestCapabilitiesDefaultUnknownAuditGradeToUnavailable(t *testing.T) {
	capability := (Capabilities{}).Clone()
	if capability.AuditGrade != AuditUnavailable {
		t.Fatalf("zero audit grade = %q, want %q", capability.AuditGrade, AuditUnavailable)
	}
	capability = (Capabilities{AuditGrade: "future-grade"}).Clone()
	if capability.AuditGrade != AuditUnavailable {
		t.Fatalf("unknown audit grade = %q, want %q", capability.AuditGrade, AuditUnavailable)
	}
	if !AuditExactBackendStream.Valid() || !AuditSampledState.Valid() ||
		!AuditUnavailable.Valid() {
		t.Fatal("known audit grades are invalid")
	}
}
