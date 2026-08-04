package audit_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
)

func TestQuorumAnchorRequiresConfiguredIndependentSuccesses(t *testing.T) {
	var successes atomic.Int64
	good := func(context.Context, audit.Checkpoint) error {
		successes.Add(1)
		return nil
	}
	bad := errors.New("witness unavailable")
	anchor, err := audit.NewQuorumAnchor(
		2,
		audit.AnchorFunc(good),
		audit.AnchorFunc(func(context.Context, audit.Checkpoint) error { return bad }),
		audit.AnchorFunc(good),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.Publish(t.Context(), audit.Checkpoint{}); err != nil {
		t.Fatal(err)
	}
	if successes.Load() != 2 {
		t.Fatalf("successful publications = %d, want 2", successes.Load())
	}
}

func TestQuorumAnchorReportsImpossibleQuorum(t *testing.T) {
	failure := errors.New("offline")
	anchor, err := audit.NewQuorumAnchor(
		2,
		audit.AnchorFunc(func(context.Context, audit.Checkpoint) error { return failure }),
		audit.AnchorFunc(func(context.Context, audit.Checkpoint) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.Publish(t.Context(), audit.Checkpoint{}); !errors.Is(err, failure) {
		t.Fatalf("Publish error = %v, want wrapped failure", err)
	}
}

func TestQuorumAnchorConvertsAdapterPanicToFailure(t *testing.T) {
	anchor, err := audit.NewQuorumAnchor(
		2,
		audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
			panic("independent witness fault")
		}),
		audit.AnchorFunc(func(context.Context, audit.Checkpoint) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.Publish(t.Context(), audit.Checkpoint{}); !errors.Is(
		err,
		teleop.ErrCallbackPanic,
	) {
		t.Fatalf("Publish error = %v, want ErrCallbackPanic", err)
	}
}

func TestQuorumAnchorBoundsContextIgnoringMinorityToOneWorker(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	finished := make(chan struct{})
	var calls atomic.Int64
	blocking := audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // Deliberately ignore context cancellation.
		close(finished)
		return nil
	})
	anchor, err := audit.NewQuorumAnchor(
		1,
		audit.AnchorFunc(func(context.Context, audit.Checkpoint) error { return nil }),
		blocking,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.Publish(t.Context(), audit.Checkpoint{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("context-ignoring minority did not start")
	}

	for index := range 64 {
		if err := anchor.Publish(t.Context(), audit.Checkpoint{EventCount: uint64(index + 1)}); err != nil {
			t.Fatalf("healthy quorum publish %d: %v", index, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("context-ignoring minority invoked %d times, want one bounded worker", got)
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("released minority worker did not finish")
	}
}

func TestQuorumAnchorValidatesConfiguration(t *testing.T) {
	if _, err := audit.NewQuorumAnchor(0); err == nil {
		t.Fatal("zero quorum unexpectedly accepted")
	}
	if _, err := audit.NewQuorumAnchor(2, audit.AnchorFunc(func(context.Context, audit.Checkpoint) error {
		return nil
	})); err == nil {
		t.Fatal("impossible quorum unexpectedly accepted")
	}
}

func TestQuorumAnchorRejectsNilCallInputs(t *testing.T) {
	anchor, err := audit.NewQuorumAnchor(
		1,
		audit.AnchorFunc(func(context.Context, audit.Checkpoint) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.Publish(nil, audit.Checkpoint{}); err == nil {
		t.Fatal("nil context unexpectedly accepted")
	}
	var nilAnchor *audit.QuorumAnchor
	if err := nilAnchor.Publish(t.Context(), audit.Checkpoint{}); err == nil {
		t.Fatal("nil receiver unexpectedly accepted")
	}
}
