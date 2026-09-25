// teleop-monitor is a Bubble Tea terminal monitor that can also emit
// newline-delimited JSON for pipes and logs.
package main

import (
	"bufio"
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
	"github.com/open-ships/teleop/generic"
	"github.com/open-ships/teleop/profiles"
	"github.com/open-ships/teleop/xbox"
)

type configuration struct {
	deviceID  string
	list      bool
	json      bool
	audit     string
	provider  string
	profile   string
	mapping   string
	configure string
}

func main() {
	// Apple's GameController framework initializes its device registry through
	// the macOS main thread's run loop.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "teleop-monitor:", terminalText(err.Error()))
		os.Exit(1)
	}
}

func run() (err error) {
	var config configuration
	flag.StringVar(&config.deviceID, "device", "", "device ID to open (skips the interactive controller picker)")
	flag.BoolVar(&config.list, "list", false, "list connected controllers and exit")
	flag.BoolVar(&config.json, "json", false, "write events as newline-delimited JSON")
	flag.StringVar(&config.audit, "audit", "", "write a lossless, hash-chained audit log to this file")
	flag.StringVar(&config.provider, "provider", "auto", "controller provider: auto, xbox, or generic")
	flag.StringVar(&config.profile, "profile", "snes", "generic controller layout (snes)")
	flag.StringVar(&config.mapping, "mapping", "", "saved generic device mapping JSON")
	flag.StringVar(&config.configure, "configure", "", "learn generic controls and save a new mapping JSON")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), monitoredSignals()...)
	defer cancel()

	if config.profile != "snes" {
		return fmt.Errorf("unknown profile %q; available: snes", config.profile)
	}
	if config.provider != "auto" && config.provider != "xbox" && config.provider != "generic" {
		return fmt.Errorf("unknown provider %q; choose auto, xbox, or generic", config.provider)
	}
	if config.configure != "" && (config.provider == "xbox" || config.list || config.json || config.audit != "" || config.mapping != "") {
		return fmt.Errorf("--configure cannot be combined with --provider xbox, --list, --json, --audit, or --mapping")
	}
	store, storeErr := defaultMappingStore()
	var mappings []generic.Mapping
	if config.provider != "xbox" && config.configure == "" {
		var loadErr error
		mappings, loadErr = store.load()
		if err := errors.Join(storeErr, loadErr); err != nil {
			fmt.Fprintln(os.Stderr, "teleop-monitor: saved mappings:", terminalText(err.Error()))
		}
	}
	if config.mapping != "" {
		if config.provider == "xbox" {
			return fmt.Errorf("--mapping requires the generic or auto provider")
		}
		mapping, err := readMapping(config.mapping)
		if err != nil {
			return fmt.Errorf("load mapping: %w", err)
		}
		// An explicit mapping takes precedence over the cached map for that device.
		mappings = replaceMapping(mappings, mapping)
	}
	genericProvider := genericProviderWithMappings(profiles.SNES, mappings)
	input := bufio.NewReader(os.Stdin)
	if config.configure != "" {
		return configureController(ctx, genericProvider, profiles.SNES, teleop.DeviceID(config.deviceID), config.configure, input, os.Stdout)
	}
	registry := teleop.NewRegistry()
	if config.provider != "generic" {
		registry.Register(xbox.NewProvider())
	}
	if config.provider != "xbox" {
		registry.Register(genericProvider)
	}
	devices, err := registry.Discover(ctx)
	if err != nil {
		if len(devices) == 0 {
			return err
		}
		// Registry preserves successful providers when another backend is unavailable.
		fmt.Fprintln(os.Stderr, "teleop-monitor: discovery:", terminalText(err.Error()))
	}
	if config.list {
		printDevices(devices)
		return nil
	}
	if len(devices) == 0 {
		return fmt.Errorf(
			"no controller found; connect it through the OS and run with --list",
		)
	}
	streaming := config.json || !terminalOutput()
	interactive := !streaming && isTerminal(int(os.Stdin.Fd()))
	device, err := chooseController(ctx, devices, teleop.DeviceID(config.deviceID), interactive, input, os.Stdout)
	if err != nil {
		return err
	}

	mapping, err := prepareGenericMapping(ctx, genericProvider, device, profiles.SNES, store, interactive, input, os.Stdout)
	if err != nil {
		return err
	}
	if mapping != nil {
		mappings = replaceMapping(mappings, *mapping)
		registry.Register(genericProviderWithMappings(profiles.SNES, mappings))
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

	controller, err := registry.Open(ctx, device.Type, device.ID, openOptions...)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := controller.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close controller: %w", closeErr))
		}
	}()

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
		fmt.Println("No connected controllers.")
		return
	}
	for _, device := range devices {
		fmt.Println(deviceLine(device))
	}
}

func deviceLine(device teleop.Descriptor) string {
	return fmt.Sprintf(
		"%s\t%s\tbackend=%s transport=%s audit=%s profile=%s mapping=%s",
		terminalText(string(device.ID)),
		terminalText(device.Name),
		terminalText(device.Backend),
		terminalText(string(device.Transport)),
		terminalText(string(device.Capability.AuditGrade)),
		terminalText(device.Properties["profile"]),
		terminalText(device.Properties["mapping_status"]),
	)
}

func selectDevice(devices []teleop.Descriptor, id teleop.DeviceID) (teleop.Descriptor, error) {
	if len(devices) == 0 {
		return teleop.Descriptor{}, fmt.Errorf("no connected controllers")
	}
	if id == "" {
		for _, device := range devices {
			if device.Properties["mapping_status"] != "required" {
				return device, nil
			}
		}
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
