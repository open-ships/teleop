package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/open-ships/teleop/audit"
)

func TestCheckpointAndWaitRejectsStaleSameEventCountReceipt(t *testing.T) {
	public, private, err := audit.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	anchor := &checkpointGateAnchor{}
	recorder := audit.NewRecorder(
		&syncBuffer{},
		audit.WithSigner(private),
		audit.WithAnchor(anchor),
		audit.WithCheckpoints(0, 0),
	)
	t.Cleanup(func() { _ = recorder.Close() })
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Checkpoint("older same-count head"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	older, err := recorder.WaitForWitness(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	gate := anchor.blockNext()
	t.Cleanup(gate.release)
	result := make(chan checkpointWaitResult, 1)
	go func() {
		receipt, waitErr := recorder.CheckpointAndWait(ctx, "new same-count head")
		result <- checkpointWaitResult{receipt: receipt, err: waitErr}
	}()
	var target audit.Checkpoint
	select {
	case target = <-gate.entered:
	case <-ctx.Done():
		t.Fatalf("new checkpoint did not reach witness: %v", ctx.Err())
	}
	if target.EventCount != older.Checkpoint.EventCount ||
		target.ChainHead == older.Checkpoint.ChainHead {
		t.Fatalf("test did not create distinct same-count heads: old=%+v new=%+v", older, target)
	}
	select {
	case premature := <-result:
		t.Fatalf("stale receipt satisfied exact checkpoint wait: %+v", premature)
	case <-time.After(25 * time.Millisecond):
	}
	gate.release()

	var got checkpointWaitResult
	select {
	case got = <-result:
	case <-ctx.Done():
		t.Fatalf("checkpoint wait did not finish: %v", ctx.Err())
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.receipt.Checkpoint.ChainHead != target.ChainHead {
		t.Fatalf("receipt = %+v, want exact target %+v", got.receipt, target)
	}
	if err := audit.VerifyCheckpoint(ed25519.PublicKey(public), got.receipt.Checkpoint); err != nil {
		t.Fatalf("verify witnessed checkpoint: %v", err)
	}
}

func TestCheckpointAndWaitAndEvidenceStatusReportQueueDrop(t *testing.T) {
	anchor := &checkpointGateAnchor{}
	recorder := audit.NewRecorder(
		&syncBuffer{},
		audit.WithAnchor(anchor),
		audit.WithCheckpoints(0, 0),
		audit.Option(func(options *audit.Options) { options.AnchorQueue = 1 }),
	)
	t.Cleanup(func() { _ = recorder.Close() })
	if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := recorder.WaitForWitness(ctx, 0); err != nil {
		t.Fatal(err)
	}

	blocker := anchor.blockNext()
	t.Cleanup(blocker.release)
	if err := recorder.Checkpoint("blocked predecessor"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocker.entered:
	case <-ctx.Done():
		t.Fatalf("predecessor checkpoint did not block witness: %v", ctx.Err())
	}

	result := make(chan checkpointWaitResult, 1)
	go func() {
		receipt, waitErr := recorder.CheckpointAndWait(ctx, "required head")
		result <- checkpointWaitResult{receipt: receipt, err: waitErr}
	}()
	// With the runner occupied and a queue depth of one, these two publications
	// necessarily evict one checkpoint after CheckpointAndWait's baseline.
	if err := recorder.Checkpoint("newer head"); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-result:
		if !errors.Is(got.err, audit.ErrWitnessRequired) {
			t.Fatalf("CheckpointAndWait error = %v, want ErrWitnessRequired", got.err)
		}
	case <-ctx.Done():
		t.Fatalf("checkpoint drop did not fail exact wait: %v", ctx.Err())
	}
	status := recorder.EvidenceStatus()
	if !errors.Is(status.Err, audit.ErrWitnessRequired) {
		t.Fatalf("EvidenceStatus did not report witness degradation: %+v", status)
	}
	if stats := recorder.AnchorStats(); stats.Dropped == 0 {
		t.Fatalf("anchor stats did not count queue drop: %+v", stats)
	}
	blocker.release()
}

type checkpointWaitResult struct {
	receipt audit.WitnessReceipt
	err     error
}

type checkpointAnchorGate struct {
	entered     chan audit.Checkpoint
	releaseCh   chan struct{}
	releaseOnce sync.Once
}

func (gate *checkpointAnchorGate) release() {
	gate.releaseOnce.Do(func() { close(gate.releaseCh) })
}

type checkpointGateAnchor struct {
	mu   sync.Mutex
	next *checkpointAnchorGate
}

func (anchor *checkpointGateAnchor) Publish(
	ctx context.Context,
	checkpoint audit.Checkpoint,
) error {
	anchor.mu.Lock()
	gate := anchor.next
	anchor.next = nil
	anchor.mu.Unlock()
	if gate == nil {
		return nil
	}
	select {
	case gate.entered <- checkpoint:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-gate.releaseCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (anchor *checkpointGateAnchor) blockNext() *checkpointAnchorGate {
	anchor.mu.Lock()
	defer anchor.mu.Unlock()
	if anchor.next != nil {
		panic("checkpoint anchor gate already armed")
	}
	gate := &checkpointAnchorGate{
		entered:   make(chan audit.Checkpoint, 1),
		releaseCh: make(chan struct{}),
	}
	anchor.next = gate
	return gate
}
