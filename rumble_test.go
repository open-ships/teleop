package teleop_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/testkit"
)

func TestSetRumbleReplacesAndStopsVibration(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{
		Capability: teleop.Capabilities{Rumble: true},
	}, 1)
	controller, err := teleop.NewController(source)
	if err != nil {
		t.Fatal(err)
	}

	first := teleop.Rumble{LowFrequency: 0.8, HighFrequency: 0.25}
	if err := controller.SetRumble(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if got := source.Rumble(); got != first {
		t.Fatalf("fake rumble = %#v, want %#v", got, first)
	}

	second := teleop.Rumble{LowFrequency: 0.1, HighFrequency: 1}
	if err := controller.SetRumble(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if got := source.Rumble(); got != second {
		t.Fatalf("replacement rumble = %#v, want %#v", got, second)
	}

	if err := controller.SetRumble(t.Context(), teleop.Rumble{}); err != nil {
		t.Fatal(err)
	}
	if got := source.Rumble(); got != (teleop.Rumble{}) {
		t.Fatalf("stopped rumble = %#v, want zero", got)
	}

	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetRumble(t.Context(), first); !errors.Is(err, teleop.ErrClosed) {
		t.Fatalf("SetRumble after Close error = %v, want ErrClosed", err)
	}
}

func TestCloseStopsRumble(t *testing.T) {
	source := testkit.NewFakeSource(teleop.Descriptor{
		Capability: teleop.Capabilities{Rumble: true},
	}, 1)
	controller, err := teleop.NewController(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.SetRumble(t.Context(), teleop.Rumble{LowFrequency: 1}); err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if got := source.Rumble(); got != (teleop.Rumble{}) {
		t.Fatalf("rumble after Close = %#v, want zero", got)
	}
}

func TestSetRumbleRejectsUnsupportedInvalidAndCanceledRequests(t *testing.T) {
	unsupported := testkit.NewFakeSource(teleop.Descriptor{}, 1)
	controller, err := teleop.NewController(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	if err := controller.SetRumble(t.Context(), teleop.Rumble{LowFrequency: 1}); !errors.Is(err, teleop.ErrUnsupported) {
		t.Fatalf("unsupported SetRumble error = %v, want ErrUnsupported", err)
	}

	supported := testkit.NewFakeSource(teleop.Descriptor{
		Capability: teleop.Capabilities{Rumble: true},
	}, 1)
	rumbling, err := teleop.NewController(supported)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rumbling.Close() })
	for _, invalid := range []teleop.Rumble{
		{LowFrequency: -0.01},
		{LowFrequency: 1.01},
		{HighFrequency: float32(math.NaN())},
		{HighFrequency: float32(math.Inf(1))},
	} {
		if err := rumbling.SetRumble(t.Context(), invalid); !errors.Is(err, teleop.ErrInvalidState) {
			t.Errorf("SetRumble(%#v) error = %v, want ErrInvalidState", invalid, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rumbling.SetRumble(ctx, teleop.Rumble{LowFrequency: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled SetRumble error = %v, want context.Canceled", err)
	}
	var nilContext context.Context
	if err := rumbling.SetRumble(nilContext, teleop.Rumble{}); !errors.Is(err, teleop.ErrInvalidState) {
		t.Fatalf("nil-context SetRumble error = %v, want ErrInvalidState", err)
	}
}

func TestControllerDowngradesUnimplementedRumbleCapability(t *testing.T) {
	source := testkit.NewReplaySource(
		teleop.Descriptor{
			Capability: teleop.Capabilities{Rumble: true},
		},
		nil,
	)
	controller, err := teleop.NewController(source, teleop.WithDeferredStart())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })

	if controller.Capabilities().Rumble {
		t.Fatal("controller retained rumble capability without a RumbleSource")
	}
	if err := controller.SetRumble(t.Context(), teleop.Rumble{LowFrequency: 1}); !errors.Is(err, teleop.ErrUnsupported) {
		t.Fatalf("SetRumble error = %v, want ErrUnsupported", err)
	}
}

type panicRumbleSource struct {
	*testkit.FakeSource
}

func (panicRumbleSource) SetRumble(context.Context, teleop.Rumble) error {
	panic("backend rumble bug")
}

func TestSetRumbleContainsSourcePanic(t *testing.T) {
	source := panicRumbleSource{FakeSource: testkit.NewFakeSource(teleop.Descriptor{
		Capability: teleop.Capabilities{Rumble: true},
	}, 1)}
	controller, err := teleop.NewController(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })

	err = controller.SetRumble(t.Context(), teleop.Rumble{LowFrequency: 1})
	if !errors.Is(err, teleop.ErrCallbackPanic) {
		t.Fatalf("panic SetRumble error = %v, want ErrCallbackPanic", err)
	}
}
