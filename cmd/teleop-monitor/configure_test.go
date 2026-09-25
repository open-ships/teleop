package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/generic"
	"github.com/open-ships/teleop/profiles"
)

func TestSNESMonitorUsesProfileLabelsAndHidesAnalogControls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := &stubController{descriptor: teleop.Descriptor{Type: generic.ControllerType, Name: "SNES pad", Capability: teleop.Capabilities{Controls: profiles.SNES.Clone().Controls}}}
	model := newMonitorModel(ctx, cancel, controller, &stubSubscription{}, "")
	for _, width := range []int{38, 58, 96} {
		rendered := model.renderState(width)
		for _, want := range []string{"[B]", "[A]", "[Y]", "[X]", "[L]", "[R]", "[Start]", "[Select]", "D-PAD"} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("width %d: missing %s in %s", width, want, rendered)
			}
		}
		for _, absent := range []string{"LT", "RT", "LEFT", "RIGHT", "Xbox", "View", "Share", "PADDLES"} {
			if strings.Contains(rendered, absent) {
				t.Fatalf("width %d: phantom control %s in %s", width, absent, rendered)
			}
		}
	}
}
func TestSelectDevicePrefersReadyAndHonorsExplicitID(t *testing.T) {
	devices := []teleop.Descriptor{{ID: "generic:1", Properties: map[string]string{"mapping_status": "required"}}, {ID: "xbox:1"}}
	d, err := selectDevice(devices, "")
	if err != nil || d.ID != "xbox:1" {
		t.Fatalf("default: %+v %v", d, err)
	}
	d, err = selectDevice(devices, "generic:1")
	if err != nil || d.ID != "generic:1" {
		t.Fatalf("explicit: %+v %v", d, err)
	}
	if _, err := selectDevice(nil, ""); err == nil {
		t.Fatal("empty discovery accepted")
	}
}
func TestSaveNewMappingDoesNotOverwrite(t *testing.T) {
	device := generic.Device{Descriptor: teleop.Descriptor{ID: "test", Name: "test", Backend: "test"}, Controls: []generic.RawControl{{ID: "button:1", Kind: generic.RawButton, Maximum: 1}}}
	mapping := generic.NewMapping(device, profiles.SNES)
	mapping.Bindings = []generic.Binding{{Control: teleop.ButtonFaceSouth, Input: "button:1", Mode: generic.Button}}
	path := filepath.Join(t.TempDir(), "mapping.json")
	if err := saveNewMapping(path, mapping); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveNewMapping(path, mapping); err == nil {
		t.Fatal("overwrote existing mapping")
	}
	second, err := os.ReadFile(path)
	if err != nil || string(first) != string(second) {
		t.Fatalf("original changed: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := generic.LoadMapping(file); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".teleop-mapping-*"))
	if len(matches) != 0 {
		t.Fatalf("temporary files leaked: %v", matches)
	}
}
