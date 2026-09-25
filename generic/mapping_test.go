package generic

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/profiles"
)

func fixture() (Device, Mapping, RawState) {
	device := Device{Descriptor: teleop.Descriptor{ID: "test:1", Type: ControllerType, Name: "SNES fixture", Backend: "test", VendorID: 121, ProductID: 17, Capability: teleop.Capabilities{AuditGrade: teleop.AuditSampledState}}}
	raw := RawState{}
	for _, id := range []string{"b1", "b2", "b3", "b4", "b5", "b6", "b7", "b8"} {
		device.Controls = append(device.Controls, RawControl{ID: id, Kind: RawButton, Maximum: 1})
		raw[id] = 0
	}
	device.Controls = append(device.Controls, RawControl{ID: "x", Kind: RawAxis, Maximum: 255}, RawControl{ID: "y", Kind: RawAxis, Maximum: 255})
	raw["x"] = 127
	raw["y"] = 127
	mapping := NewMapping(device, profiles.SNES)
	for i, c := range profiles.SNES.Controls[:8] {
		mapping.Bindings = append(mapping.Bindings, Binding{Control: c.ID, Input: device.Controls[i].ID, Mode: Button})
	}
	mapping.Bindings = append(mapping.Bindings,
		Binding{Control: teleop.DPadUp, Input: "y", Mode: AxisNegative},
		Binding{Control: teleop.DPadDown, Input: "y", Mode: AxisPositive},
		Binding{Control: teleop.DPadLeft, Input: "x", Mode: AxisNegative},
		Binding{Control: teleop.DPadRight, Input: "x", Mode: AxisPositive},
	)
	return device, mapping, raw
}
func TestSNESMappingSimultaneousControlsAndRelease(t *testing.T) {
	device, mapping, raw := fixture()
	if err := mapping.Validate(device, profiles.SNES); err != nil {
		t.Fatal(err)
	}
	raw["b1"] = 1
	raw["b5"] = 1
	raw["x"] = 255
	raw["y"] = 0
	state, err := mapping.apply(device, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Button(teleop.ButtonFaceSouth) || !state.Button(teleop.ButtonBumperLeft) || !state.DPad.Up || !state.DPad.Right || state.DPad.Left || state.DPad.Down {
		t.Fatalf("incorrect simultaneous input: %+v", state)
	}
	_, _, raw = fixture()
	state, err = mapping.apply(device, raw)
	if err != nil || state.Button(teleop.ButtonFaceSouth) || state.DPad != (teleop.DPad{}) {
		t.Fatalf("release: %+v %v", state, err)
	}
	if state.LeftStick != (teleop.Stick{}) || state.LeftTrigger != 0 {
		t.Fatal("D-pad invented analog controls")
	}
}
func TestHatDirectionsAndNull(t *testing.T) {
	for _, positions := range []int64{4, 8} {
		for _, minimum := range []int64{0, 1} {
			c := RawControl{ID: "hat", Kind: RawHat, Minimum: minimum, Maximum: minimum + positions - 1}
			for value := minimum - 1; value <= minimum+positions; value++ {
				index := value - minimum
				if positions == 4 {
					index *= 2
				}
				valid := value >= c.Minimum && value <= c.Maximum
				expected := []bool{valid && (index == 7 || index == 0 || index == 1), valid && index >= 1 && index <= 3, valid && index >= 3 && index <= 5, valid && index >= 5 && index <= 7}
				for i, mode := range []BindingMode{HatUp, HatRight, HatDown, HatLeft} {
					if got := pressed(c, value, mode); got != expected[i] {
						t.Fatalf("positions=%d min=%d value=%d mode=%s: %v", positions, minimum, value, mode, got)
					}
				}
			}
		}
	}
}
func TestDetectBindingRejectsAmbiguityAndLearnsAxes(t *testing.T) {
	d, _, baseline := fixture()
	_, _, current := fixture()
	current["b2"] = 1
	b, err := DetectBinding(d, baseline, current, teleop.ButtonFaceEast)
	if err != nil || b.Input != "b2" || b.Mode != Button {
		t.Fatalf("button: %+v %v", b, err)
	}
	current["b3"] = 1
	if _, err := DetectBinding(d, baseline, current, teleop.ButtonFaceEast); err == nil {
		t.Fatal("accepted ambiguous press")
	}
	_, _, current = fixture()
	current["x"] = 0
	b, err = DetectBinding(d, baseline, current, teleop.DPadLeft)
	if err != nil || b.Input != "x" || b.Mode != AxisNegative {
		t.Fatalf("axis: %+v %v", b, err)
	}
	current["x"] = 126
	if _, err := DetectBinding(d, baseline, current, teleop.DPadLeft); err == nil {
		t.Fatal("learned center noise")
	}
}
func TestMappingRejectsInvalidInputAndWrongDevice(t *testing.T) {
	d, m, raw := fixture()
	for _, change := range []func(*Device, *Mapping){
		func(d *Device, m *Mapping) { d.Descriptor.ProductID++ },
		func(d *Device, m *Mapping) { d.Controls[0].ID = "other" },
		func(d *Device, m *Mapping) { m.Bindings = m.Bindings[:len(m.Bindings)-1] },
		func(d *Device, m *Mapping) { m.Bindings[0].Input = "unknown" },
		func(d *Device, m *Mapping) { m.Bindings[0].Mode = HatUp },
		func(d *Device, m *Mapping) { m.Bindings[0].Control = teleop.StickLeft },
		func(d *Device, m *Mapping) { m.Bindings[0] = m.Bindings[1] },
		func(d *Device, m *Mapping) { m.Bindings[0].Input = m.Bindings[1].Input },
	} {
		dd, mm := d.Clone(), m.clone()
		change(&dd, &mm)
		if err := mm.Validate(dd, profiles.SNES); err == nil {
			t.Fatalf("accepted invalid mapping: %+v", mm)
		}
	}
	raw["x"] = 999
	if _, err := m.apply(d, raw); !errors.Is(err, teleop.ErrInvalidState) {
		t.Fatalf("accepted out-of-range axis: %v", err)
	}
	delete(raw, "x")
	if _, err := m.apply(d, raw); !errors.Is(err, teleop.ErrInvalidState) {
		t.Fatalf("accepted incomplete sample: %v", err)
	}
}
func TestMappingPersistenceAndReconnectIdentity(t *testing.T) {
	d, m, _ := fixture()
	var buf bytes.Buffer
	if err := SaveMapping(&buf, m); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMapping(&buf)
	if err != nil {
		t.Fatal(err)
	}
	d.Descriptor.ID = "test:reattached"
	if err := loaded.Validate(d, profiles.SNES); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(m)
	for _, data := range []string{string(encoded) + " {}", strings.Replace(string(encoded), `"version":1`, `"version":2`, 1), strings.Replace(string(encoded), `"version":1`, `"unexpected":true,"version":1`, 1), strings.Repeat(" ", 1<<20+1)} {
		if _, err := LoadMapping(strings.NewReader(data)); err == nil {
			t.Fatal("accepted malformed mapping file")
		}
	}
}

func TestLoadMappingRejectsDuplicateJSONFields(t *testing.T) {
	_, mapping, _ := fixture()
	encoded, _ := json.Marshal(mapping)
	duplicate := strings.Replace(string(encoded), `"version":1`, `"version":1,"version":2`, 1)
	if _, err := LoadMapping(strings.NewReader(duplicate)); err == nil {
		t.Fatal("accepted ambiguous JSON")
	}
}

func TestHatSetupLearnsCardinalsAndRejectsDiagonals(t *testing.T) {
	device := Device{Controls: []RawControl{{ID: "hat", Kind: RawHat, Minimum: 1, Maximum: 8}}}
	baseline := RawState{"hat": 0}
	for value, want := range map[int64]BindingMode{1: HatUp, 3: HatRight, 5: HatDown, 7: HatLeft} {
		binding, err := DetectBinding(device, baseline, RawState{"hat": value}, teleop.DPadUp)
		if err != nil || binding.Mode != want {
			t.Fatalf("%d: %+v %v", value, binding, err)
		}
	}
	if _, err := DetectBinding(device, baseline, RawState{"hat": 2}, teleop.DPadUp); err == nil {
		t.Fatal("learned diagonal")
	}
}
