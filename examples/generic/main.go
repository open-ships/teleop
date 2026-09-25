// This example uses a mapping created by teleop-monitor --configure snes.json.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/generic"
	"github.com/open-ships/teleop/profiles"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: go run ./examples/generic path/to/snes.json")
	}
	if err := run(os.Args[1]); err != nil {
		log.Fatal(err)
	}
}
func run(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	mapping, err := generic.LoadMapping(file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	provider := generic.NewProvider(generic.WithProfile(profiles.SNES), generic.WithMapping(mapping))
	devices, err := provider.Discover(ctx)
	if err != nil {
		return err
	}
	var selected teleop.Descriptor
	for _, device := range devices {
		if device.Properties["mapping_status"] == "ready" {
			selected = device
			break
		}
	}
	if selected.ID == "" {
		return fmt.Errorf("no connected controller matches this mapping")
	}
	controller, err := provider.Open(ctx, selected.ID)
	if err != nil {
		return err
	}
	defer controller.Close()
	events, err := controller.Subscribe(teleop.SubscriptionOptions{Delivery: teleop.DeliveryLossless, Buffer: 1024})
	if err != nil {
		return err
	}
	defer events.Close()
	for {
		event, err := events.Next(ctx)
		if errors.Is(err, context.Canceled) || errors.Is(err, teleop.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if button, ok := event.(teleop.ButtonEvent); ok {
			fmt.Printf("%s %s\n", button.Button, button.Phase)
		}
	}
}
