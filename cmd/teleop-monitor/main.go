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
	"runtime"

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
	// Apple's GameController framework initializes its device registry through
	// the macOS main thread's run loop.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "teleop-monitor:", terminalText(err.Error()))
		os.Exit(1)
	}
}

func run() (err error) {
	var config configuration
	flag.StringVar(&config.deviceID, "device", "", "device ID to open (defaults to the first controller)")
	flag.BoolVar(&config.list, "list", false, "list connected Xbox controllers and exit")
	flag.BoolVar(&config.json, "json", false, "write events as newline-delimited JSON")
	flag.StringVar(&config.audit, "audit", "", "write a lossless, hash-chained audit log to this file")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), monitoredSignals()...)
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
		auditFile, err = os.OpenFile(config.audit, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open audit log: %w", err)
		}
		recorder = audit.NewRecorder(auditFile, audit.WithFlushEveryEvent(true))
		defer func() {
			if closeErr := recorder.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close audit recorder: %w", closeErr))
			}
			if closeErr := auditFile.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close audit file: %w", closeErr))
			}
		}()
		openOptions = append(openOptions, teleop.WithAuditSink(recorder))
	}

	controller, err := provider.Open(ctx, device.ID, openOptions...)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := controller.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close controller: %w", closeErr))
		}
	}()

	streaming := config.json || !terminalOutput()
	delivery := teleop.DeliveryLatest
	if streaming {
		delivery = teleop.DeliveryLossless
	}
	subscription, err := controller.Subscribe(teleop.SubscriptionOptions{
		Delivery: delivery,
		Buffer:   8192,
	})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := subscription.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close subscription: %w", closeErr))
		}
	}()

	if streaming {
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
		fmt.Println(deviceLine(device))
	}
}

func deviceLine(device teleop.Descriptor) string {
	return fmt.Sprintf(
		"%s\t%s\tbackend=%s transport=%s audit=%s",
		terminalText(string(device.ID)),
		terminalText(device.Name),
		terminalText(device.Backend),
		terminalText(string(device.Transport)),
		terminalText(string(device.Capability.AuditGrade)),
	)
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
