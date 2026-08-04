package teleop_test

import (
	"testing"

	"github.com/open-ships/teleop"
)

func TestEventIDStringIsStableAndUnambiguous(t *testing.T) {
	id := teleop.EventID{
		Session:  teleop.SessionID{0xab, 0xcd},
		Stream:   `authority/"quoted"`,
		Sequence: 42,
	}
	want := `abcd0000000000000000000000000000/"authority/\"quoted\""/42`
	if got := id.String(); got != want {
		t.Fatalf("EventID.String() = %q, want %q", got, want)
	}
}
