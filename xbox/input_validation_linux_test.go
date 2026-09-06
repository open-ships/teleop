//go:build linux

package xbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"syscall"
	"testing"
	"unsafe"

	"github.com/open-ships/teleop"
)

func TestLinuxReadRejectsInvalidRawControls(t *testing.T) {
	for _, test := range []struct {
		name  string
		event linuxInputEvent
		info  linuxAbsInfo
	}{
		{"gross axis corruption", linuxInputEvent{Type: evAbs, Code: absX, Value: 2147483647}, linuxAbsInfo{Minimum: -32768, Maximum: 32767}},
		{"below calibrated range", linuxInputEvent{Type: evAbs, Code: absX, Value: -32769}, linuxAbsInfo{Minimum: -32768, Maximum: 32767}},
		{"above calibrated range", linuxInputEvent{Type: evAbs, Code: absX, Value: 32768}, linuxAbsInfo{Minimum: -32768, Maximum: 32767}},
		{"reversed calibration", linuxInputEvent{Type: evAbs, Code: absX}, linuxAbsInfo{Minimum: 1, Maximum: -1}},
		{"zero width calibration", linuxInputEvent{Type: evAbs, Code: absX}, linuxAbsInfo{}},
		{"trigger corruption", linuxInputEvent{Type: evAbs, Code: absZ, Value: 1024}, linuxAbsInfo{Minimum: 0, Maximum: 1023}},
		{"hat corruption", linuxInputEvent{Type: evAbs, Code: absHat0X, Value: 2}, linuxAbsInfo{Minimum: -1, Maximum: 1}},
		{"invalid digital value", linuxInputEvent{Type: evKey, Code: btnSouth, Value: -1}, linuxAbsInfo{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &linuxSource{ranges: map[uint16]linuxAbsInfo{test.event.Code: test.info}}
			if err := binary.Write(&source.pending, binary.NativeEndian, test.event); err != nil {
				t.Fatal(err)
			}
			observation, err := source.Read(context.Background())
			if !errors.Is(err, teleop.ErrInvalidState) {
				t.Fatalf("Read returned apparent input %+v, err=%v", observation, err)
			}
			if !reflect.DeepEqual(observation, teleop.Observation{}) {
				t.Fatalf("invalid input exposed: %+v", observation)
			}
		})
	}
}

func TestLinuxResyncIsAtomicOnRequiredAxisFailure(t *testing.T) {
	for _, invalidValue := range []bool{false, true} {
		t.Run(map[bool]string{false: "ioctl failure", true: "invalid raw state"}[invalidValue], func(t *testing.T) {
			source := &linuxSource{
				ranges: map[uint16]linuxAbsInfo{absX: {Minimum: -32768, Maximum: 32767}},
				state:  teleop.State{LeftStick: teleop.Stick{X: 0.5}, Buttons: teleop.Buttons{FaceSouth: true}},
			}
			before := source.state.Clone()
			source.queryState = func(request uintptr, output unsafe.Pointer) error {
				if request == linuxIOR('E', 0x18, 96) {
					return nil
				}
				if invalidValue {
					*(*linuxAbsInfo)(output) = linuxAbsInfo{Minimum: -32768, Maximum: 32767, Value: 2147483647}
					return nil
				}
				return syscall.EIO
			}
			if err := source.resync(); err == nil {
				t.Fatal("partial state accepted")
			}
			if !reflect.DeepEqual(source.state, before) {
				t.Fatalf("partial resync changed state: %+v", source.state)
			}
			if source.ranges[absX].Value != 0 {
				t.Fatal("failed resync changed calibration")
			}
			// The recovery Read must fail too, rather than publishing a partial
			// neutral observation after SYN_DROPPED.
			for _, event := range []linuxInputEvent{{Type: evSyn, Code: synDropped}, {Type: evSyn, Code: synReport}} {
				if err := binary.Write(&source.pending, binary.NativeEndian, event); err != nil {
					t.Fatal(err)
				}
			}
			if observation, err := source.Read(context.Background()); err == nil || !reflect.DeepEqual(observation, teleop.Observation{}) {
				t.Fatalf("recovery exposed partial state: %+v err=%v", observation, err)
			}
		})
	}
}

func TestLinuxResyncRetainsCalibratedEndpointEvidence(t *testing.T) {
	source := &linuxSource{ranges: map[uint16]linuxAbsInfo{absX: {}}, initial: true}
	source.queryState = func(request uintptr, output unsafe.Pointer) error {
		if request != linuxIOR('E', 0x18, 96) {
			*(*linuxAbsInfo)(output) = linuxAbsInfo{Minimum: -32768, Maximum: 32767, Value: 32767}
		}
		return nil
	}
	if err := source.resync(); err != nil {
		t.Fatal(err)
	}
	observation, err := source.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.State.LeftStick.X != 1 || observation.Native.Fields["axis.0.value"] != 32767 {
		t.Fatalf("valid endpoint or native calibration lost: %+v", observation)
	}
}

func FuzzLinuxRawAxisRead(f *testing.F) {
	f.Add(int32(0), int32(-32768), int32(32767))
	f.Add(int32(2147483647), int32(-32768), int32(32767))
	f.Fuzz(func(t *testing.T, value, minimum, maximum int32) {
		source := &linuxSource{ranges: map[uint16]linuxAbsInfo{absX: {Minimum: minimum, Maximum: maximum}}}
		var pending bytes.Buffer
		for _, event := range []linuxInputEvent{{Type: evAbs, Code: absX, Value: value}, {Type: evSyn, Code: synReport}} {
			if err := binary.Write(&pending, binary.NativeEndian, event); err != nil {
				t.Fatal(err)
			}
		}
		source.pending = pending
		observation, err := source.Read(context.Background())
		valid := maximum > minimum && value >= minimum && value <= maximum
		if valid != (err == nil) {
			t.Fatalf("raw %d range [%d,%d] observation=%+v err=%v", value, minimum, maximum, observation, err)
		}
		if valid && (observation.State.LeftStick.X < -1 || observation.State.LeftStick.X > 1) {
			t.Fatal("normalization escaped its range")
		}
	})
}
