package teleop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/testkit"
)

type attestedSink struct{}

func (*attestedSink) Record(context.Context, teleop.Event) error { return nil }

type attestedProcessor struct{}

func (*attestedProcessor) Process(teleop.Event) []teleop.Event { return nil }

type valueAttestedSink struct{}

func (valueAttestedSink) Record(context.Context, teleop.Event) error { return nil }

type attestationTestClock struct{}

func (attestationTestClock) Now() time.Time { return time.Now() }

func (attestationTestClock) NewTicker(interval time.Duration) teleop.Ticker {
	return attestationTestTicker{Ticker: time.NewTicker(interval)}
}

type attestationTestTicker struct{ *time.Ticker }

func (ticker attestationTestTicker) C() <-chan time.Time { return ticker.Ticker.C }

func TestControllerAttestsExactSealedPipeline(t *testing.T) {
	sink := &attestedSink{}
	processor := &attestedProcessor{}
	attestation, err := teleop.NewPipelineAttestation(teleop.PipelineRequirements{
		AuditSinks:       []teleop.EventSink{sink},
		Processors:       []teleop.Processor{processor},
		SynchronousAudit: true,
		LivenessInterval: 20 * time.Millisecond,
		ShutdownTimeout:  250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	source := testkit.NewFakeSource(teleop.Descriptor{
		ID:   "pipeline-attestation:0",
		Name: "Pipeline attestation test",
	}, 1)
	controller, err := teleop.NewController(
		source,
		teleop.WithAuditSink(sink),
		teleop.WithSynchronousAudit(),
		teleop.WithProcessor(processor),
		teleop.WithLiveness(20*time.Millisecond, 0),
		teleop.WithShutdownTimeout(250*time.Millisecond),
		attestation.Option(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if err := controller.AttestPipeline(attestation); err != nil {
		t.Fatalf("AttestPipeline: %v", err)
	}
}

func TestControllerAttestationDetectsPostSealOverride(t *testing.T) {
	for _, override := range []teleop.OpenOption{teleop.WithLiveness(0, 0), teleop.WithReplayBackpressure()} {
		sink := &attestedSink{}
		processor := &attestedProcessor{}
		attestation, err := teleop.NewPipelineAttestation(teleop.PipelineRequirements{
			AuditSinks:       []teleop.EventSink{sink},
			Processors:       []teleop.Processor{processor},
			SynchronousAudit: true,
			LivenessInterval: 20 * time.Millisecond,
			ShutdownTimeout:  250 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		source := testkit.NewFakeSource(teleop.Descriptor{ID: "pipeline-attestation:1"}, 1)
		controller, err := teleop.NewController(
			source,
			teleop.WithAuditSink(sink),
			teleop.WithSynchronousAudit(),
			teleop.WithProcessor(processor),
			teleop.WithLiveness(20*time.Millisecond, 0),
			teleop.WithShutdownTimeout(250*time.Millisecond),
			attestation.Option(),
			override,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer controller.Close()
		if err := controller.AttestPipeline(attestation); !errors.Is(
			err,
			teleop.ErrPipelineAttestation,
		) {
			t.Fatalf("AttestPipeline error = %v, want override rejection", err)
		}
	}
}

func TestControllerAttestationRejectsUnsafeEffectiveOptions(t *testing.T) {
	contextKey := struct{}{}
	for _, test := range []struct {
		name   string
		option teleop.OpenOption
	}{
		{name: "custom clock", option: teleop.WithClock(attestationTestClock{})},
		{
			name: "custom context",
			option: teleop.WithContext(context.WithValue(
				context.Background(),
				contextKey,
				"provider-controlled",
			)),
		},
		{name: "pipeline buffers", option: teleop.WithPipelineBuffers(4, 4)},
		{name: "callback timeout", option: teleop.WithCallbackTimeout(time.Millisecond)},
		{name: "deferred start", option: teleop.WithDeferredStart()},
		{name: "replay backpressure", option: teleop.WithReplayBackpressure()},
		{name: "stale policy", option: teleop.WithLiveness(20*time.Millisecond, time.Second)},
		{name: "stale neutralization", option: teleop.WithNeutralizeOnStale(true)},
		{name: "clock step threshold", option: teleop.WithClockStepThreshold(time.Millisecond)},
	} {
		t.Run(test.name, func(t *testing.T) {
			sink := &attestedSink{}
			processor := &attestedProcessor{}
			attestation, err := teleop.NewPipelineAttestation(teleop.PipelineRequirements{
				AuditSinks:       []teleop.EventSink{sink},
				Processors:       []teleop.Processor{processor},
				SynchronousAudit: true,
				LivenessInterval: 20 * time.Millisecond,
				ShutdownTimeout:  250 * time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			source := testkit.NewFakeSource(teleop.Descriptor{
				ID: teleop.DeviceID("unsafe-pipeline:" + test.name),
			}, 1)
			controller, err := teleop.NewController(
				source,
				teleop.WithAuditSink(sink),
				teleop.WithSynchronousAudit(),
				teleop.WithProcessor(processor),
				teleop.WithLiveness(20*time.Millisecond, 0),
				teleop.WithShutdownTimeout(250*time.Millisecond),
				test.option,
				attestation.Option(),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer controller.Close()
			if err := controller.AttestPipeline(attestation); !errors.Is(
				err,
				teleop.ErrPipelineAttestation,
			) {
				t.Fatalf("AttestPipeline error = %v, want unsafe-option rejection", err)
			}
		})
	}
}

func TestPipelineAttestationRequiresPointerBackedCallbacks(t *testing.T) {
	_, err := teleop.NewPipelineAttestation(teleop.PipelineRequirements{
		AuditSinks:      []teleop.EventSink{valueAttestedSink{}},
		ShutdownTimeout: time.Second,
	})
	if !errors.Is(err, teleop.ErrPipelineAttestation) {
		t.Fatalf("NewPipelineAttestation error = %v, want identity rejection", err)
	}
}
