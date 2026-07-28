package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/xbox"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() (err error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	provider := xbox.NewProvider()
	devices, err := provider.Discover(ctx)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		return errors.New("connect an Xbox controller first")
	}

	controller, err := provider.Open(ctx, devices[0].ID)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := controller.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close controller: %w", closeErr))
		}
	}()

	events, err := controller.Subscribe(teleop.SubscriptionOptions{
		Delivery: teleop.DeliveryLossless,
	})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := events.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close event subscription: %w", closeErr))
		}
	}()

	for {
		event, err := events.Next(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		switch event := event.(type) {
		case teleop.ButtonEvent:
			fmt.Printf("%s %s\n", event.Button, event.Phase)
		case teleop.StickEvent:
			fmt.Printf("%s stick: %+0.3f, %+0.3f\n", event.Stick, event.Position.X, event.Position.Y)
		}
	}
}
