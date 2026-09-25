//go:build windows

package generic

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"github.com/open-ships/teleop"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// WinMM provides non-XInput joystick access without an extra runtime library.
// This is a sampled compatibility backend, limited to 32 buttons and one POV.
var winmm = windows.NewLazySystemDLL("winmm.dll")
var joyNum = winmm.NewProc("joyGetNumDevs")
var joyCaps = winmm.NewProc("joyGetDevCapsW")
var joyPos = winmm.NewProc("joyGetPosEx")

type joystickCaps struct {
	Manufacturer, Product              uint16
	Name                               [32]uint16
	XMin, XMax, YMin, YMax, ZMin, ZMax uint32
	Buttons, PeriodMin, PeriodMax      uint32
	RMin, RMax, UMin, UMax, VMin, VMax uint32
	Caps, MaxAxes, Axes, MaxButtons    uint32
	RegistryKey                        [32]uint16
	OEMDriver                          [260]uint16
}
type joystickState struct {
	Size, Flags                                      uint32
	X, Y, Z, R, U, V                                 uint32
	Buttons, ButtonNumber, POV, Reserved1, Reserved2 uint32
}

var _ [728 - unsafe.Sizeof(joystickCaps{})]byte
var _ [unsafe.Sizeof(joystickCaps{}) - 728]byte
var _ [52 - unsafe.Sizeof(joystickState{})]byte
var _ [unsafe.Sizeof(joystickState{}) - 52]byte

func joystickError(operation string, status uintptr) error {
	if status == 0 {
		return nil
	}
	if status == 167 || status == 6 {
		return fmt.Errorf("%w: %s status %d", teleop.ErrDisconnected, operation, status)
	}
	return fmt.Errorf("%w: %s status %d", teleop.ErrUnavailable, operation, status)
}
func readJoystick(slot uint32) (joystickState, error) {
	state := joystickState{Size: uint32(unsafe.Sizeof(joystickState{})), Flags: 0xff | 0x200}
	result, _, _ := joyPos.Call(uintptr(slot), uintptr(unsafe.Pointer(&state)))
	return state, joystickError("joyGetPosEx", result)
}
func readJoystickCaps(slot uint32) (joystickCaps, error) {
	var caps joystickCaps
	result, _, _ := joyCaps.Call(uintptr(slot), uintptr(unsafe.Pointer(&caps)), unsafe.Sizeof(caps))
	return caps, joystickError("joyGetDevCapsW", result)
}

// Driver manufacturer IDs are not USB vendor IDs. Resolve the OEM name when
// available and keep the driver's own identifiers in descriptor properties.
func joystickOEM(slot uint32, caps joystickCaps) string {
	const base = `SYSTEM\CurrentControlSet\Control\MediaProperties\PrivateProperties\Joystick\`
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		key, err := registry.OpenKey(root, base+windows.UTF16ToString(caps.RegistryKey[:])+`\CurrentJoystickSettings`, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		oem, _, err := key.GetStringValue(fmt.Sprintf("Joystick%dOEMName", slot+1))
		key.Close()
		if err != nil {
			continue
		}
		for _, oemRoot := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
			key, err := registry.OpenKey(oemRoot, base+`OEM\`+oem, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			name, _, err := key.GetStringValue("OEMName")
			key.Close()
			if err == nil && name != "" {
				return name
			}
		}
	}
	return windows.UTF16ToString(caps.Name[:])
}
func joystickDevice(slot uint32, caps joystickCaps) Device {
	name := joystickOEM(slot, caps)
	if name == "" {
		name = "Windows joystick"
	}
	d := Device{Descriptor: teleop.Descriptor{ID: teleop.DeviceID(fmt.Sprintf("winmm:%d", slot)), Type: ControllerType, Name: name, Backend: "windows-winmm", Transport: teleop.TransportUnknown, Capability: teleop.Capabilities{AuditGrade: teleop.AuditSampledState}, Properties: map[string]string{"driver_manufacturer": strconv.Itoa(int(caps.Manufacturer)), "driver_product": strconv.Itoa(int(caps.Product)), "transport_health_scope": "fresh joyGetPosEx slot poll and matching joyGetDevCapsW; not physical link response", "audit_note": "WinMM state sampled every 8 ms; transitions between samples may be missed; slot identity may be reused"}}}
	for i := uint32(0); i < min(caps.Buttons, 32); i++ {
		d.Controls = append(d.Controls, RawControl{ID: fmt.Sprintf("button:%d", i+1), Kind: RawButton, Maximum: 1})
	}
	axes := []struct {
		id               string
		minimum, maximum uint32
		enabled          bool
	}{
		{"x", caps.XMin, caps.XMax, caps.Axes >= 1}, {"y", caps.YMin, caps.YMax, caps.Axes >= 2},
		{"z", caps.ZMin, caps.ZMax, caps.Caps&1 != 0}, {"r", caps.RMin, caps.RMax, caps.Caps&2 != 0},
		{"u", caps.UMin, caps.UMax, caps.Caps&4 != 0}, {"v", caps.VMin, caps.VMax, caps.Caps&8 != 0},
	}
	for _, a := range axes {
		if a.enabled && a.maximum > a.minimum {
			d.Controls = append(d.Controls, RawControl{ID: "axis:" + a.id, Kind: RawAxis, Minimum: int64(a.minimum), Maximum: int64(a.maximum)})
		}
	}
	if caps.Caps&16 != 0 {
		d.Controls = append(d.Controls, RawControl{ID: "pov", Kind: RawHat, Maximum: 7})
	}
	return d
}
func discoverPlatform(ctx context.Context) ([]Device, error) {
	for _, proc := range []*windows.LazyProc{joyNum, joyCaps, joyPos} {
		if err := proc.Find(); err != nil {
			return nil, fmt.Errorf("%w: %v", teleop.ErrUnsupported, err)
		}
	}
	count, _, _ := joyNum.Call()
	var devices []Device
	for slot := uint32(0); slot < uint32(count); slot++ {
		if err := ctx.Err(); err != nil {
			return devices, err
		}
		if _, err := readJoystick(slot); err != nil {
			continue
		}
		caps, err := readJoystickCaps(slot)
		if err != nil {
			continue
		}
		device := joystickDevice(slot, caps)
		if !xboxOwned(device.Descriptor) && len(device.Controls) > 0 {
			devices = append(devices, device)
		}
	}
	return devices, nil
}
func openPlatform(ctx context.Context, id teleop.DeviceID) (RawSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw := strings.TrimPrefix(string(id), "winmm:")
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || string(id) != fmt.Sprintf("winmm:%d", value) {
		return nil, fmt.Errorf("%w: invalid joystick ID", teleop.ErrUnavailable)
	}
	for _, proc := range []*windows.LazyProc{joyCaps, joyPos} {
		if err := proc.Find(); err != nil {
			return nil, fmt.Errorf("%w: %v", teleop.ErrUnsupported, err)
		}
	}
	slot := uint32(value)
	caps, err := readJoystickCaps(slot)
	if err != nil {
		return nil, err
	}
	device := joystickDevice(slot, caps)
	source := &sampledDevice{device: device, close: func() error { return nil }}
	source.poll = func() (RawState, error) {
		current, err := readJoystickCaps(slot)
		if err != nil {
			return nil, err
		}
		if current != caps {
			return nil, teleop.ErrDisconnected
		}
		state, err := readJoystick(slot)
		if err != nil {
			return nil, err
		}
		all := RawState{"axis:x": int64(state.X), "axis:y": int64(state.Y), "axis:z": int64(state.Z), "axis:r": int64(state.R), "axis:u": int64(state.U), "axis:v": int64(state.V), "pov": -1}
		if state.POV < 36000 {
			all["pov"] = int64((state.POV + 2250) / 4500 % 8)
		}
		for i := uint32(0); i < min(caps.Buttons, 32); i++ {
			all[fmt.Sprintf("button:%d", i+1)] = int64((state.Buttons >> i) & 1)
		}
		result := make(RawState, len(device.Controls))
		for _, c := range device.Controls {
			result[c.ID] = all[c.ID]
		}
		return result, nil
	}
	return source, nil
}
