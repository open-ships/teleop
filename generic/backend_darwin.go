//go:build darwin && cgo

package generic

/*
#cgo LDFLAGS: -framework CoreFoundation -framework IOKit
#include <stdlib.h>
#include "native_darwin.h"
*/
import "C"

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"github.com/open-ships/teleop"
)

func darwinError(status C.int) error {
	switch status {
	case 0:
		return nil
	case -2:
		return teleop.ErrDisconnected
	case -3:
		return teleop.ErrPermission
	default:
		return fmt.Errorf("%w: IOKit HID operation failed", teleop.ErrUnavailable)
	}
}
func darwinDescriptor(info *C.teleop_hid_device) teleop.Descriptor {
	transport := teleop.TransportUnknown
	switch strings.ToLower(C.GoString(&info.transport[0])) {
	case "usb":
		transport = teleop.TransportUSB
	case "bluetooth":
		transport = teleop.TransportBluetooth
	}
	name := strings.TrimSpace(C.GoString(&info.name[0]))
	if name == "" {
		name = "HID gamepad"
	}
	return teleop.Descriptor{
		ID: teleop.DeviceID(fmt.Sprintf("hid:%016x", uint64(info.id))), Type: ControllerType, Name: name, Backend: "darwin-iohid", Transport: transport,
		VendorID: uint16(info.vendor), ProductID: uint16(info.product),
		Capability: teleop.Capabilities{AuditGrade: teleop.AuditSampledState},
		Properties: map[string]string{"transport_health_scope": "fresh I/O Registry lookup of retained HID attachment; not physical link response", "audit_note": "HID element state sampled every 8 ms; transitions between samples may be missed"},
	}
}
func discoverPlatform(ctx context.Context) ([]Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var list *C.teleop_hid_device
	var count C.size_t
	if err := darwinError(C.teleop_hid_list(&list, &count)); err != nil {
		return nil, err
	}
	defer C.free(unsafe.Pointer(list))
	var devices []Device
	for _, item := range unsafe.Slice(list, int(count)) {
		if err := ctx.Err(); err != nil {
			return devices, err
		}
		descriptor := darwinDescriptor(&item)
		if xboxOwned(descriptor) {
			continue
		}
		raw, err := openPlatform(ctx, descriptor.ID)
		if err != nil {
			return devices, fmt.Errorf("inspect %s: %w", descriptor.Name, err)
		}
		device := raw.Device()
		_ = raw.Close()
		if len(device.Controls) > 0 {
			devices = append(devices, device)
		}
	}
	return devices, nil
}
func openPlatform(ctx context.Context, id teleop.DeviceID) (RawSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value := strings.TrimPrefix(string(id), "hid:")
	identifier, err := strconv.ParseUint(value, 16, 64)
	if err != nil || identifier == 0 || string(id) != fmt.Sprintf("hid:%016x", identifier) {
		return nil, fmt.Errorf("%w: invalid HID ID %q", teleop.ErrUnavailable, id)
	}
	var info C.teleop_hid_device
	var elements *C.teleop_hid_element
	var count C.size_t
	var status C.int
	handle := C.teleop_hid_open(C.uint64_t(identifier), &info, &elements, &count, &status)
	if handle == nil {
		return nil, darwinError(status)
	}
	defer C.free(unsafe.Pointer(elements))
	device := Device{Descriptor: darwinDescriptor(&info)}
	for _, e := range unsafe.Slice(elements, int(count)) {
		kind := RawButton
		if e.kind == 2 {
			kind = RawAxis
		}
		if e.kind == 3 {
			kind = RawHat
		}
		device.Controls = append(device.Controls, RawControl{ID: fmt.Sprintf("hid:%04x:%04x:%d", uint32(e.page), uint32(e.usage), uint32(e.cookie)), Kind: kind, Minimum: int64(e.minimum), Maximum: int64(e.maximum)})
	}
	values := make([]C.int64_t, len(device.Controls))
	source := &sampledDevice{device: device, close: func() error { C.teleop_hid_close(handle); return nil }}
	source.poll = func() (RawState, error) {
		if len(values) == 0 {
			return nil, fmt.Errorf("%w: no usable HID inputs", teleop.ErrUnsupported)
		}
		if err := darwinError(C.teleop_hid_poll(handle, &values[0], C.size_t(len(values)))); err != nil {
			return nil, err
		}
		state := make(RawState, len(values))
		for i, value := range values {
			state[device.Controls[i].ID] = int64(value)
		}
		return state, nil
	}
	return source, nil
}
