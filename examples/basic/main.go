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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	provider := xbox.NewProvider()
	devices, err := provider.Discover(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if len(devices) == 0 {
		log.Fatal("connect an Xbox controller first")
	}

	controller, err := provider.Open(ctx, devices[0].ID)
	if err != nil {
		log.Fatal(err)
	}
	defer controller.Close()

	events, err := controller.Subscribe(teleop.SubscriptionOptions{
		Delivery: teleop.DeliveryLossless,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer events.Close()

	for {
		event, err := events.Next(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Fatal(err)
		}
		switch event := event.(type) {
		case teleop.ButtonEvent:
			fmt.Printf("%s %s\n", event.Button, event.Phase)
		case teleop.StickEvent:
			fmt.Printf("%s stick: %+0.3f, %+0.3f\n", event.Stick, event.Position.X, event.Position.Y)
		}
	}
}
