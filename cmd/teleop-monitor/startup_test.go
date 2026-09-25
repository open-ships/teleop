package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/generic"
	"github.com/open-ships/teleop/profiles"
)

func TestControllerPickerDoesNotSkipUnmappedPadOrRememberSelection(t *testing.T) {
	devices := []teleop.Descriptor{
		{ID: "generic:one", Type: generic.ControllerType, Name: "SNES pad", Properties: map[string]string{"mapping_status": "required"}},
		{ID: "xbox:one", Type: teleop.ControllerXbox, Name: "Xbox pad"},
	}
	for _, selection := range []struct {
		line string
		want teleop.DeviceID
	}{{"1\n", "generic:one"}, {"2\n", "xbox:one"}, {"1\n", "generic:one"}} {
		var output bytes.Buffer
		got, err := chooseController(context.Background(), devices, "", true, strings.NewReader(selection.line), &output)
		if err != nil || got.ID != selection.want {
			t.Fatalf("selection %+v: %+v %v", selection, got, err)
		}
		for _, want := range []string{"Choose a controller", "SNES pad", "Xbox pad", "needs mapping"} {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("picker missing %q: %s", want, output.String())
			}
		}
	}
	// Even a single attached pad is explicitly selected on an interactive launch.
	var output bytes.Buffer
	if _, err := chooseController(context.Background(), devices[:1], "", true, strings.NewReader("1\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Controller number") {
		t.Fatal("single controller bypassed picker")
	}
}
func TestPickerRetriesAndSharesBufferedInputWithSetup(t *testing.T) {
	device := teleop.Descriptor{ID: "pad", Name: "Pad"}
	input := bufio.NewReader(strings.NewReader("bogus\n0\n2\n1\n\n"))
	var output bytes.Buffer
	got, err := chooseController(context.Background(), []teleop.Descriptor{device}, "", true, input, &output)
	if err != nil || got.ID != device.ID {
		t.Fatalf("selection: %+v %v", got, err)
	}
	if err := setupEnter(context.Background(), input); err != nil {
		t.Fatalf("picker consumed setup input: %v", err)
	}
	if strings.Count(output.String(), "Enter a number") != 3 {
		t.Fatalf("invalid selections: %s", output.String())
	}
	if _, err := chooseController(context.Background(), []teleop.Descriptor{device}, "", true, strings.NewReader("q\n"), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("quit: %v", err)
	}
}
func TestExplicitAndNoninteractiveSelectionDoNotPrompt(t *testing.T) {
	devices := []teleop.Descriptor{{ID: "one"}, {ID: "two"}}
	for _, interactive := range []bool{false, true} {
		var output bytes.Buffer
		got, err := chooseController(context.Background(), devices, "two", interactive, strings.NewReader(""), &output)
		if err != nil || got.ID != "two" || output.Len() != 0 {
			t.Fatalf("explicit selection: %+v %v %s", got, err, output.String())
		}
	}
	var output bytes.Buffer
	if _, err := chooseController(context.Background(), devices, "", false, strings.NewReader(""), &output); err != nil || output.Len() != 0 {
		t.Fatalf("noninteractive: %v %s", err, output.String())
	}
}

// This raw source follows setup's prompts, emulating a person holding each
// requested button and releasing it when captured. All twelve SNES controls
// traverse the real detection, validation, persistence and reuse path.
type setupPad struct {
	device  generic.Device
	pressed int
	closed  bool
	opens   int
}

func newSetupPad(name string) *setupPad {
	d := generic.Device{Descriptor: teleop.Descriptor{ID: teleop.DeviceID(name), Type: generic.ControllerType, Name: name, Backend: "setup-test", Properties: map[string]string{"mapping_status": "required"}}}
	for i := range profiles.SNES.Controls {
		d.Controls = append(d.Controls, generic.RawControl{ID: fmt.Sprintf("button:%d", i), Kind: generic.RawButton, Maximum: 1})
	}
	return &setupPad{device: d, pressed: -1}
}
func (p *setupPad) Device() generic.Device { return p.device.Clone() }
func (p *setupPad) OpenRaw(ctx context.Context, id teleop.DeviceID) (generic.RawSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id != p.device.Descriptor.ID {
		return nil, teleop.ErrUnavailable
	}
	p.opens++
	p.closed = false
	return p, nil
}
func (p *setupPad) Read(ctx context.Context) (generic.RawState, error) {
	timer := time.NewTimer(5 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	raw := generic.RawState{}
	for i, c := range p.device.Controls {
		raw[c.ID] = 0
		if i == p.pressed {
			raw[c.ID] = 1
		}
	}
	return raw, nil
}
func (p *setupPad) Close() error { p.closed = true; return nil }

type setupOutput struct {
	bytes.Buffer
	pad  *setupPad
	next int
}

func (o *setupOutput) Write(b []byte) (int, error) {
	text := string(b)
	if strings.Contains(text, "Press and hold") {
		o.pad.pressed = o.next
		o.next++
	}
	if strings.Contains(text, "Captured") {
		o.pad.pressed = -1
	}
	return o.Buffer.Write(b)
}
func TestGenericSetupSavesPerDeviceAndContinuesInSameRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	pad := newSetupPad("SNES one")
	store := mappingStore{directory: filepath.Join(t.TempDir(), "mappings")}
	output := &setupOutput{pad: pad}
	mapping, err := prepareGenericMapping(ctx, pad, pad.device.Descriptor, profiles.SNES, store, true, strings.NewReader("\n"), output)
	if err != nil {
		t.Fatal(err)
	}
	if mapping == nil || !pad.closed || pad.opens != 1 {
		t.Fatalf("setup lifecycle: %+v, closed=%v opens=%d", mapping, pad.closed, pad.opens)
	}
	if err := mapping.Validate(pad.device, profiles.SNES); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Opening monitor") || strings.Contains(output.String(), "Run teleop-monitor") {
		t.Fatalf("setup did not continue: %s", output.String())
	}
	loaded, err := store.load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("saved maps: %+v %v", loaded, err)
	}
	if err := loaded[0].Validate(pad.device, profiles.SNES); err != nil {
		t.Fatal(err)
	}
	// A second device gets its own cache entry; updating one never erases the other.
	second := *mapping
	second.Match.Name = "SNES two"
	if err := store.save(second); err != nil {
		t.Fatal(err)
	}
	if err := store.save(*mapping); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.load()
	if err != nil || len(loaded) != 2 {
		t.Fatalf("multiple controllers: %+v %v", loaded, err)
	}
	matches, _ := filepath.Glob(filepath.Join(store.directory, ".mapping-*"))
	if len(matches) != 0 {
		t.Fatalf("left temporary maps: %v", matches)
	}
	// The loaded map skips setup. The picker is still shown by chooseController.
	ready := pad.device.Descriptor.Clone()
	ready.Properties["mapping_status"] = "ready"
	again, err := prepareGenericMapping(ctx, pad, ready, profiles.SNES, store, true, strings.NewReader(""), io.Discard)
	if err != nil || again != nil || pad.opens != 1 {
		t.Fatalf("ready controller was remapped: %+v %v opens=%d", again, err, pad.opens)
	}
}
func TestUnmappedJSONDoesNotPromptOrOpenHardware(t *testing.T) {
	pad := newSetupPad("SNES")
	var output bytes.Buffer
	_, err := prepareGenericMapping(context.Background(), pad, pad.device.Descriptor, profiles.SNES, mappingStore{}, false, strings.NewReader(""), &output)
	if !errors.Is(err, generic.ErrMappingRequired) || output.Len() != 0 || pad.opens != 0 {
		t.Fatalf("noninteractive setup: %v output=%q opens=%d", err, output.String(), pad.opens)
	}
	xbox := teleop.Descriptor{Type: teleop.ControllerXbox}
	if m, err := prepareGenericMapping(context.Background(), pad, xbox, profiles.SNES, mappingStore{}, true, strings.NewReader(""), &output); err != nil || m != nil || pad.opens != 0 {
		t.Fatalf("Xbox requested setup: %+v %v", m, err)
	}
}
func TestCancelledSetupDoesNotPersistAndClosesRawSource(t *testing.T) {
	pad := newSetupPad("SNES")
	store := mappingStore{directory: filepath.Join(t.TempDir(), "maps")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := prepareGenericMapping(ctx, pad, pad.device.Descriptor, profiles.SNES, store, true, strings.NewReader("\n"), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	// Cancellation after opening must also release the raw source.
	if _, err := mapController(context.Background(), pad, profiles.SNES, pad.device.Descriptor.ID, strings.NewReader(""), io.Discard); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF: %v", err)
	}
	if !pad.closed {
		t.Fatal("raw source left open after cancelled setup")
	}
	if _, err := os.Stat(store.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete setup wrote mappings: %v", err)
	}
}
func TestMappingCacheKeepsValidDevicesWhenOneFileIsCorrupt(t *testing.T) {
	pad := newSetupPad("SNES")
	mapping := generic.NewMapping(pad.device, profiles.SNES)
	mapping.Bindings = []generic.Binding{{Control: teleop.ButtonFaceSouth, Input: "button:0", Mode: generic.Button}}
	store := mappingStore{directory: t.TempDir()}
	if err := store.save(mapping); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.directory, "broken.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.load()
	if err == nil || len(loaded) != 1 {
		t.Fatalf("partial cache load: %+v %v", loaded, err)
	}
	override := mapping
	override.Bindings = []generic.Binding{{Control: teleop.ButtonFaceSouth, Input: "button:1", Mode: generic.Button}}
	loaded = replaceMapping(loaded, override)
	if len(loaded) != 1 || loaded[0].Bindings[0].Input != "button:1" {
		t.Fatalf("explicit mapping failed to override cache: %+v", loaded)
	}
}
