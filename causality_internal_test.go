package teleop

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/open-ships/teleop/internal/eventorder"
)

func TestControllerPublishedCausalityScalesWithStreams(t *testing.T) {
	const events = 100_000
	session := SessionID{1}
	controller := &Controller{session: session}
	for sequence := uint64(1); sequence <= events; sequence++ {
		if err := controller.markPublished(EventID{
			Session:  session,
			Stream:   "input",
			Sequence: sequence,
		}); err != nil {
			t.Fatalf("mark event %d published: %v", sequence, err)
		}
	}
	for sequence := uint64(1); sequence <= 3; sequence++ {
		if err := controller.markPublished(EventID{
			Session:  session,
			Stream:   "derived",
			Sequence: sequence,
		}); err != nil {
			t.Fatalf("mark derived event %d published: %v", sequence, err)
		}
	}

	if got := controller.published.StreamCount(); got != 2 {
		t.Fatalf("retained streams = %d, want 2 after %d events", got, events+3)
	}
	if err := controller.validateCommandCauses([]EventID{
		{Session: session, Stream: "input", Sequence: 1},
		{Session: session, Stream: "input", Sequence: events},
		{Session: session, Stream: "derived", Sequence: 2},
	}); err != nil {
		t.Fatalf("published causes rejected: %v", err)
	}
}

func TestControllerCommandCausesRequireSameSessionAndPublishedPrefix(t *testing.T) {
	session := SessionID{1}
	controller := &Controller{session: session}
	if err := controller.markPublished(EventID{
		Session: session, Stream: "input", Sequence: 1,
	}); err != nil {
		t.Fatal(err)
	}

	tests := map[string]EventID{
		"future sequence": {Session: session, Stream: "input", Sequence: 2},
		"missing stream":  {Session: session, Stream: "missing", Sequence: 1},
		"other session":   {Session: SessionID{2}, Stream: "input", Sequence: 1},
		"zero sequence":   {Session: session, Stream: "input"},
	}
	for name, cause := range tests {
		t.Run(name, func(t *testing.T) {
			if err := controller.validateCommandCauses([]EventID{cause}); !errors.Is(
				err,
				ErrInvalidState,
			) {
				t.Fatalf("validation error = %v, want ErrInvalidState", err)
			}
		})
	}
}

func TestControllerRejectsNoncontiguousPublishedIdentity(t *testing.T) {
	session := SessionID{1}
	controller := &Controller{session: session}
	if err := controller.markPublished(EventID{
		Session: session, Stream: "input", Sequence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]EventID{
		"gap":           {Session: session, Stream: "input", Sequence: 3},
		"duplicate":     {Session: session, Stream: "input", Sequence: 1},
		"other session": {Session: SessionID{2}, Stream: "input", Sequence: 2},
	} {
		t.Run(name, func(t *testing.T) {
			if err := controller.markPublished(id); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("mark error = %v, want ErrInvalidState", err)
			}
		})
	}
}

func TestControllerBoundsStreamPerEventIdentityState(t *testing.T) {
	session := SessionID{1}
	base := time.Now()
	controller := &Controller{
		session:   session,
		clock:     systemClock{},
		clocks:    newClockMonitor(base, 0),
		allocated: eventorder.New(MaxEventStreamsPerSession),
		published: eventorder.New(MaxEventStreamsPerSession),
	}
	for index := 0; index < MaxEventStreamsPerSession; index++ {
		header := controller.NewHeader(
			fmt.Sprintf("application-%d", index),
			base,
			0,
		)
		if header.ID.Sequence != 1 {
			t.Fatalf("stream %d sequence = %d, want 1", index, header.ID.Sequence)
		}
		if err := controller.markPublished(header.ID); err != nil {
			t.Fatalf("publish stream %d: %v", index, err)
		}
	}
	overflow := controller.NewHeader("one-stream-too-many", base, 0)
	if overflow.ID.Sequence != 0 {
		t.Fatalf("overflow sequence = %d, want invalid zero", overflow.ID.Sequence)
	}
	if err := controller.markPublished(overflow.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("overflow publication error = %v, want ErrInvalidState", err)
	}
	if got := controller.allocated.StreamCount(); got != MaxEventStreamsPerSession {
		t.Fatalf("allocated streams = %d, want cap %d", got, MaxEventStreamsPerSession)
	}
	if got := controller.published.StreamCount(); got != MaxEventStreamsPerSession {
		t.Fatalf("published streams = %d, want cap %d", got, MaxEventStreamsPerSession)
	}
}

func TestSubscriptionGapDoesNotSplitReservedInputPrefix(t *testing.T) {
	session := SessionID{1}
	base := time.Now()
	controller := &Controller{
		session:   session,
		clock:     systemClock{},
		clocks:    newClockMonitor(base, 0),
		allocated: eventorder.New(MaxEventStreamsPerSession),
		published: eventorder.New(MaxEventStreamsPerSession),
	}
	first := controller.NewHeader("input", base, 0)
	second := controller.NewHeader("input", base, 0)
	if err := controller.markPublished(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := controller.recordSubscriptionGap(
		ButtonEvent{Meta: first},
		1,
		"test coalescing",
	); err != nil {
		t.Fatalf("record subscription gap: %v", err)
	}
	if err := controller.markPublished(second.ID); err != nil {
		t.Fatalf("publish reserved second input identity: %v", err)
	}
	if got := controller.published.Through("input"); got != 2 {
		t.Fatalf("input published through %d, want 2", got)
	}
	if got := controller.published.Through("delivery"); got != 1 {
		t.Fatalf("delivery published through %d, want 1", got)
	}
}
