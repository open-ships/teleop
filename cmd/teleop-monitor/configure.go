package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/generic"
	"github.com/open-ships/teleop/profiles"
)

func configureController(ctx context.Context, provider *generic.Provider, profile profiles.Profile, id teleop.DeviceID, path string, input io.Reader, output io.Writer) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("mapping file %q already exists; choose a new path", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	devices, err := provider.Inspect(ctx)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		return fmt.Errorf("no generic gamepad found; connect it and run with --list --provider generic")
	}
	if id == "" && len(devices) > 1 {
		return fmt.Errorf("multiple generic gamepads found; choose one with --device (see --list)")
	}
	device := devices[0]
	if id != "" {
		found := false
		for _, d := range devices {
			if d.Descriptor.ID == id {
				device = d
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("generic controller %q is not connected", id)
		}
	}
	mapping, err := mapController(ctx, provider, profile, device.Descriptor.ID, input, output)
	if err != nil {
		return err
	}
	if err := saveNewMapping(path, mapping); err != nil {
		return err
	}
	fmt.Fprintf(output, "Saved %s.\nRun teleop-monitor --mapping %q --provider generic\n", terminalText(path), path)
	return nil
}

type rawControllerOpener interface {
	OpenRaw(context.Context, teleop.DeviceID) (generic.RawSource, error)
}

func mapController(ctx context.Context, provider rawControllerOpener, profile profiles.Profile, id teleop.DeviceID, input io.Reader, output io.Writer) (mapping generic.Mapping, err error) {
	source, err := provider.OpenRaw(ctx, id)
	if err != nil {
		return generic.Mapping{}, err
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	return learnMapping(ctx, source, profile, input, output)
}

func learnMapping(ctx context.Context, source generic.RawSource, profile profiles.Profile, input io.Reader, output io.Writer) (generic.Mapping, error) {
	// Use the open handle's identity in case discovery raced a reconnect.
	device := source.Device()
	fmt.Fprintf(output, "Configure %s for %s.\nRelease all controls, then press Enter. Ctrl-C cancels.\n", terminalText(device.Descriptor.Name), terminalText(profile.ID))
	if err := setupEnter(ctx, input); err != nil {
		return generic.Mapping{}, err
	}
	baseline, err := source.Read(ctx)
	if err != nil {
		return generic.Mapping{}, err
	}
	for _, control := range device.Controls {
		if control.Kind == generic.RawButton && baseline[control.ID] != 0 {
			return generic.Mapping{}, fmt.Errorf("a button is still pressed; release every control and restart setup")
		}
		if control.Kind == generic.RawHat && baseline[control.ID] >= control.Minimum && baseline[control.ID] <= control.Maximum {
			return generic.Mapping{}, fmt.Errorf("the D-pad is still pressed; release every control and restart setup")
		}
	}
	mapping := generic.NewMapping(device, profile)
	for index, control := range profile.Controls {
		fmt.Fprintf(output, "[%d/%d] Press and hold %s…\n", index+1, len(profile.Controls), terminalText(control.Label))
		binding, err := learnControl(ctx, source, baseline, control.ID, mapping.Bindings)
		if err != nil {
			return generic.Mapping{}, fmt.Errorf("learn %s: %w", control.Label, err)
		}
		mapping.Bindings = append(mapping.Bindings, binding)
		fmt.Fprintln(output, "  Captured. Release it.")
		if err := waitReleased(ctx, source, baseline); err != nil {
			return generic.Mapping{}, err
		}
	}
	if err := mapping.Validate(device, profile); err != nil {
		return generic.Mapping{}, err
	}
	return mapping, nil
}

func setupEnter(ctx context.Context, input io.Reader) error {
	_, err := readSetupLine(ctx, input)
	return err
}

func readSetupLine(ctx context.Context, input io.Reader) (string, error) {
	reader, ok := input.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(input)
	}
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() { line, err := reader.ReadString('\n'); done <- result{line, err} }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		return r.line, r.err
	}
}

// Require a stable press for 40 ms to avoid learning a transient or ambiguous
// multi-button press. Each step is bounded and respects cancellation.
func learnControl(ctx context.Context, source generic.RawSource, baseline generic.RawState, target teleop.ControlID, previous []generic.Binding) (generic.Binding, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var candidate generic.Binding
	var since time.Time
	for {
		raw, err := source.Read(ctx)
		if err != nil {
			return generic.Binding{}, err
		}
		binding, err := generic.DetectBinding(source.Device(), baseline, raw, target)
		if err != nil {
			since = time.Time{}
			continue
		}
		duplicate := false
		for _, old := range previous {
			if old.Input == binding.Input && old.Mode == binding.Mode {
				duplicate = true
				break
			}
		}
		if duplicate {
			since = time.Time{}
			continue
		}
		if since.IsZero() || binding != candidate {
			candidate = binding
			since = time.Now()
			continue
		}
		if time.Since(since) >= 40*time.Millisecond {
			return binding, nil
		}
	}
}
func waitReleased(ctx context.Context, source generic.RawSource, baseline generic.RawState) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var since time.Time
	for {
		raw, err := source.Read(ctx)
		if err != nil {
			return err
		}
		released := true
		for _, control := range source.Device().Controls {
			before, after := baseline[control.ID], raw[control.ID]
			if control.Kind == generic.RawAxis {
				if math.Abs(float64(before)-float64(after)) > float64(control.Maximum-control.Minimum)*0.2 {
					released = false
				}
			} else if before != after {
				released = false
			}
		}
		if !released {
			since = time.Time{}
			continue
		}
		if since.IsZero() {
			since = time.Now()
		}
		if time.Since(since) >= 100*time.Millisecond {
			return nil
		}
	}
}

// Write fully before linking into place. An existing mapping is never replaced,
// even if it appears after setup starts. The temporary file shares the target
// directory so the link is atomic and does not cross filesystems.
func saveNewMapping(path string, mapping generic.Mapping) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".teleop-mapping-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := generic.SaveMapping(file, mapping); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temp, path); err != nil {
		return fmt.Errorf("save mapping without replacing an existing file: %w", err)
	}
	return nil
}
