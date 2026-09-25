package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/generic"
	"github.com/open-ships/teleop/profiles"
)

// The monitor owns persistence; the generic library remains explicitly configured.
// Each device identity/layout has its own file. No selected controller is saved.
type mappingStore struct{ directory string }

func defaultMappingStore() (mappingStore, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return mappingStore{}, err
	}
	return mappingStore{directory: filepath.Join(directory, "teleop", "mappings")}, nil
}
func mappingKey(mapping generic.Mapping) string {
	identity := struct {
		Profile string
		Match   generic.DeviceMatch
	}{mapping.Profile, mapping.Match}
	data, _ := json.Marshal(identity)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
func readMapping(path string) (generic.Mapping, error) {
	file, err := os.Open(path)
	if err != nil {
		return generic.Mapping{}, err
	}
	mapping, err := generic.LoadMapping(file)
	return mapping, errors.Join(err, file.Close())
}
func (s mappingStore) load() ([]generic.Mapping, error) {
	if s.directory == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var mappings []generic.Mapping
	var loadErr error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		mapping, err := readMapping(filepath.Join(s.directory, entry.Name()))
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		mappings = replaceMapping(mappings, mapping)
	}
	return mappings, loadErr
}
func (s mappingStore) save(mapping generic.Mapping) error {
	if s.directory == "" {
		return errors.New("user configuration directory is unavailable")
	}
	if err := os.MkdirAll(s.directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(s.directory, ".mapping-*")
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
	// Only this identity's cache entry is replaced; other pads retain their maps.
	return os.Rename(temp, filepath.Join(s.directory, mappingKey(mapping)+".json"))
}
func replaceMapping(mappings []generic.Mapping, mapping generic.Mapping) []generic.Mapping {
	key := mappingKey(mapping)
	result := make([]generic.Mapping, 0, len(mappings)+1)
	for _, existing := range mappings {
		if mappingKey(existing) != key {
			result = append(result, existing)
		}
	}
	return append(result, mapping)
}
func genericProviderWithMappings(profile profiles.Profile, mappings []generic.Mapping) *generic.Provider {
	options := []generic.Option{generic.WithProfile(profile)}
	for _, mapping := range mappings {
		options = append(options, generic.WithMapping(mapping))
	}
	return generic.NewProvider(options...)
}

func chooseController(ctx context.Context, devices []teleop.Descriptor, id teleop.DeviceID, interactive bool, input io.Reader, output io.Writer) (teleop.Descriptor, error) {
	if !interactive || id != "" || len(devices) == 0 {
		return selectDevice(devices, id)
	}
	reader, ok := input.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(input)
	}
	fmt.Fprintln(output, "Choose a controller:")
	for index, device := range devices {
		status := "ready"
		if device.Properties["mapping_status"] == "required" {
			status = "needs mapping"
		}
		fmt.Fprintf(output, "  %d. %s (%s, %s)\n     %s\n", index+1, terminalText(device.Name), terminalText(string(device.Type)), status, terminalText(string(device.ID)))
	}
	for {
		fmt.Fprint(output, "Controller number (q to quit): ")
		line, err := readSetupLine(ctx, reader)
		if err != nil {
			return teleop.Descriptor{}, err
		}
		line = strings.TrimSpace(line)
		if strings.EqualFold(line, "q") {
			return teleop.Descriptor{}, context.Canceled
		}
		index, err := strconv.Atoi(line)
		if err == nil && index >= 1 && index <= len(devices) {
			return devices[index-1], nil
		}
		fmt.Fprintf(output, "Enter a number from 1 to %d.\n", len(devices))
	}
}

// prepareGenericMapping returns a new mapping only when setup was necessary.
// The caller registers it and continues opening the selected controller.
func prepareGenericMapping(ctx context.Context, provider rawControllerOpener, device teleop.Descriptor, profile profiles.Profile, store mappingStore, interactive bool, input io.Reader, output io.Writer) (*generic.Mapping, error) {
	if device.Type != generic.ControllerType || device.Properties["mapping_status"] != "required" {
		return nil, nil
	}
	if !interactive {
		return nil, fmt.Errorf("%w: open teleop-monitor in an interactive terminal and select %q to map it, or supply --mapping", generic.ErrMappingRequired, device.Name)
	}
	fmt.Fprintln(output, "Let's map this controller before opening the monitor.")
	mapping, err := mapController(ctx, provider, profile, device.ID, input, output)
	if err != nil {
		return nil, err
	}
	if err := store.save(mapping); err != nil {
		fmt.Fprintf(output, "Mapping is ready for this session, but could not be saved: %s\n", terminalText(err.Error()))
	} else {
		fmt.Fprintln(output, "Mapping saved for this controller. It will be loaded automatically next time.")
	}
	fmt.Fprintln(output, "Opening monitor…")
	return &mapping, nil
}
