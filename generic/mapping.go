package generic

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/open-ships/teleop/internal/strictjson"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/profiles"
)

// ErrMappingRequired means discovery succeeded but input has no explicit mapping.
var ErrMappingRequired = errors.New("generic: device mapping required")

// BindingMode converts one raw input into a digital control.
type BindingMode string

const (
	Button       BindingMode = "button"
	AxisNegative BindingMode = "axis-negative"
	AxisPositive BindingMode = "axis-positive"
	HatUp        BindingMode = "hat-up"
	HatRight     BindingMode = "hat-right"
	HatDown      BindingMode = "hat-down"
	HatLeft      BindingMode = "hat-left"
)

// Binding assigns a raw input to one physical position in a profile. Axis
// directions engage at half travel from center. Hats preserve diagonals.
type Binding struct {
	Control teleop.ControlID `json:"control"`
	Input   string           `json:"input"`
	Mode    BindingMode      `json:"mode"`
}

// DeviceMatch scopes a saved mapping to one backend, device identity and input
// layout. It deliberately does not use the transient attachment ID.
type DeviceMatch struct {
	Backend     string `json:"backend"`
	VendorID    uint16 `json:"vendor_id"`
	ProductID   uint16 `json:"product_id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// Mapping is a versioned, portable JSON document. It is portable between
// attachments with the same backend and layout, not between different OS APIs.
type Mapping struct {
	Version  int         `json:"version"`
	Profile  string      `json:"profile"`
	Match    DeviceMatch `json:"match"`
	Bindings []Binding   `json:"bindings"`
}

// NewMapping binds a profile to the inspected device. Populate Bindings before
// passing the mapping to WithMapping or saving it.
func NewMapping(device Device, profile profiles.Profile) Mapping {
	d := device.Descriptor
	return Mapping{Version: 1, Profile: profile.ID, Match: DeviceMatch{Backend: d.Backend, VendorID: d.VendorID, ProductID: d.ProductID, Name: d.Name, Fingerprint: device.Fingerprint()}}
}
func (m Mapping) clone() Mapping { m.Bindings = slices.Clone(m.Bindings); return m }
func (m Mapping) matches(d Device) bool {
	want := NewMapping(d, profiles.Profile{}).Match
	return m.Match == want
}

// LoadMapping rejects unknown fields, oversized files, and trailing documents.
// Device compatibility is checked at discovery/open time.
func LoadMapping(r io.Reader) (Mapping, error) {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20+1))
	if err != nil {
		return Mapping{}, err
	}
	if len(data) > 1<<20 {
		return Mapping{}, fmt.Errorf("mapping exceeds 1 MiB")
	}
	if err := strictjson.Validate(data, 1<<20); err != nil {
		return Mapping{}, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var m Mapping
	if err := dec.Decode(&m); err != nil {
		return Mapping{}, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Mapping{}, fmt.Errorf("mapping must contain exactly one JSON document")
	}
	if err := m.validateDocument(); err != nil {
		return Mapping{}, err
	}
	return m, nil
}

// SaveMapping checks document structure and writes JSON. Call Mapping.Validate
// first to check the target device and profile. The caller controls atomic file
// replacement and permissions; no directories or files are created here.
func SaveMapping(w io.Writer, m Mapping) error {
	if err := m.validateDocument(); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}
func (m Mapping) validateDocument() error {
	if m.Version != 1 || m.Profile == "" || m.Match.Backend == "" || m.Match.Name == "" || len(m.Match.Fingerprint) != 64 || len(m.Bindings) == 0 {
		return fmt.Errorf("%w: incomplete or unsupported mapping document", teleop.ErrInvalidState)
	}
	if _, err := hex.DecodeString(m.Match.Fingerprint); err != nil {
		return fmt.Errorf("%w: invalid input fingerprint", teleop.ErrInvalidState)
	}
	seen := map[teleop.ControlID]bool{}
	for _, b := range m.Bindings {
		if b.Control == "" || b.Input == "" || seen[b.Control] || !validMode(b.Mode) {
			return fmt.Errorf("%w: invalid or duplicate binding for %s", teleop.ErrInvalidState, b.Control)
		}
		seen[b.Control] = true
	}
	return nil
}
func validMode(m BindingMode) bool {
	switch m {
	case Button, AxisNegative, AxisPositive, HatUp, HatRight, HatDown, HatLeft:
		return true
	}
	return false
}

func validateProfile(p profiles.Profile) error {
	if p.ID == "" || len(p.Controls) == 0 {
		return fmt.Errorf("%w: empty profile", teleop.ErrInvalidState)
	}
	seen := map[teleop.ControlID]bool{}
	for _, c := range p.Controls {
		if c.ID == "" || seen[c.ID] || (c.Kind != teleop.ControlButton && c.Kind != teleop.ControlDPad) {
			return fmt.Errorf("%w: profile must contain unique digital controls", teleop.ErrInvalidState)
		}
		if c.Kind == teleop.ControlDPad && c.ID != teleop.DPadUp && c.ID != teleop.DPadDown && c.ID != teleop.DPadLeft && c.ID != teleop.DPadRight {
			return fmt.Errorf("%w: invalid D-pad control %s", teleop.ErrInvalidState, c.ID)
		}
		seen[c.ID] = true
	}
	return nil
}

func (m Mapping) validate(d Device, p profiles.Profile) error {
	if err := m.validateDocument(); err != nil {
		return err
	}
	if !m.matches(d) || m.Profile != p.ID {
		return fmt.Errorf("%w: mapping identity or profile mismatch", teleop.ErrInvalidState)
	}
	if len(m.Bindings) != len(p.Controls) {
		return fmt.Errorf("%w: map every profile control", teleop.ErrInvalidState)
	}
	inputs := map[string]RawControl{}
	for _, c := range d.Controls {
		inputs[c.ID] = c
	}
	used := map[string]bool{}
	for _, b := range m.Bindings {
		if !slices.ContainsFunc(p.Controls, func(c teleop.ControlDescriptor) bool { return c.ID == b.Control }) {
			return fmt.Errorf("%w: %s is not in profile %s", teleop.ErrInvalidState, b.Control, p.ID)
		}
		c, ok := inputs[b.Input]
		if !ok || !compatible(c, b.Mode) {
			return fmt.Errorf("%w: incompatible input %s for %s", teleop.ErrInvalidState, b.Input, b.Control)
		}
		key := b.Input + "/" + string(b.Mode)
		if used[key] {
			return fmt.Errorf("%w: input %s assigned twice", teleop.ErrInvalidState, key)
		}
		used[key] = true
	}
	return nil
}
func compatible(c RawControl, m BindingMode) bool {
	switch m {
	case Button:
		return c.Kind == RawButton && c.Minimum == 0 && c.Maximum >= 1
	case AxisNegative, AxisPositive:
		return c.Kind == RawAxis && c.Maximum > c.Minimum
	case HatUp, HatRight, HatDown, HatLeft:
		return c.Kind == RawHat && (c.Maximum-c.Minimum == 7 || c.Maximum-c.Minimum == 3)
	}
	return false
}

func pressed(c RawControl, value int64, mode BindingMode) bool {
	switch mode {
	case Button:
		return value != 0
	case AxisNegative:
		return axisValue(c, value) <= -0.5
	case AxisPositive:
		return axisValue(c, value) >= 0.5
	default:
		if value < c.Minimum || value > c.Maximum {
			return false
		}
		direction := value - c.Minimum
		if c.Maximum-c.Minimum == 3 {
			direction *= 2
		}
		switch mode {
		case HatUp:
			return direction == 7 || direction == 0 || direction == 1
		case HatRight:
			return direction >= 1 && direction <= 3
		case HatDown:
			return direction >= 3 && direction <= 5
		case HatLeft:
			return direction >= 5 && direction <= 7
		}
	}
	return false
}
func axisValue(c RawControl, v int64) float64 {
	return 2*(float64(v)-float64(c.Minimum))/(float64(c.Maximum)-float64(c.Minimum)) - 1
}

func validateRaw(d Device, raw RawState) error {
	for _, c := range d.Controls {
		v, ok := raw[c.ID]
		if !ok {
			return fmt.Errorf("%w: missing raw input %s", teleop.ErrInvalidState, c.ID)
		}
		if c.Maximum <= c.Minimum {
			return fmt.Errorf("%w: invalid range for %s", teleop.ErrInvalidState, c.ID)
		}
		if c.Kind != RawHat && (v < c.Minimum || v > c.Maximum) {
			return fmt.Errorf("%w: raw input %s outside range", teleop.ErrInvalidState, c.ID)
		}
	}
	return nil
}
func (m Mapping) apply(d Device, raw RawState) (teleop.State, error) {
	if err := validateRaw(d, raw); err != nil {
		return teleop.State{}, err
	}
	controls := map[string]RawControl{}
	for _, c := range d.Controls {
		controls[c.ID] = c
	}
	var state teleop.State
	for _, b := range m.Bindings {
		state.SetButton(b.Control, pressed(controls[b.Input], raw[b.Input], b.Mode))
	}
	return state, nil
}

// DetectBinding identifies a single button, axis direction, or cardinal hat
// movement from a released baseline. Ambiguous presses produce an error so a
// setup tool can ask for one control at a time. Hat diagonals are not learned.
func DetectBinding(d Device, baseline, current RawState, target teleop.ControlID) (Binding, error) {
	if err := validateRaw(d, baseline); err != nil {
		return Binding{}, err
	}
	if err := validateRaw(d, current); err != nil {
		return Binding{}, err
	}
	var found []Binding
	for _, c := range d.Controls {
		before, after := baseline[c.ID], current[c.ID]
		var mode BindingMode
		switch c.Kind {
		case RawButton:
			if before == 0 && after != 0 {
				mode = Button
			}
		case RawAxis:
			if math.Abs(axisValue(c, before)) < 0.25 {
				if axisValue(c, after) <= -0.5 {
					mode = AxisNegative
				}
				if axisValue(c, after) >= 0.5 {
					mode = AxisPositive
				}
			}
		case RawHat:
			if before < c.Minimum || before > c.Maximum {
				direction := after - c.Minimum
				if c.Maximum-c.Minimum == 3 {
					direction *= 2
				}
				switch direction {
				case 0:
					mode = HatUp
				case 2:
					mode = HatRight
				case 4:
					mode = HatDown
				case 6:
					mode = HatLeft
				}
			}
		}
		if mode != "" {
			found = append(found, Binding{Control: target, Input: c.ID, Mode: mode})
		}
	}
	if len(found) != 1 {
		return Binding{}, fmt.Errorf("expected one input, observed %d", len(found))
	}
	return found[0], nil
}

// Validate checks completeness and compatibility before a mapping is saved.
func (m Mapping) Validate(d Device, p profiles.Profile) error {
	if err := validateProfile(p); err != nil {
		return err
	}
	return m.validate(d, p)
}
