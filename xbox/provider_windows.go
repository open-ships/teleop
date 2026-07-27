//go:build windows

package xbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/open-ships/teleop"
)

const (
	xinputDPadUp        = 0x0001
	xinputDPadDown      = 0x0002
	xinputDPadLeft      = 0x0004
	xinputDPadRight     = 0x0008
	xinputStart         = 0x0010
	xinputBack          = 0x0020
	xinputLeftThumb     = 0x0040
	xinputRightThumb    = 0x0080
	xinputLeftShoulder  = 0x0100
	xinputRightShoulder = 0x0200
	xinputA             = 0x1000
	xinputB             = 0x2000
	xinputX             = 0x4000
	xinputY             = 0x8000

	errorDeviceNotConnected = 1167
)

type xinputGamepad struct {
	Buttons      uint16
	LeftTrigger  uint8
	RightTrigger uint8
	ThumbLX      int16
	ThumbLY      int16
	ThumbRX      int16
	ThumbRY      int16
}

type xinputState struct {
	PacketNumber uint32
	Gamepad      xinputGamepad
}

func discoverPlatform(ctx context.Context) ([]teleop.Descriptor, error) {
	getState, err := loadXInput()
	if err != nil {
		return nil, err
	}
	var devices []teleop.Descriptor
	for slot := uint32(0); slot < 4; slot++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var state xinputState
		result, _, _ := getState.Call(uintptr(slot), uintptr(unsafe.Pointer(&state)))
		if result == 0 {
			devices = append(devices, windowsDescriptor(slot))
		}
	}
	return devices, nil
}

func openPlatform(ctx context.Context, id teleop.DeviceID) (teleop.InputSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slot, err := parseXInputID(id)
	if err != nil {
		return nil, err
	}
	getState, err := loadXInput()
	if err != nil {
		return nil, err
	}
	var state xinputState
	result, _, callErr := getState.Call(uintptr(slot), uintptr(unsafe.Pointer(&state)))
	if result != 0 {
		if result == errorDeviceNotConnected {
			return nil, fmt.Errorf("%w: %s", teleop.ErrDisconnected, id)
		}
		return nil, fmt.Errorf("XInputGetState: %w", callErr)
	}
	return &windowsSource{
		slot:       slot,
		getState:   getState,
		descriptor: windowsDescriptor(slot),
		lastPacket: ^uint32(0),
		done:       make(chan struct{}),
	}, nil
}

func windowsDescriptor(slot uint32) teleop.Descriptor {
	supported := map[teleop.ControlID]bool{
		ButtonA: true, ButtonB: true, ButtonX: true, ButtonY: true,
		LeftBumper: true, RightBumper: true, LeftStick: true, RightStick: true,
		Menu: true, View: true,
		teleop.DPadUp: true, teleop.DPadDown: true,
		teleop.DPadLeft: true, teleop.DPadRight: true,
		teleop.StickLeft: true, teleop.StickRight: true,
		teleop.TriggerLeft: true, teleop.TriggerRight: true,
	}
	capability := capabilities(teleop.AuditSampledState, supported)
	return teleop.Descriptor{
		ID:         teleop.DeviceID(fmt.Sprintf("xinput:%d", slot)),
		Type:       teleop.ControllerXbox,
		Name:       fmt.Sprintf("Xbox controller %d", slot+1),
		Transport:  teleop.TransportUnknown,
		Backend:    "windows-xinput",
		Capability: capability,
		Properties: map[string]string{
			"xinput_slot": strconv.FormatUint(uint64(slot), 10),
			"audit_note":  "XInput exposes sampled state, not a complete event history",
		},
	}
}

type windowsSource struct {
	slot       uint32
	getState   *syscall.LazyProc
	descriptor teleop.Descriptor
	lastPacket uint32
	done       chan struct{}
	closeOnce  sync.Once
}

func (s *windowsSource) Descriptor() teleop.Descriptor {
	return s.descriptor.Clone()
}

func (s *windowsSource) Read(ctx context.Context) (teleop.Observation, error) {
	ticker := time.NewTicker(4 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return teleop.Observation{}, ctx.Err()
		case <-s.done:
			return teleop.Observation{}, teleop.ErrClosed
		case <-ticker.C:
			var state xinputState
			result, _, callErr := s.getState.Call(
				uintptr(s.slot),
				uintptr(unsafe.Pointer(&state)),
			)
			if result != 0 {
				if result == errorDeviceNotConnected {
					return teleop.Observation{}, teleop.ErrDisconnected
				}
				return teleop.Observation{}, fmt.Errorf("XInputGetState: %w", callErr)
			}
			if state.PacketNumber == s.lastPacket {
				continue
			}
			s.lastPacket = state.PacketNumber
			var raw bytes.Buffer
			_ = binary.Write(&raw, binary.LittleEndian, state)
			return teleop.Observation{
				State:      xinputTeleopState(state.Gamepad),
				ObservedAt: time.Now(),
				Native: teleop.NativeInput{
					Format: "windows-xinput-state",
					Data:   raw.Bytes(),
					Fields: map[string]int64{
						"packet_number": int64(state.PacketNumber),
						"slot":          int64(s.slot),
					},
				},
			}, nil
		}
	}
}

func (s *windowsSource) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	return nil
}

func xinputTeleopState(gamepad xinputGamepad) teleop.State {
	button := func(mask uint16) bool { return gamepad.Buttons&mask != 0 }
	return teleop.State{
		Buttons: teleop.Buttons{
			FaceSouth:     button(xinputA),
			FaceEast:      button(xinputB),
			FaceWest:      button(xinputX),
			FaceNorth:     button(xinputY),
			BumperLeft:    button(xinputLeftShoulder),
			BumperRight:   button(xinputRightShoulder),
			StickLeft:     button(xinputLeftThumb),
			StickRight:    button(xinputRightThumb),
			MenuPrimary:   button(xinputStart),
			MenuSecondary: button(xinputBack),
		},
		LeftStick: teleop.Stick{
			X: normalizeXInputAxis(gamepad.ThumbLX),
			Y: normalizeXInputAxis(gamepad.ThumbLY),
		},
		RightStick: teleop.Stick{
			X: normalizeXInputAxis(gamepad.ThumbRX),
			Y: normalizeXInputAxis(gamepad.ThumbRY),
		},
		LeftTrigger:  float32(gamepad.LeftTrigger) / 255,
		RightTrigger: float32(gamepad.RightTrigger) / 255,
		DPad: teleop.DPad{
			Up:    button(xinputDPadUp),
			Down:  button(xinputDPadDown),
			Left:  button(xinputDPadLeft),
			Right: button(xinputDPadRight),
		},
	}
}

func normalizeXInputAxis(value int16) float32 {
	if value < 0 {
		return float32(value) / 32768
	}
	return float32(value) / 32767
}

func loadXInput() (*syscall.LazyProc, error) {
	var loadErrors []string
	for _, name := range []string{"xinput1_4.dll", "xinput1_3.dll", "xinput9_1_0.dll"} {
		library := syscall.NewLazyDLL(name)
		if err := library.Load(); err != nil {
			loadErrors = append(loadErrors, err.Error())
			continue
		}
		procedure := library.NewProc("XInputGetState")
		if err := procedure.Find(); err != nil {
			loadErrors = append(loadErrors, err.Error())
			continue
		}
		return procedure, nil
	}
	return nil, fmt.Errorf("%w: load XInput: %s", teleop.ErrUnavailable, strings.Join(loadErrors, "; "))
}

func parseXInputID(id teleop.DeviceID) (uint32, error) {
	value := strings.TrimPrefix(string(id), "xinput:")
	slot, err := strconv.ParseUint(value, 10, 32)
	if err != nil || slot > 3 {
		return 0, fmt.Errorf("%w: invalid XInput device ID %q", teleop.ErrUnavailable, id)
	}
	return uint32(slot), nil
}
