package assured_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/action"
	"github.com/open-ships/teleop/assured"
	"github.com/open-ships/teleop/gesture"
)

type lifetimeProvider struct{ directProvider }

func (p lifetimeProvider) Open(ctx context.Context, _ teleop.DeviceID, options ...teleop.OpenOption) (teleop.GameController, error) {
	return teleop.NewController(p.source, append([]teleop.OpenOption{teleop.WithContext(ctx)}, options...)...)
}

func TestAssuredComposesProcessorsThroughContextBindingProvider(t *testing.T) {
	store := &syncStore{}
	config, _ := validConfig(t, store, &witnessAnchor{}, &acceptingActuator{})
	config.Processors = []teleop.Processor{gesture.New(gesture.DefaultConfig()), action.New(action.OnButton("operator.press", teleop.ButtonBumperRight, teleop.PhasePressed))}
	source := exactSource(false)
	startup, cancelStartup := context.WithCancel(t.Context())
	session, err := assured.OpenProvider(startup, lifetimeProvider{directProvider{source: source}}, source.Descriptor().ID, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	// Expiring the startup request must not bypass ordered, evidenced shutdown.
	cancelStartup()
	sub, err := session.Subscribe(teleop.SubscriptionOptions{Delivery: teleop.DeliveryLossless})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	state := teleop.State{}
	state.SetButton(teleop.ButtonBumperRight, true)
	if err := source.Push(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for {
		event, err := sub.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if mapped, ok := event.(action.Event); ok && mapped.Action == "operator.press" {
			break
		}
	}
}
