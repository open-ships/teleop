//go:build linux

package generic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"github.com/open-ships/teleop"
)

type linuxID struct{ Bus, Vendor, Product, Version uint16 }
type linuxAbs struct{ Value, Minimum, Maximum, Fuzz, Flat, Resolution int32 }

func linuxIOCTL(file *os.File, request uintptr, value unsafe.Pointer) error {
	raw, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	err = raw.Control(func(fd uintptr) {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(value))
		if errno != 0 {
			callErr = errno
		}
	})
	return errors.Join(err, callErr)
}
func linuxError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("%w: %v", teleop.ErrPermission, err)
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENODEV), errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.EIO):
		return fmt.Errorf("%w: %v", teleop.ErrDisconnected, err)
	default:
		return err
	}
}
func linuxBit(bits []byte, bit int) bool {
	return bit >= 0 && bit/8 < len(bits) && bits[bit/8]&(1<<uint(bit%8)) != 0
}

func discoverPlatform(ctx context.Context) ([]Device, error) {
	stable, _ := filepath.Glob("/dev/input/by-id/*-event-joystick")
	fallback, err := filepath.Glob("/dev/input/event*")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var devices []Device
	var permission error
	for _, path := range append(stable, fallback...) {
		if err := ctx.Err(); err != nil {
			return devices, err
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		source, err := openPlatform(ctx, teleop.DeviceID(path))
		if err != nil {
			if errors.Is(err, teleop.ErrPermission) {
				permission = err
			}
			continue
		}
		d := source.Device()
		_ = source.Close()
		if !xboxOwned(d.Descriptor) {
			devices = append(devices, d)
		}
	}
	if len(devices) == 0 && permission != nil {
		return nil, permission
	}
	return devices, nil
}

func openPlatform(ctx context.Context, id teleop.DeviceID) (RawSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := string(id)
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, "/dev/input/") {
		return nil, fmt.Errorf("%w: invalid evdev path", teleop.ErrUnavailable)
	}
	file, err := os.Open(clean)
	if err != nil {
		return nil, linuxError(err)
	}
	success := false
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()
	var identity linuxID
	if err := linuxIOCTL(file, linuxIOR('E', 2, unsafe.Sizeof(identity)), unsafe.Pointer(&identity)); err != nil {
		return nil, linuxError(err)
	}
	name := make([]byte, 256)
	if err := linuxIOCTL(file, linuxIOR('E', 6, uintptr(len(name))), unsafe.Pointer(&name[0])); err != nil {
		return nil, err
	}
	end := slices.Index(name, byte(0))
	if end >= 0 {
		name = name[:end]
	}
	keys := make([]byte, 96)
	if err := linuxIOCTL(file, linuxIOR('E', 0x21, uintptr(len(keys))), unsafe.Pointer(&keys[0])); err != nil {
		return nil, err
	}
	axes := make([]byte, 8)
	if err := linuxIOCTL(file, linuxIOR('E', 0x23, uintptr(len(axes))), unsafe.Pointer(&axes[0])); err != nil {
		return nil, err
	}
	transport := teleop.TransportUnknown
	switch identity.Bus {
	case 3:
		transport = teleop.TransportUSB
	case 5:
		transport = teleop.TransportBluetooth
	}
	device := Device{Descriptor: teleop.Descriptor{ID: id, Type: ControllerType, Name: strings.TrimSpace(string(name)), Transport: transport, Backend: "linux-evdev-generic", VendorID: identity.Vendor, ProductID: identity.Product, Capability: teleop.Capabilities{AuditGrade: teleop.AuditSampledState}, Properties: map[string]string{"transport_health_scope": "fresh EVIOCGID ioctl on retained evdev attachment; not physical link response", "audit_note": "evdev key and axis state sampled every 8 ms; transitions between samples may be missed"}}}
	if device.Descriptor.Name == "" {
		device.Descriptor.Name = "evdev gamepad"
	}
	var buttonCodes, axisCodes []int
	// Exclude keyboards, mice and non-gamepad collections even if they expose axes.
	for code := 0x120; code < 0x140; code++ {
		if linuxBit(keys, code) {
			buttonCodes = append(buttonCodes, code)
		}
	}
	if len(buttonCodes) < 4 {
		return nil, fmt.Errorf("%w: not a digital gamepad", teleop.ErrUnsupported)
	}
	for code := 0x220; code <= 0x223; code++ {
		if linuxBit(keys, code) {
			buttonCodes = append(buttonCodes, code)
		}
	}
	for _, code := range buttonCodes {
		device.Controls = append(device.Controls, RawControl{ID: fmt.Sprintf("key:%d", code), Kind: RawButton, Minimum: 0, Maximum: 1})
	}
	for code := 0; code < 64; code++ {
		if !linuxBit(axes, code) {
			continue
		}
		var info linuxAbs
		if err := linuxIOCTL(file, linuxIOR('E', uintptr(0x40+code), unsafe.Sizeof(info)), unsafe.Pointer(&info)); err != nil {
			return nil, err
		}
		if info.Maximum <= info.Minimum {
			continue
		}
		axisCodes = append(axisCodes, code)
		device.Controls = append(device.Controls, RawControl{ID: fmt.Sprintf("abs:%d", code), Kind: RawAxis, Minimum: int64(info.Minimum), Maximum: int64(info.Maximum)})
	}
	source := &sampledDevice{device: device, close: file.Close}
	source.poll = func() (RawState, error) {
		var currentID linuxID
		if err := linuxIOCTL(file, linuxIOR('E', 2, unsafe.Sizeof(currentID)), unsafe.Pointer(&currentID)); err != nil {
			return nil, linuxError(err)
		}
		if currentID != identity {
			return nil, teleop.ErrDisconnected
		}
		values := make([]byte, 96)
		if err := linuxIOCTL(file, linuxIOR('E', 0x18, uintptr(len(values))), unsafe.Pointer(&values[0])); err != nil {
			return nil, linuxError(err)
		}
		state := make(RawState, len(device.Controls))
		for _, code := range buttonCodes {
			var v int64
			if linuxBit(values, code) {
				v = 1
			}
			state[fmt.Sprintf("key:%d", code)] = v
		}
		for _, code := range axisCodes {
			var info linuxAbs
			if err := linuxIOCTL(file, linuxIOR('E', uintptr(0x40+code), unsafe.Sizeof(info)), unsafe.Pointer(&info)); err != nil {
				return nil, linuxError(err)
			}
			state[fmt.Sprintf("abs:%d", code)] = int64(info.Value)
		}
		return state, nil
	}
	success = true
	return source, nil
}
