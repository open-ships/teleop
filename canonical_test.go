package teleop_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/open-ships/teleop"
)

type mutableEvent struct {
	Meta    teleop.Header `json:"header"`
	Payload []string      `json:"payload"`
}

func (e *mutableEvent) Header() teleop.Header { return e.Meta.Clone() }
func (*mutableEvent) Kind() teleop.EventKind  { return "test.mutable" }

func TestFreezeEventCapturesImmutableBytes(t *testing.T) {
	event := &mutableEvent{
		Meta: teleop.Header{ID: teleop.EventID{
			Session:  teleop.SessionID{1},
			Stream:   "test",
			Sequence: 1,
		}},
		Payload: []string{"original"},
	}
	frozen, err := teleop.FreezeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	event.Payload[0] = "mutated"
	event.Meta.ID.Sequence = 99

	if frozen.Header().ID.Sequence != 1 {
		t.Fatalf("frozen header sequence = %d, want 1", frozen.Header().ID.Sequence)
	}
	var decoded mutableEvent
	if err := json.Unmarshal(frozen.JSON(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Payload) != 1 || decoded.Payload[0] != "original" {
		t.Fatalf("frozen payload = %v, want original", decoded.Payload)
	}
}

type mismatchedHeaderEvent struct {
	Meta teleop.Header `json:"header"`
}

func (e mismatchedHeaderEvent) Header() teleop.Header {
	header := e.Meta.Clone()
	header.ID.Sequence++
	return header
}
func (mismatchedHeaderEvent) Kind() teleop.EventKind { return "test.mismatch" }

func TestFreezeEventRejectsHeaderMismatch(t *testing.T) {
	_, err := teleop.FreezeEvent(mismatchedHeaderEvent{Meta: teleop.Header{
		ID: teleop.EventID{Session: teleop.SessionID{1}, Stream: "test", Sequence: 1},
	}})
	if !errors.Is(err, teleop.ErrInvalidState) {
		t.Fatalf("FreezeEvent error = %v, want ErrInvalidState", err)
	}
}

type panickingEvent struct{}

func (panickingEvent) Header() teleop.Header  { panic("boom") }
func (panickingEvent) Kind() teleop.EventKind { return "test.panic" }

func TestFreezeEventContainsThirdPartyPanic(t *testing.T) {
	_, err := teleop.FreezeEvent(panickingEvent{})
	if !errors.Is(err, teleop.ErrCallbackPanic) {
		t.Fatalf("FreezeEvent error = %v, want ErrCallbackPanic", err)
	}
}
