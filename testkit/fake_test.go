package testkit_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/testkit"
)

func TestFakeSourcePushReadAndClose(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{}, 1)
	state := teleop.State{LeftTrigger: 0.5}
	if err := source.Push(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	observation, err := source.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.State.LeftTrigger != 0.5 ||
		observation.Native.Format != "testkit" ||
		observation.ObservedAt.IsZero() {
		t.Fatalf("observation = %#v", observation)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Read(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after Close error = %v, want EOF", err)
	}
	if err := source.Push(context.Background(), state); !errors.Is(err, teleop.ErrClosed) {
		t.Fatalf("Push after Close error = %v, want ErrClosed", err)
	}
}

func TestFakeSourceRumble(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{
		Capability: teleop.Capabilities{Rumble: true},
	}, 1)
	want := teleop.Rumble{LowFrequency: 0.75, HighFrequency: 0.4}
	if err := source.SetRumble(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	if got := source.Rumble(); got != want {
		t.Fatalf("Rumble = %#v, want %#v", got, want)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if got := source.Rumble(); got != (teleop.Rumble{}) {
		t.Fatalf("Rumble after Close = %#v, want zero", got)
	}
	if err := source.SetRumble(t.Context(), want); !errors.Is(err, teleop.ErrClosed) {
		t.Fatalf("SetRumble after Close error = %v, want ErrClosed", err)
	}
}

func TestFakeSourceTransportHealthIsExplicitAndIsolated(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{ID: "health"}, 1)
	if health := source.TransportHealth(); health.Sequence != 0 || health.SilenceVerifiable {
		t.Fatalf("default health = %+v, want unverifiable zero", health)
	}

	want := teleop.TransportHealth{
		Sequence:          7,
		CheckedAt:         time.Now(),
		Connected:         true,
		SilenceVerifiable: true,
	}
	source.SetTransportHealth(want)
	if got := source.TransportHealth(); got != want {
		t.Fatalf("health = %+v, want %+v", got, want)
	}
}

func TestReplaySourcePreservesOrderAndHonorsClose(t *testing.T) {
	observedAt := time.Unix(100, 0)
	source := testkit.NewReplaySource(
		teleop.Descriptor{ID: "replay:0"},
		[]teleop.Observation{
			{ObservedAt: observedAt, State: teleop.State{LeftTrigger: 0.25}},
			{ObservedAt: observedAt.Add(time.Second), State: teleop.State{LeftTrigger: 0.75}},
		},
	)
	for _, want := range []float32{0.25, 0.75} {
		observation, err := source.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if observation.State.LeftTrigger != want {
			t.Fatalf("trigger = %v, want %v", observation.State.LeftTrigger, want)
		}
	}
	if _, err := source.Read(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("exhausted replay error = %v, want EOF", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Read(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("closed replay error = %v, want EOF", err)
	}
}
