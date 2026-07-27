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
	index, err := parseDarwinID(id)
	if err != nil {
		return nil, err
	}
	descriptor, ok := darwinDescriptor(index)
	if !ok {
		return nil, fmt.Errorf("%w: %s", teleop.ErrUnavailable, id)
	}
	handle := C.teleop_gc_open(C.int(index))
	if handle == nil {
		return nil, fmt.Errorf("%w: open %s", teleop.ErrUnavailable, id)
	}
	return &darwinSource{
		handle:     handle,
		descriptor: descriptor,
	}, nil
}

func darwinDescriptor(index int) (teleop.Descriptor, bool) {
	name := make([]byte, 256)
	var features C.uint32_t
	ok := C.teleop_gc_info(
		C.int(index),
		(*C.char)(unsafe.Pointer(&name[0])),
		C.size_t(len(name)),
		&features,
	)
	if ok == 0 {
		return teleop.Descriptor{}, false
	}
	if end := bytes.IndexByte(name, 0); end >= 0 {
		name = name[:end]
	}
	lowerName := strings.ToLower(string(name))
	if !strings.Contains(lowerName, "xbox") &&
		!strings.Contains(lowerName, "x-box") &&
		!strings.Contains(lowerName, "microsoft") {
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
	mask := uint32(features)
	supported[Menu] = mask&1 != 0
	supported[View] = mask&2 != 0
	supported[Xbox] = mask&4 != 0
	supported[Share] = mask&8 != 0
	supported[LeftStick] = mask&16 != 0
	supported[RightStick] = mask&32 != 0
	return teleop.Descriptor{
		ID:         teleop.DeviceID(fmt.Sprintf("gamecontroller:%d", index)),
		Type:       teleop.ControllerXbox,
		Name:       string(name),
		Transport:  teleop.TransportUnknown,
		Backend:    "darwin-gamecontroller",
		Capability: capabilities(teleop.AuditExactBackendStream, supported),
		Properties: map[string]string{
			"gamecontroller_index": strconv.Itoa(index),
		},
	}, true
}

type darwinSource struct {
	mu         sync.Mutex
	handle     unsafe.Pointer
	descriptor teleop.Descriptor
	closed     bool
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
		s.mu.Unlock()

		var native C.teleop_gc_state
		result := int(C.teleop_gc_next(handle, &native, 100))
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
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	C.teleop_gc_close(s.handle)
	return nil
}

func parseDarwinID(id teleop.DeviceID) (int, error) {
	value := strings.TrimPrefix(string(id), "gamecontroller:")
	index, err := strconv.Atoi(value)
	if err != nil || index < 0 {
		return 0, fmt.Errorf("%w: invalid Game Controller device ID %q", teleop.ErrUnavailable, id)
	}
	return index, nil
}
