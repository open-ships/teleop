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
