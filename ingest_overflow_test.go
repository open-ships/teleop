package teleop

import (
	"context"
	"errors"
	"testing"
	"time"
)

type burstInput struct{}

func (burstInput) Descriptor() Descriptor { return Descriptor{} }
func (burstInput) Close() error           { return nil }
func (burstInput) Read(ctx context.Context) (Observation, error) {
	return Observation{}, ctx.Err()
}

func TestLiveIngestOverflowStillFailsWithoutWaitingForConsumer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	controller := &Controller{
		source:    burstInput{},
		sourceCtx: ctx,
		clock:     systemClock{},
		ingest:    make(chan sourceResult, 2),
		fatal:     make(chan error, 1),
	}
	// Model a stalled processor: nobody drains ingest. The default live
	// producer must report loss instead of waiting indefinitely for space.
	done := make(chan struct{})
	go func() {
		controller.readLoop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("live ingest waited for queue space")
	}
	select {
	case err := <-controller.fatal:
		if !errors.Is(err, ErrPipelineOverflow) {
			t.Fatalf("overflow error: %v", err)
		}
	default:
		t.Fatal("live ingest failed to report overflow")
	}
}
