//go:build linux

package xbox

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"github.com/open-ships/teleop"
)

func TestLinuxInputEventMatchesKernelABI(t *testing.T) {
	t.Parallel()

	if encoded, memory := binary.Size(linuxInputEvent{}), int(unsafe.Sizeof(linuxInputEvent{})); encoded != memory {
		t.Fatalf("encoded input_event size = %d, memory ABI size = %d", encoded, memory)
	}
}

func TestLinuxEventMapping(t *testing.T) {
	t.Parallel()

	source := linuxSource{
		ranges: map[uint16]linuxAbsInfo{
			absX:     {Minimum: -32768, Maximum: 32767},
			absY:     {Minimum: -32768, Maximum: 32767},
			absZ:     {Minimum: 0, Maximum: 1023},
			absHat0X: {Minimum: -1, Maximum: 1},
		},
	}
	source.apply(linuxInputEvent{Type: evKey, Code: btnSouth, Value: 1})
	source.apply(linuxInputEvent{Type: evKey, Code: keyRecord, Value: 1})
	source.apply(linuxInputEvent{Type: evKey, Code: btnGripLeft, Value: 1})
	source.apply(linuxInputEvent{Type: evAbs, Code: absX, Value: 32767})
	source.apply(linuxInputEvent{Type: evAbs, Code: absY, Value: -32768})
	source.apply(linuxInputEvent{Type: evAbs, Code: absZ, Value: 1023})
	source.apply(linuxInputEvent{Type: evAbs, Code: absHat0X, Value: -1})

	if !source.state.Button(ButtonA) ||
		!source.state.Button(Share) ||
		!source.state.Button(Paddle1) {
		t.Fatalf("buttons = %#v", source.state.Buttons)
	}
	if source.state.LeftStick.X != 1 || source.state.LeftStick.Y != 1 {
		t.Fatalf("left stick = %#v", source.state.LeftStick)
	}
	if source.state.LeftTrigger != 1 {
		t.Fatalf("left trigger = %f", source.state.LeftTrigger)
	}
	if !source.state.Button(teleop.DPadLeft) {
		t.Fatalf("dpad = %#v", source.state.DPad)
	}
}
