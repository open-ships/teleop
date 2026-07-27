// teleop-monitor is a Bubble Tea terminal monitor that can also emit
// newline-delimited JSON for pipes and logs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/xbox"
)

type configuration struct {
	deviceID string
	list     bool
	json     bool
	audit    string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "teleop-monitor:", err)
		os.Exit(1)
	}
}

func run() error {
	var config configuration
	flag.StringVar(&config.deviceID, "device", "", "device ID to open (defaults to the first controller)")
	flag.BoolVar(&config.list, "list", false, "list connected Xbox controllers and exit")
	flag.BoolVar(&config.json, "json", false, "write events as newline-delimited JSON")
	flag.StringVar(&config.audit, "audit", "", "write a lossless, hash-chained audit log to this file")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	provider := xbox.NewProvider()
	devices, err := provider.Discover(ctx)
	if err != nil {
		return err
	}
	if config.list {
		printDevices(devices)
		return nil
	}
	if len(devices) == 0 {
		return fmt.Errorf(
			"no Xbox controller found; connect it through the OS and run with --list",
		)
	}
	device, err := selectDevice(devices, teleop.DeviceID(config.deviceID))
	if err != nil {
		return err
	}

	var (
		openOptions []teleop.OpenOption
		recorder    *audit.Recorder
		auditFile   *os.File
	)
	if config.audit != "" {
		auditFile, err = os.OpenFile(config.audit, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("open audit log: %w", err)
		}
		defer auditFile.Close()
		recorder = audit.NewRecorder(auditFile, audit.WithFlushEveryEvent(true))
		defer recorder.Close()
		openOptions = append(openOptions, teleop.WithAuditSink(recorder))
	}

	controller, err := provider.Open(ctx, device.ID, openOptions...)
	if err != nil {
		return err
	}
	defer controller.Close()

	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{
		Delivery: teleop.DeliveryLossless,
		Buffer:   8192,
	})
	if err != nil {
		return err
	}
	defer subscription.Close()

	if config.json || !terminalOutput() {
		return streamJSON(ctx, subscription)
	}
	return runTUI(ctx, controller, subscription, config.audit)
}

func printDevices(devices []teleop.Descriptor) {
	if len(devices) == 0 {
		fmt.Println("No connected Xbox controllers.")
		return
	}
	for _, device := range devices {
		fmt.Printf(
			"%s\t%s\tbackend=%s transport=%s audit=%s\n",
			device.ID,
			device.Name,
			device.Backend,
			device.Transport,
			device.Capability.AuditGrade,
		)
	}
}

func selectDevice(devices []teleop.Descriptor, id teleop.DeviceID) (teleop.Descriptor, error) {
	if id == "" {
		return devices[0], nil
	}
	for _, device := range devices {
		if device.ID == id {
			return device, nil
		}
	}
	return teleop.Descriptor{}, fmt.Errorf("controller %q is not connected", id)
}

func streamJSON(ctx context.Context, subscription teleop.Subscription) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	for {
		event, err := subscription.Next(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, teleop.ErrClosed) {
				return nil
			}
			return err
		}
		envelope := struct {
			Kind  teleop.EventKind `json:"kind"`
			Event teleop.Event     `json:"event"`
		}{
			Kind:  event.Kind(),
			Event: event,
		}
		if err := encoder.Encode(envelope); err != nil {
			return err
		}
	}
}
