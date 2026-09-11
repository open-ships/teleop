package teleop_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/testkit"
)

type replayBarrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (barrier *replayBarrier) Process(teleop.Event) []teleop.Event {
	barrier.once.Do(func() { close(barrier.entered); <-barrier.release })
	return nil
}

func TestReplayBackpressurePreservesLongHistoryAndAudit(t *testing.T) {
	const count = 2000
	observations := make([]teleop.Observation, count)
	for i := range observations {
		observations[i] = teleop.Observation{ObservedAt: time.Unix(int64(i+1), 0), State: teleop.State{LeftTrigger: float32(i % 2)}}
	}
	observations[1000].Gap = &teleop.SourceGap{Dropped: 3, Reason: "recorded gap"}
	source := testkit.NewReplaySource(teleop.Descriptor{}, observations)
	barrier := &replayBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	var output bytes.Buffer
	recorder := audit.NewRecorder(&output)
	controller, err := teleop.NewController(source,
		teleop.WithDeferredStart(), teleop.WithReplayBackpressure(),
		teleop.WithPipelineBuffers(2, 8), teleop.WithAuditSink(recorder),
		teleop.WithSynchronousAudit(), teleop.WithProcessor(barrier))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controller.Close(); _ = recorder.Close() }()
	sub, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 8192})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("processor did not start")
	}
	close(barrier.release)
	seen, gaps := 0, 0
	for {
		event, err := sub.Next(ctx)
		if err != nil {
			if !errors.Is(err, teleop.ErrDisconnected) {
				t.Fatal(err)
			}
			break
		}
		switch value := event.(type) {
		case teleop.ObservationEvent:
			if value.Meta.Synthetic {
				continue
			}
			if seen >= count || value.Meta.ObservedAt != observations[seen].ObservedAt || value.Current.LeftTrigger != observations[seen].State.LeftTrigger {
				t.Fatalf("replay order at observation %d: %+v", seen, value)
			}
			seen++
		case teleop.GapEvent:
			gaps++
			if value.Dropped != 3 || value.Reason != "recorded gap" {
				t.Fatalf("unexpected gap: %+v", value)
			}
		}
	}
	if seen != count || gaps != 1 {
		t.Fatalf("replayed %d/%d observations, gaps=%d", seen, count, gaps)
	}
	if err := controller.Close(); !errors.Is(err, teleop.ErrDisconnected) {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := audit.ReadAll(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	recorded := 0
	for _, record := range records {
		if record.Kind == teleop.EventObservation && !record.Header.Synthetic {
			recorded++
		}
	}
	if recorded != count {
		t.Fatalf("audit retained %d/%d observations", recorded, count)
	}
}

// This source reaches its third Read only after two reads can fill the
// two-slot ingest queue and leave the producer waiting for queue space.
type replayReadProbe struct {
	*testkit.ReplaySource
	reads int
	third chan context.Context
}

func (source *replayReadProbe) Read(ctx context.Context) (teleop.Observation, error) {
	source.reads++
	if source.reads == 3 {
		source.third <- ctx
	}
	return source.ReplaySource.Read(ctx)
}

func TestReplayBackpressureWaitIsCanceledByCloseAndContext(t *testing.T) {
	for _, explicitClose := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "close"}[explicitClose], func(t *testing.T) {
			source := &replayReadProbe{ReplaySource: testkit.NewReplaySource(teleop.Descriptor{}, make([]teleop.Observation, 100)), third: make(chan context.Context, 1)}
			barrier := &replayBarrier{entered: make(chan struct{}), release: make(chan struct{})}
			lifetime, cancelLifetime := context.WithCancel(t.Context())
			defer cancelLifetime()
			controller, err := teleop.NewController(source, teleop.WithContext(lifetime), teleop.WithReplayBackpressure(), teleop.WithPipelineBuffers(2, 8), teleop.WithProcessor(barrier))
			if err != nil {
				t.Fatal(err)
			}
			defer controller.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			select {
			case <-barrier.entered:
			case <-ctx.Done():
				t.Fatal("processor did not start")
			}
			var readCtx context.Context
			select {
			case readCtx = <-source.third:
			case <-ctx.Done():
				t.Fatal("ingest did not fill")
			}
			closed := make(chan error, 1)
			if explicitClose {
				go func() { closed <- controller.Close() }()
			} else {
				cancelLifetime()
			}
			select {
			case <-readCtx.Done():
			case <-ctx.Done():
				t.Fatal("source cancellation was not propagated")
			}
			close(barrier.release)
			select {
			case <-controller.Done():
			case <-ctx.Done():
				t.Fatal("controller did not terminate")
			}
			if explicitClose {
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
			}
			if errors.Is(controller.Err(), teleop.ErrPipelineOverflow) {
				t.Fatalf("paused replay overflowed: %v", controller.Err())
			}
		})
	}
}
