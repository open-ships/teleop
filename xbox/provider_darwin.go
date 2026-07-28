//go:build darwin && cgo

package xbox

/*
#cgo CFLAGS: -fblocks
#cgo LDFLAGS: -framework Foundation -framework GameController
#include <stdlib.h>
#include "native_darwin.h"
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/open-ships/teleop"
)

func discoverPlatform(ctx context.Context) ([]teleop.Descriptor, error) {
	count := int(C.teleop_gc_count())
	var devices []teleop.Descriptor
	for index := 0; index < count; index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		descriptor, ok := darwinDescriptor(index)
		if ok {
			devices = append(devices, descriptor)
		}
	}
	return devices, nil
}

func openPlatform(ctx context.Context, id teleop.DeviceID) (teleop.InputSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identifier, err := parseDarwinID(id)
	if err != nil {
		return nil, err
	}
	descriptor, ok := darwinDescriptorByID(identifier)
	if !ok {
		return nil, fmt.Errorf("%w: %s", teleop.ErrUnavailable, id)
	}
	handle := C.teleop_gc_open(C.uint64_t(identifier))
	if handle == nil {
		return nil, fmt.Errorf("%w: open %s", teleop.ErrUnavailable, id)
	}
	return &darwinSource{
		handle:     handle,
		descriptor: descriptor,
		closeDone:  make(chan struct{}),
	}, nil
}

func darwinDescriptor(index int) (teleop.Descriptor, bool) {
	name := make([]byte, 256)
	productCategory := make([]byte, 256)
	var identifier C.uint64_t
	var features C.uint32_t
	ok := C.teleop_gc_info(
		C.int(index),
		&identifier,
		(*C.char)(unsafe.Pointer(&name[0])),
		C.size_t(len(name)),
		(*C.char)(unsafe.Pointer(&productCategory[0])),
		C.size_t(len(productCategory)),
		&features,
	)
	if ok == 0 {
		return teleop.Descriptor{}, false
	}
	return newDarwinDescriptor(
		uint64(identifier),
		index,
		nullTerminatedString(name),
		nullTerminatedString(productCategory),
		uint32(features),
	)
}

func darwinDescriptorByID(identifier uint64) (teleop.Descriptor, bool) {
	name := make([]byte, 256)
	productCategory := make([]byte, 256)
	var features C.uint32_t
	ok := C.teleop_gc_info_by_id(
		C.uint64_t(identifier),
		(*C.char)(unsafe.Pointer(&name[0])),
		C.size_t(len(name)),
		(*C.char)(unsafe.Pointer(&productCategory[0])),
		C.size_t(len(productCategory)),
		&features,
	)
	if ok == 0 {
		return teleop.Descriptor{}, false
	}
	return newDarwinDescriptor(
		identifier,
		-1,
		nullTerminatedString(name),
		nullTerminatedString(productCategory),
		uint32(features),
	)
}

func newDarwinDescriptor(
	identifier uint64,
	index int,
	vendorName string,
	category string,
	features uint32,
) (teleop.Descriptor, bool) {
	if !isXboxIdentity(vendorName, category) {
		return teleop.Descriptor{}, false
	}
	supported := map[teleop.ControlID]bool{
		ButtonA: true, ButtonB: true, ButtonX: true, ButtonY: true,
		LeftBumper: true, RightBumper: true,
		teleop.DPadUp: true, teleop.DPadDown: true,
		teleop.DPadLeft: true, teleop.DPadRight: true,
		teleop.StickLeft: true, teleop.StickRight: true,
		teleop.TriggerLeft: true, teleop.TriggerRight: true,
	}
	mask := features
	supported[Menu] = mask&1 != 0
	supported[View] = mask&2 != 0
	supported[Xbox] = mask&4 != 0
	supported[Share] = mask&8 != 0
	supported[LeftStick] = mask&16 != 0
	supported[RightStick] = mask&32 != 0
	properties := map[string]string{
		"gamecontroller_identifier":       fmt.Sprintf("%016x", identifier),
		"gamecontroller_product_category": category,
	}
	if index >= 0 {
		properties["gamecontroller_index"] = strconv.Itoa(index)
	}
	return teleop.Descriptor{
		ID:         formatDarwinID(identifier),
		Type:       teleop.ControllerXbox,
		Name:       darwinControllerName(vendorName, category),
		Transport:  teleop.TransportUnknown,
		Backend:    "darwin-gamecontroller",
		Capability: capabilities(teleop.AuditExactBackendStream, supported),
		Properties: properties,
	}, true
}

func nullTerminatedString(value []byte) string {
	if end := bytes.IndexByte(value, 0); end >= 0 {
		value = value[:end]
	}
	return string(value)
}

func isXboxIdentity(vendorName, productCategory string) bool {
	identity := strings.ToLower(vendorName + " " + productCategory)
	return strings.Contains(identity, "xbox") ||
		strings.Contains(identity, "x-box") ||
		strings.Contains(identity, "microsoft")
}

func darwinControllerName(vendorName, productCategory string) string {
	switch strings.ToLower(strings.TrimSpace(vendorName)) {
	case "", "controller", "game controller", "gamepad":
		if productCategory != "" {
			return productCategory
		}
	}
	return vendorName
}

type darwinSource struct {
	mu         sync.Mutex
	handle     unsafe.Pointer
	descriptor teleop.Descriptor
	closed     bool
	active     sync.WaitGroup
	closeDone  chan struct{}
}

func (s *darwinSource) Descriptor() teleop.Descriptor {
	return s.descriptor.Clone()
}

func (s *darwinSource) Read(ctx context.Context) (teleop.Observation, error) {
	for {
		if err := ctx.Err(); err != nil {
			return teleop.Observation{}, err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return teleop.Observation{}, teleop.ErrClosed
		}
		handle := s.handle
		s.active.Add(1)
		s.mu.Unlock()

		var native C.teleop_gc_state
		result := int(C.teleop_gc_next(handle, &native, 100))
		s.active.Done()
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return teleop.Observation{}, teleop.ErrClosed
		}
		switch result {
		case 0:
			continue
		case -1:
			return teleop.Observation{}, teleop.ErrClosed
		case -2:
			return teleop.Observation{}, teleop.ErrDisconnected
		}

		state := teleop.State{
			Buttons: teleop.Buttons{
				FaceSouth:     native.a != 0,
				FaceEast:      native.b != 0,
				FaceWest:      native.x != 0,
				FaceNorth:     native.y != 0,
				BumperLeft:    native.left_bumper != 0,
				BumperRight:   native.right_bumper != 0,
				StickLeft:     native.left_stick != 0,
				StickRight:    native.right_stick != 0,
				MenuPrimary:   native.menu != 0,
				MenuSecondary: native.view != 0,
				System:        native.home != 0,
				Capture:       native.share != 0,
			},
			LeftStick: teleop.Stick{
				X: float32(native.left_x),
				Y: float32(native.left_y),
			},
			RightStick: teleop.Stick{
				X: float32(native.right_x),
				Y: float32(native.right_y),
			},
			LeftTrigger:  float32(native.left_trigger),
			RightTrigger: float32(native.right_trigger),
			DPad: teleop.DPad{
				Up:    native.dpad_up != 0,
				Down:  native.dpad_down != 0,
				Left:  native.dpad_left != 0,
				Right: native.dpad_right != 0,
			},
		}
		var raw bytes.Buffer
		_ = binary.Write(&raw, binary.LittleEndian, struct {
			LeftX, LeftY, RightX, RightY float32
			LeftTrigger, RightTrigger    float32
			Sequence                     uint64
		}{
			state.LeftStick.X,
			state.LeftStick.Y,
			state.RightStick.X,
			state.RightStick.Y,
			state.LeftTrigger,
			state.RightTrigger,
			uint64(native.sequence),
		})
		observation := teleop.Observation{
			State:           state,
			ObservedAt:      time.Now(),
			DeviceTimestamp: int64(float64(native.timestamp) * float64(time.Second)),
			Native: teleop.NativeInput{
				Format: "darwin-gamecontroller-state",
				Data:   raw.Bytes(),
				Fields: map[string]int64{
					"sequence": int64(native.sequence),
				},
			},
		}
		if native.dropped != 0 {
			observation.Gap = &teleop.SourceGap{
				Reason: "Game Controller callback queue overflow",
			}
		}
		return observation, nil
	}
}

func (s *darwinSource) Close() error {
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		return nil
	}
	s.closed = true
	handle := s.handle
	s.mu.Unlock()

	// teleop_gc_next uses the native handle during its timed wait. Prevent new
	// reads, wait for existing calls to return, and only then release it.
	s.active.Wait()
	C.teleop_gc_close(handle)

	s.mu.Lock()
	s.handle = nil
	close(s.closeDone)
	s.mu.Unlock()
	return nil
}

func formatDarwinID(identifier uint64) teleop.DeviceID {
	return teleop.DeviceID(fmt.Sprintf("gamecontroller:%016x", identifier))
}

func parseDarwinID(id teleop.DeviceID) (uint64, error) {
	const prefix = "gamecontroller:"
	raw := string(id)
	if !strings.HasPrefix(raw, prefix) {
		return 0, fmt.Errorf("%w: invalid Game Controller device ID %q", teleop.ErrUnavailable, id)
	}
	value := strings.TrimPrefix(raw, prefix)
	identifier, err := strconv.ParseUint(value, 16, 64)
	if err != nil ||
		identifier == 0 ||
		len(value) != 16 ||
		value != fmt.Sprintf("%016x", identifier) {
		return 0, fmt.Errorf("%w: invalid Game Controller device ID %q", teleop.ErrUnavailable, id)
	}
	return identifier, nil
}
