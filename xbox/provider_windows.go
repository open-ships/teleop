//go:build windows

package xbox

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/open-ships/teleop"
	"golang.org/x/sys/windows"
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
	xinputPollInterval      = 4 * time.Millisecond
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

var (
	_ [12 - unsafe.Sizeof(xinputGamepad{})]byte
	_ [unsafe.Sizeof(xinputGamepad{}) - 12]byte
	_ [16 - unsafe.Sizeof(xinputState{})]byte
	_ [unsafe.Sizeof(xinputState{}) - 16]byte
	_ [4 - unsafe.Offsetof(xinputState{}.Gamepad)]byte
	_ [unsafe.Offsetof(xinputState{}.Gamepad) - 4]byte
)

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
		err := callXInputGetState(getState, slot, &state)
		if err == nil {
			devices = append(devices, windowsDescriptor(slot))
			continue
		}
		if !errors.Is(err, teleop.ErrDisconnected) {
			return nil, err
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
	if err := callXInputGetState(getState, slot, &state); err != nil {
		return nil, fmt.Errorf("open %s: %w", id, err)
	}
	return &windowsSource{
		slot:       slot,
		getState:   getState,
		descriptor: windowsDescriptor(slot),
		lastPacket: ^uint32(0),
		ticker:     time.NewTicker(xinputPollInterval),
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
	getState   *windows.LazyProc
	descriptor teleop.Descriptor
	lastPacket uint32
	ticker     *time.Ticker
	done       chan struct{}
	closeOnce  sync.Once
}

func (s *windowsSource) Descriptor() teleop.Descriptor {
	return s.descriptor.Clone()
}

func (s *windowsSource) Read(ctx context.Context) (teleop.Observation, error) {
	for {
		select {
		case <-ctx.Done():
			return teleop.Observation{}, ctx.Err()
		case <-s.done:
			return teleop.Observation{}, teleop.ErrClosed
		case <-s.ticker.C:
			var state xinputState
			if err := callXInputGetState(s.getState, s.slot, &state); err != nil {
				return teleop.Observation{}, err
			}
			select {
			case <-s.done:
				return teleop.Observation{}, teleop.ErrClosed
			default:
			}
			if state.PacketNumber == s.lastPacket {
				continue
			}
			gap := xinputPacketGap(s.lastPacket, state.PacketNumber)
			s.lastPacket = state.PacketNumber
			return teleop.Observation{
				State:      xinputTeleopState(state.Gamepad),
				ObservedAt: time.Now(),
				Native: teleop.NativeInput{
					Format: "windows-xinput-state",
					Data:   encodeXInputState(state),
					Fields: map[string]int64{
						"packet_number": int64(state.PacketNumber),
						"slot":          int64(s.slot),
					},
				},
				Gap: gap,
			}, nil
		}
	}
}

func xinputPacketGap(previous, current uint32) *teleop.SourceGap {
	if previous == ^uint32(0) {
		return nil // Initial state has no prior poll to compare against.
	}
	delta := current - previous // uint32 subtraction preserves wrap-around.
	if delta <= 1 {
		return nil
	}
	return &teleop.SourceGap{
		Dropped: uint64(delta - 1),
		Reason:  "XInput packet-number discontinuity",
	}
}

func (s *windowsSource) Close() error {
	s.closeOnce.Do(func() {
		s.ticker.Stop()
		close(s.done)
	})
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

func encodeXInputState(state xinputState) []byte {
	data := make([]byte, 16)
	binary.LittleEndian.PutUint32(data[0:4], state.PacketNumber)
	binary.LittleEndian.PutUint16(data[4:6], state.Gamepad.Buttons)
	data[6] = state.Gamepad.LeftTrigger
	data[7] = state.Gamepad.RightTrigger
	binary.LittleEndian.PutUint16(data[8:10], uint16(state.Gamepad.ThumbLX))
	binary.LittleEndian.PutUint16(data[10:12], uint16(state.Gamepad.ThumbLY))
	binary.LittleEndian.PutUint16(data[12:14], uint16(state.Gamepad.ThumbRX))
	binary.LittleEndian.PutUint16(data[14:16], uint16(state.Gamepad.ThumbRY))
	return data
}

var (
	xinputLoadOnce sync.Once
	xinputGetState *windows.LazyProc
	xinputLoadErr  error
)

func loadXInput() (*windows.LazyProc, error) {
	xinputLoadOnce.Do(func() {
		var loadErrors []string
		for _, name := range []string{"xinput1_4.dll", "xinput1_3.dll", "xinput9_1_0.dll"} {
			// XInput is an operating-system component. Restrict lookup to the
			// Windows system directory so an application-local DLL cannot
			// shadow it.
			library := windows.NewLazySystemDLL(name)
			if err := library.Load(); err != nil {
				loadErrors = append(loadErrors, err.Error())
				continue
			}
			procedure := library.NewProc("XInputGetState")
			if err := procedure.Find(); err != nil {
				loadErrors = append(loadErrors, err.Error())
				continue
			}
			xinputGetState = procedure
			return
		}
		xinputLoadErr = fmt.Errorf(
			"%w: load XInput from the Windows system directory: %s",
			teleop.ErrUnavailable,
			strings.Join(loadErrors, "; "),
		)
	})
	return xinputGetState, xinputLoadErr
}

func callXInputGetState(
	procedure *windows.LazyProc,
	slot uint32,
	state *xinputState,
) error {
	result, _, _ := procedure.Call(
		uintptr(slot),
		uintptr(unsafe.Pointer(state)),
	)
	return xinputResultError(result)
}

func xinputResultError(result uintptr) error {
	switch result {
	case 0:
		return nil
	case errorDeviceNotConnected:
		return teleop.ErrDisconnected
	default:
		// XInputGetState returns a Win32 status directly; Proc.Call's third
		// value is GetLastError and is not the status for this API.
		return fmt.Errorf(
			"%w: XInputGetState: %w",
			teleop.ErrUnavailable,
			syscall.Errno(result),
		)
	}
}

func parseXInputID(id teleop.DeviceID) (uint32, error) {
	const prefix = "xinput:"
	raw := string(id)
	if !strings.HasPrefix(raw, prefix) {
		return 0, fmt.Errorf("%w: invalid XInput device ID %q", teleop.ErrUnavailable, id)
	}
	value := strings.TrimPrefix(raw, prefix)
	slot, err := strconv.ParseUint(value, 10, 32)
	if err != nil || slot > 3 || value != strconv.FormatUint(slot, 10) {
		return 0, fmt.Errorf("%w: invalid XInput device ID %q", teleop.ErrUnavailable, id)
	}
	return uint32(slot), nil
}
