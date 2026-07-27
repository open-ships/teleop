//go:build linux

package xbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/open-ships/teleop"
)

const (
	evSyn = 0x00
	evKey = 0x01
	evAbs = 0x03

	synReport  = 0
	synDropped = 3

	absX     = 0x00
	absY     = 0x01
	absZ     = 0x02
	absRX    = 0x03
	absRY    = 0x04
	absRZ    = 0x05
	absHat0X = 0x10
	absHat0Y = 0x11

	btnSouth  = 0x130
	btnEast   = 0x131
	btnNorth  = 0x133
	btnWest   = 0x134
	btnTL     = 0x136
	btnTR     = 0x137
	btnSelect = 0x13a
	btnStart  = 0x13b
	btnMode   = 0x13c
	btnThumbL = 0x13d
	btnThumbR = 0x13e

	btnDPadUp    = 0x220
	btnDPadDown  = 0x221
	btnDPadLeft  = 0x222
	btnDPadRight = 0x223

	// Kernel 6.17 standardized these grip/paddle codes.
	btnGripLeft   = 0x224
	btnGripRight  = 0x225
	btnGripLeft2  = 0x226
	btnGripRight2 = 0x227

	keyF12    = 88
	keyRecord = 167

	busUSB       = 0x03
	busBluetooth = 0x05
)

type linuxInputID struct {
	BusType uint16
	Vendor  uint16
	Product uint16
	Version uint16
}

type linuxAbsInfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

type linuxInputEvent struct {
	Time  syscall.Timeval
	Type  uint16
	Code  uint16
	Value int32
}

func discoverPlatform(ctx context.Context) ([]teleop.Descriptor, error) {
	paths, err := linuxInputPaths()
	if err != nil {
		return nil, err
	}
	var (
		devices       []teleop.Descriptor
		permissionErr error
	)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		descriptor, xboxDevice, err := inspectLinuxDevice(path)
		if err != nil {
			if errors.Is(err, os.ErrPermission) {
				permissionErr = err
			}
			continue
		}
		if xboxDevice {
			devices = append(devices, descriptor)
		}
	}
	if len(devices) == 0 && permissionErr != nil {
		return nil, fmt.Errorf("%w: read /dev/input devices: %v", teleop.ErrPermission, permissionErr)
	}
	return devices, nil
}

func linuxInputPaths() ([]string, error) {
	stable, err := filepath.Glob("/dev/input/by-id/*-event-joystick")
	if err != nil {
		return nil, fmt.Errorf("discover stable input paths: %w", err)
	}
	fallback, err := filepath.Glob("/dev/input/event*")
	if err != nil {
		return nil, fmt.Errorf("discover input paths: %w", err)
	}
	seen := make(map[string]bool)
	var paths []string
	for _, path := range append(stable, fallback...) {
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		if seen[realPath] {
			continue
		}
		seen[realPath] = true
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func inspectLinuxDevice(path string) (teleop.Descriptor, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return teleop.Descriptor{}, false, err
	}
	defer file.Close()

	name, err := linuxDeviceString(file.Fd(), 0x06)
	if err != nil {
		return teleop.Descriptor{}, false, err
	}
	lowerName := strings.ToLower(name)
	if !strings.Contains(lowerName, "xbox") &&
		!strings.Contains(lowerName, "x-box") &&
		!strings.Contains(lowerName, "microsoft") {
		return teleop.Descriptor{}, false, nil
	}

	var inputID linuxInputID
	if err := linuxIOCTL(file.Fd(), linuxIOR('E', 0x02, unsafe.Sizeof(inputID)), unsafe.Pointer(&inputID)); err != nil {
		return teleop.Descriptor{}, false, err
	}
	transport := teleop.TransportUnknown
	switch inputID.BusType {
	case busBluetooth:
		transport = teleop.TransportBluetooth
	case busUSB:
		transport = teleop.TransportUSB
	}
	realPath, _ := filepath.EvalSymlinks(path)
	supported, _, err := linuxCapabilities(file)
	if err != nil {
		return teleop.Descriptor{}, false, err
	}
	if !supported[ButtonA] || !supported[teleop.StickLeft] {
		return teleop.Descriptor{}, false, nil
	}
	return teleop.Descriptor{
		ID:        teleop.DeviceID(path),
		Type:      teleop.ControllerXbox,
		Name:      name,
		Transport: transport,
		Backend:   "linux-evdev",
		VendorID:  inputID.Vendor,
		ProductID: inputID.Product,
		Properties: map[string]string{
			"path":      path,
			"real_path": realPath,
		},
		Capability: capabilities(teleop.AuditExactBackendStream, supported),
	}, true, nil
}

func openPlatform(ctx context.Context, id teleop.DeviceID) (teleop.InputSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := string(id)
	descriptor, xboxDevice, err := inspectLinuxDevice(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w: open %s: %v", teleop.ErrPermission, path, err)
		}
		return nil, err
	}
	if !xboxDevice {
		return nil, fmt.Errorf("%w: %s is not an Xbox controller", teleop.ErrUnavailable, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	_, ranges, err := linuxCapabilities(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	source := &linuxSource{
		file:       file,
		descriptor: descriptor,
		ranges:     ranges,
		initial:    true,
	}
	if err := source.resync(); err != nil {
		file.Close()
		return nil, err
	}
	return source, nil
}

type linuxSource struct {
	file       *os.File
	descriptor teleop.Descriptor
	ranges     map[uint16]linuxAbsInfo
	state      teleop.State
	raw        bytes.Buffer
	dropped    bool
	initial    bool
	closeOnce  sync.Once
}

func (s *linuxSource) Descriptor() teleop.Descriptor {
	return s.descriptor.Clone()
}

func (s *linuxSource) Read(ctx context.Context) (teleop.Observation, error) {
	if s.initial {
		s.initial = false
		return teleop.Observation{
			State:      s.state.Clone(),
			ObservedAt: time.Now(),
			Native: teleop.NativeInput{
				Format: "linux-evdev-initial-state",
			},
		}, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return teleop.Observation{}, err
		}
		var event linuxInputEvent
		if err := binary.Read(s.file, binary.LittleEndian, &event); err != nil {
			if errors.Is(err, os.ErrClosed) {
				return teleop.Observation{}, teleop.ErrClosed
			}
			return teleop.Observation{}, err
		}
		_ = binary.Write(&s.raw, binary.LittleEndian, event)

		if event.Type == evSyn && event.Code == synDropped {
			s.dropped = true
			continue
		}
		if s.dropped {
			if event.Type == evSyn && event.Code == synReport {
				if err := s.resync(); err != nil {
					return teleop.Observation{}, err
				}
				raw := append([]byte(nil), s.raw.Bytes()...)
				s.raw.Reset()
				s.dropped = false
				return teleop.Observation{
					State:      s.state.Clone(),
					ObservedAt: linuxEventTime(event),
					Native: teleop.NativeInput{
						Format: "linux-evdev",
						Data:   raw,
					},
					Gap: &teleop.SourceGap{Reason: "evdev SYN_DROPPED buffer overrun"},
				}, nil
			}
			continue
		}

		s.apply(event)
		if event.Type == evSyn && event.Code == synReport {
			raw := append([]byte(nil), s.raw.Bytes()...)
			s.raw.Reset()
			return teleop.Observation{
				State:           s.state.Clone(),
				ObservedAt:      linuxEventTime(event),
				DeviceTimestamp: int64(event.Time.Sec)*1_000_000_000 + int64(event.Time.Usec)*1_000,
				Native: teleop.NativeInput{
					Format: "linux-evdev",
					Data:   raw,
				},
			}, nil
		}
	}
}

func (s *linuxSource) apply(event linuxInputEvent) {
	switch event.Type {
	case evKey:
		if control, ok := linuxKeyControl(event.Code); ok {
			s.state.SetButton(control, event.Value != 0)
		}
	case evAbs:
		info, known := s.ranges[event.Code]
		if !known {
			return
		}
		switch event.Code {
		case absX:
			s.state.LeftStick.X = teleop.NormalizeAxis(event.Value, info.Minimum, info.Maximum)
		case absY:
			s.state.LeftStick.Y = -teleop.NormalizeAxis(event.Value, info.Minimum, info.Maximum)
		case absRX:
			s.state.RightStick.X = teleop.NormalizeAxis(event.Value, info.Minimum, info.Maximum)
		case absRY:
			s.state.RightStick.Y = -teleop.NormalizeAxis(event.Value, info.Minimum, info.Maximum)
		case absZ:
			s.state.LeftTrigger = teleop.NormalizeTrigger(event.Value, info.Minimum, info.Maximum)
		case absRZ:
			s.state.RightTrigger = teleop.NormalizeTrigger(event.Value, info.Minimum, info.Maximum)
		case absHat0X:
			s.state.DPad.Left = event.Value < 0
			s.state.DPad.Right = event.Value > 0
		case absHat0Y:
			s.state.DPad.Up = event.Value < 0
			s.state.DPad.Down = event.Value > 0
		}
	}
}

func (s *linuxSource) resync() error {
	keys := make([]byte, 96)
	if err := linuxIOCTLBytes(s.file.Fd(), linuxIOR('E', 0x18, uintptr(len(keys))), keys); err != nil {
		return fmt.Errorf("query controller key state: %w", err)
	}
	digital := make(map[teleop.ControlID]bool)
	for code, control := range linuxKeyControls() {
		digital[control] = digital[control] || linuxBit(keys, code)
	}
	for control, pressed := range digital {
		s.state.SetButton(control, pressed)
	}
	for code := range s.ranges {
		var info linuxAbsInfo
		if err := linuxIOCTL(
			s.file.Fd(),
			linuxIOR('E', uintptr(0x40+code), unsafe.Sizeof(info)),
			unsafe.Pointer(&info),
		); err != nil {
			continue
		}
		s.ranges[code] = info
		s.apply(linuxInputEvent{Type: evAbs, Code: code, Value: info.Value})
	}
	return nil
}

func (s *linuxSource) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.file.Close() })
	return err
}

func linuxCapabilities(file *os.File) (map[teleop.ControlID]bool, map[uint16]linuxAbsInfo, error) {
	keys := make([]byte, 96)
	if err := linuxIOCTLBytes(file.Fd(), linuxIOR('E', 0x20+evKey, uintptr(len(keys))), keys); err != nil {
		return nil, nil, err
	}
	absolute := make([]byte, 16)
	if err := linuxIOCTLBytes(file.Fd(), linuxIOR('E', 0x20+evAbs, uintptr(len(absolute))), absolute); err != nil {
		return nil, nil, err
	}
	supported := make(map[teleop.ControlID]bool)
	for code, control := range linuxKeyControls() {
		if linuxBit(keys, code) {
			supported[control] = true
		}
	}
	ranges := make(map[uint16]linuxAbsInfo)
	for _, code := range []uint16{absX, absY, absZ, absRX, absRY, absRZ, absHat0X, absHat0Y} {
		if !linuxBit(absolute, int(code)) {
			continue
		}
		var info linuxAbsInfo
		if err := linuxIOCTL(
			file.Fd(),
			linuxIOR('E', uintptr(0x40+code), unsafe.Sizeof(info)),
			unsafe.Pointer(&info),
		); err == nil {
			ranges[code] = info
		}
	}
	if _, ok := ranges[absX]; ok {
		supported[teleop.StickLeft] = true
	}
	if _, ok := ranges[absRX]; ok {
		supported[teleop.StickRight] = true
	}
	if _, ok := ranges[absZ]; ok {
		supported[teleop.TriggerLeft] = true
	}
	if _, ok := ranges[absRZ]; ok {
		supported[teleop.TriggerRight] = true
	}
	if _, ok := ranges[absHat0X]; ok {
		supported[teleop.DPadLeft] = true
		supported[teleop.DPadRight] = true
	}
	if _, ok := ranges[absHat0Y]; ok {
		supported[teleop.DPadUp] = true
		supported[teleop.DPadDown] = true
	}
	return supported, ranges, nil
}

func linuxKeyControls() map[int]teleop.ControlID {
	return map[int]teleop.ControlID{
		btnSouth:      ButtonA,
		btnEast:       ButtonB,
		btnWest:       ButtonX,
		btnNorth:      ButtonY,
		btnTL:         LeftBumper,
		btnTR:         RightBumper,
		btnThumbL:     LeftStick,
		btnThumbR:     RightStick,
		btnStart:      Menu,
		btnSelect:     View,
		btnMode:       Xbox,
		keyF12:        Share,
		keyRecord:     Share,
		btnDPadUp:     teleop.DPadUp,
		btnDPadDown:   teleop.DPadDown,
		btnDPadLeft:   teleop.DPadLeft,
		btnDPadRight:  teleop.DPadRight,
		btnGripLeft:   Paddle1,
		btnGripRight:  Paddle2,
		btnGripLeft2:  Paddle3,
		btnGripRight2: Paddle4,
	}
}

func linuxKeyControl(code uint16) (teleop.ControlID, bool) {
	control, ok := linuxKeyControls()[int(code)]
	return control, ok
}

func linuxDeviceString(fd uintptr, number uintptr) (string, error) {
	buffer := make([]byte, 256)
	if err := linuxIOCTLBytes(fd, linuxIOR('E', number, uintptr(len(buffer))), buffer); err != nil {
		return "", err
	}
	if index := bytes.IndexByte(buffer, 0); index >= 0 {
		buffer = buffer[:index]
	}
	return string(buffer), nil
}

func linuxBit(buffer []byte, bit int) bool {
	index := bit / 8
	return index >= 0 && index < len(buffer) && buffer[index]&(1<<uint(bit%8)) != 0
}

func linuxIOR(kind, number, size uintptr) uintptr {
	const (
		read      = 2
		direction = 30
		sizeShift = 16
		typeShift = 8
	)
	return uintptr(read)<<direction | size<<sizeShift | kind<<typeShift | number
}

func linuxIOCTL(fd, request uintptr, value unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}

func linuxIOCTLBytes(fd, request uintptr, value []byte) error {
	if len(value) == 0 {
		return nil
	}
	return linuxIOCTL(fd, request, unsafe.Pointer(&value[0]))
}

func linuxEventTime(event linuxInputEvent) time.Time {
	if event.Time.Sec == 0 {
		return time.Now()
	}
	return time.Unix(int64(event.Time.Sec), int64(event.Time.Usec)*1_000)
}
