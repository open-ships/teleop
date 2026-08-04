package assured_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/open-ships/teleop/assured"
	"github.com/open-ships/teleop/audit"
)

func TestAssuredReadinessWaitsForNewSameEventCountCheckpoint(t *testing.T) {
	store := &syncStore{}
	anchor := &sameCountGateAnchor{
		entered: make(chan audit.Checkpoint, 1),
		release: make(chan struct{}),
	}
	config, _ := validConfig(t, store, anchor, &acceptingActuator{})
	config.CheckpointEvery = 1
	config.WitnessTimeout = 2 * time.Second
	result := make(chan assuredOpenResult, 1)
	go func() {
		session, err := assured.OpenSource(t.Context(), exactSource(false), config)
		result <- assuredOpenResult{session: session, err: err}
	}()

	select {
	case <-anchor.entered:
	case <-time.After(time.Second):
		t.Fatal("readiness checkpoint did not reach witness")
	}
	select {
	case premature := <-result:
		if premature.session != nil {
			_ = premature.session.Close()
		}
		t.Fatalf("OpenSource returned before readiness checkpoint acknowledgement: %v", premature.err)
	case <-time.After(25 * time.Millisecond):
	}
	anchor.unblock()

	select {
	case opened := <-result:
		if opened.err != nil {
			t.Fatal(opened.err)
		}
		if err := opened.session.Close(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OpenSource did not finish after readiness witness acknowledgement")
	}
}

type assuredOpenResult struct {
	session *assured.Session
	err     error
}

type sameCountGateAnchor struct {
	mu             sync.Mutex
	lastCheckpoint *audit.Checkpoint
	blocked        bool
	entered        chan audit.Checkpoint
	release        chan struct{}
	releaseOnce    sync.Once
}

func (anchor *sameCountGateAnchor) Publish(
	ctx context.Context,
	checkpoint audit.Checkpoint,
) error {
	anchor.mu.Lock()
	block := !anchor.blocked &&
		checkpoint.RecordType == "checkpoint" &&
		anchor.lastCheckpoint != nil &&
		anchor.lastCheckpoint.RecordType == "checkpoint" &&
		anchor.lastCheckpoint.EventCount == checkpoint.EventCount
	if block {
		anchor.blocked = true
	}
	cloned := checkpoint
	anchor.lastCheckpoint = &cloned
	anchor.mu.Unlock()
	if !block {
		return nil
	}
	select {
	case anchor.entered <- checkpoint:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-anchor.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (anchor *sameCountGateAnchor) unblock() {
	anchor.releaseOnce.Do(func() { close(anchor.release) })
}
