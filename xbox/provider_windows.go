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

	xinputCapsFFBSupported  = 0x0001
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

type xinputVibration struct {
	LeftMotorSpeed  uint16
	RightMotorSpeed uint16
}

type xinputCapabilities struct {
	Type      uint8
	Subtype   uint8
	Flags     uint16
	Gamepad   xinputGamepad
	Vibration xinputVibration
}

var (
	_ [12 - unsafe.Sizeof(xinputGamepad{})]byte
	_ [unsafe.Sizeof(xinputGamepad{}) - 12]byte
	_ [16 - unsafe.Sizeof(xinputState{})]byte
	_ [unsafe.Sizeof(xinputState{}) - 16]byte
	_ [4 - unsafe.Offsetof(xinputState{}.Gamepad)]byte
	_ [unsafe.Offsetof(xinputState{}.Gamepad) - 4]byte
	_ [4 - unsafe.Sizeof(xinputVibration{})]byte
	_ [unsafe.Sizeof(xinputVibration{}) - 4]byte
	_ [20 - unsafe.Sizeof(xinputCapabilities{})]byte
	_ [unsafe.Sizeof(xinputCapabilities{}) - 20]byte
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
			rumble, err := xinputRumbleSupported(slot)
			if err != nil {
				return nil, err
			}
			devices = append(devices, windowsDescriptor(slot, rumble))
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
	rumble, err := xinputRumbleSupported(slot)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", id, err)
	}
	now := time.Now()
	return &windowsSource{
		slot:       slot,
		getState:   getState,
		setState:   xinputSetState,
		descriptor: windowsDescriptor(slot, rumble),
		lastPacket: ^uint32(0),
		ticker:     time.NewTicker(xinputPollInterval),
		done:       make(chan struct{}),
		health: teleop.TransportHealth{
			Sequence:          1,
			CheckedAt:         now,
			Connected:         true,
			SilenceVerifiable: true,
		},
	}, nil
}

func windowsDescriptor(slot uint32, rumble bool) teleop.Descriptor {
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
	capability.Rumble = rumble
	return teleop.Descriptor{
		ID:         teleop.DeviceID(fmt.Sprintf("xinput:%d", slot)),
		Type:       teleop.ControllerXbox,
		Name:       fmt.Sprintf("Xbox controller %d", slot+1),
		Transport:  teleop.TransportUnknown,
		Backend:    "windows-xinput",
		Capability: capability,
		Properties: map[string]string{
			"xinput_slot":            strconv.FormatUint(uint64(slot), 10),
			"audit_note":             "XInput exposes sampled state, not a complete event history",
			"transport_health_scope": "fresh XInputGetState slot poll; not physical link response",
		},
	}
}

type windowsSource struct {
	slot       uint32
	getState   *windows.LazyProc
	setState   *windows.LazyProc
	descriptor teleop.Descriptor
	lastPacket uint32
	ticker     *time.Ticker
	done       chan struct{}
	closeOnce  sync.Once
	rumbleMu   sync.Mutex
	healthMu   sync.RWMutex
	health     teleop.TransportHealth
	closed     bool
	closeErr   error
}

func (s *windowsSource) Descriptor() teleop.Descriptor {
	return s.descriptor.Clone()
}

func (s *windowsSource) TransportHealth() teleop.TransportHealth {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()
	return s.health
}

func (s *windowsSource) updateTransportHealth(checkedAt time.Time, connected bool) {
	s.healthMu.Lock()
	s.health.Sequence++
	s.health.CheckedAt = checkedAt
	s.health.Connected = connected
	s.health.SilenceVerifiable = true
	s.healthMu.Unlock()
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
			checkErr := callXInputGetState(s.getState, s.slot, &state)
			checkedAt := time.Now()
			if checkErr != nil {
				s.updateTransportHealth(checkedAt, false)
				return teleop.Observation{}, checkErr
			}
			s.updateTransportHealth(checkedAt, true)
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
				ObservedAt: checkedAt,
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

func (s *windowsSource) SetRumble(ctx context.Context, rumble teleop.Rumble) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.rumbleMu.Lock()
	defer s.rumbleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return teleop.ErrClosed
	}
	if !s.descriptor.Capability.Rumble || s.setState == nil {
		return teleop.ErrUnsupported
	}
	vibration := xinputRumble(rumble)
	return callXInputSetState(s.setState, s.slot, &vibration)
}

func (s *windowsSource) Close() error {
	s.closeOnce.Do(func() {
		s.rumbleMu.Lock()
		defer s.rumbleMu.Unlock()
		s.closed = true
		s.updateTransportHealth(time.Now(), false)
		if s.descriptor.Capability.Rumble && s.setState != nil {
			vibration := xinputVibration{}
			err := callXInputSetState(s.setState, s.slot, &vibration)
			if err != nil && !errors.Is(err, teleop.ErrDisconnected) {
				s.closeErr = err
			}
		}
		s.ticker.Stop()
		close(s.done)
	})
	return s.closeErr
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

func xinputRumble(rumble teleop.Rumble) xinputVibration {
	return xinputVibration{
		LeftMotorSpeed:  rumbleMagnitude(rumble.LowFrequency),
		RightMotorSpeed: rumbleMagnitude(rumble.HighFrequency),
	}
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
	xinputLoadOnce        sync.Once
	xinputGetState        *windows.LazyProc
	xinputGetCapabilities *windows.LazyProc
	xinputSetState        *windows.LazyProc
	xinputLoadErr         error
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
			capabilitiesProcedure := library.NewProc("XInputGetCapabilities")
			if err := capabilitiesProcedure.Find(); err != nil {
				loadErrors = append(loadErrors, err.Error())
				continue
			}
			setProcedure := library.NewProc("XInputSetState")
			if err := setProcedure.Find(); err != nil {
				loadErrors = append(loadErrors, err.Error())
				continue
			}
			xinputGetState = procedure
			xinputGetCapabilities = capabilitiesProcedure
			xinputSetState = setProcedure
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

func callXInputSetState(
	procedure *windows.LazyProc,
	slot uint32,
	vibration *xinputVibration,
) error {
	result, _, _ := procedure.Call(
		uintptr(slot),
		uintptr(unsafe.Pointer(vibration)),
	)
	return xinputSetStateResultError(result)
}

func xinputRumbleSupported(slot uint32) (bool, error) {
	if xinputGetCapabilities == nil {
		return false, teleop.ErrUnsupported
	}
	var capabilities xinputCapabilities
	result, _, _ := xinputGetCapabilities.Call(
		uintptr(slot),
		0,
		uintptr(unsafe.Pointer(&capabilities)),
	)
	if err := xinputOperationResultError("XInputGetCapabilities", result); err != nil {
		return false, err
	}
	return capabilities.Flags&xinputCapsFFBSupported != 0, nil
}

func xinputResultError(result uintptr) error {
	return xinputOperationResultError("XInputGetState", result)
}

func xinputSetStateResultError(result uintptr) error {
	return xinputOperationResultError("XInputSetState", result)
}

func xinputOperationResultError(operation string, result uintptr) error {
	switch result {
	case 0:
		return nil
	case errorDeviceNotConnected:
		return teleop.ErrDisconnected
	default:
		// XInputGetState returns a Win32 status directly; Proc.Call's third
		// value is GetLastError and is not the status for this API.
		return fmt.Errorf(
			"%w: %s: %w",
			teleop.ErrUnavailable,
			operation,
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
