//go:build windows

package xbox

import (
	"encoding/binary"
	"errors"
	"syscall"
	"testing"
	"unsafe"

	"github.com/open-ships/teleop"
)

func TestXInputStructsMatchWindowsABI(t *testing.T) {
	t.Parallel()

	if got := binary.Size(xinputGamepad{}); got != 12 {
		t.Fatalf("encoded XINPUT_GAMEPAD size = %d, want 12", got)
	}
	if got := unsafe.Sizeof(xinputGamepad{}); got != 12 {
		t.Fatalf("memory XINPUT_GAMEPAD size = %d, want 12", got)
	}
	if got := binary.Size(xinputState{}); got != 16 {
		t.Fatalf("encoded XINPUT_STATE size = %d, want 16", got)
	}
	if got := unsafe.Sizeof(xinputState{}); got != 16 {
		t.Fatalf("memory XINPUT_STATE size = %d, want 16", got)
	}
	if got := unsafe.Offsetof(xinputState{}.Gamepad); got != 4 {
		t.Fatalf("XINPUT_STATE.Gamepad offset = %d, want 4", got)
	}
	if got := unsafe.Sizeof(xinputVibration{}); got != 4 {
		t.Fatalf("XINPUT_VIBRATION size = %d, want 4", got)
	}
	if got := unsafe.Sizeof(xinputCapabilities{}); got != 20 {
		t.Fatalf("XINPUT_CAPABILITIES size = %d, want 20", got)
	}
}

func TestEncodeXInputState(t *testing.T) {
	t.Parallel()

	state := xinputState{
		PacketNumber: 0x01020304,
		Gamepad: xinputGamepad{
			Buttons:      0x1000,
			LeftTrigger:  1,
			RightTrigger: 2,
			ThumbLX:      -32768,
			ThumbLY:      32767,
			ThumbRX:      -1,
			ThumbRY:      0,
		},
	}
	want := []byte{
		0x04, 0x03, 0x02, 0x01,
		0x00, 0x10, 0x01, 0x02,
		0x00, 0x80, 0xff, 0x7f,
		0xff, 0xff, 0x00, 0x00,
	}
	got := encodeXInputState(state)
	if string(got) != string(want) {
		t.Fatalf("encoded state = % x, want % x", got, want)
	}
}

func TestXInputMapping(t *testing.T) {
	t.Parallel()

	state := xinputTeleopState(xinputGamepad{
		Buttons: xinputA | xinputB | xinputX | xinputY |
			xinputLeftShoulder | xinputRightShoulder |
			xinputLeftThumb | xinputRightThumb |
			xinputStart | xinputBack |
			xinputDPadUp | xinputDPadDown | xinputDPadLeft | xinputDPadRight,
		LeftTrigger:  255,
		RightTrigger: 128,
		ThumbLX:      32767,
		ThumbLY:      -32768,
		ThumbRX:      -16384,
		ThumbRY:      16384,
	})
	if !state.Button(ButtonA) ||
		!state.Button(ButtonB) ||
		!state.Button(ButtonX) ||
		!state.Button(ButtonY) ||
		!state.Button(LeftBumper) ||
		!state.Button(RightBumper) ||
		!state.Button(LeftStick) ||
		!state.Button(RightStick) ||
		!state.Button(Menu) ||
		!state.Button(View) ||
		!state.Button(teleop.DPadUp) ||
		!state.Button(teleop.DPadDown) ||
		!state.Button(teleop.DPadLeft) ||
		!state.Button(teleop.DPadRight) {
		t.Fatalf("buttons = %#v, dpad = %#v", state.Buttons, state.DPad)
	}
	if state.LeftTrigger != 1 {
		t.Fatalf("left trigger = %f", state.LeftTrigger)
	}
	if state.LeftStick.X != 1 || state.LeftStick.Y != -1 {
		t.Fatalf("left stick = %#v", state.LeftStick)
	}
	if state.RightStick.X != -0.5 {
		t.Fatalf("right stick X = %f, want -0.5", state.RightStick.X)
	}
	if state.RightStick.Y <= 0.5 || state.RightStick.Y >= 0.501 {
		t.Fatalf("right stick Y = %f, want approximately 0.500015", state.RightStick.Y)
	}
}

func TestNormalizeXInputAxisEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value int16
		want  float32
	}{
		{value: -32768, want: -1},
		{value: 0, want: 0},
		{value: 32767, want: 1},
	}
	for _, test := range tests {
		if got := normalizeXInputAxis(test.value); got != test.want {
			t.Errorf("normalizeXInputAxis(%d) = %f, want %f", test.value, got, test.want)
		}
	}
}

func TestXInputRumbleMapping(t *testing.T) {
	t.Parallel()

	got := xinputRumble(teleop.Rumble{
		LowFrequency:  1,
		HighFrequency: 0.5,
	})
	if got.LeftMotorSpeed != 65535 || got.RightMotorSpeed != 32768 {
		t.Fatalf("XINPUT_VIBRATION = %#v, want left 65535 and right 32768", got)
	}
	if got := xinputRumble(teleop.Rumble{}); got != (xinputVibration{}) {
		t.Fatalf("zero rumble = %#v, want zero vibration", got)
	}
}

func TestWindowsDescriptorReportsRumbleCapability(t *testing.T) {
	t.Parallel()

	if !windowsDescriptor(0, true).Capability.Rumble {
		t.Fatal("XInput descriptor does not advertise rumble")
	}
	if windowsDescriptor(0, false).Capability.Rumble {
		t.Fatal("XInput descriptor advertises unavailable rumble")
	}
}

func TestParseXInputID(t *testing.T) {
	t.Parallel()

	for slot := uint32(0); slot < 4; slot++ {
		id := teleop.DeviceID("xinput:" + string(rune('0'+slot)))
		got, err := parseXInputID(id)
		if err != nil || got != slot {
			t.Errorf("parseXInputID(%q) = %d, %v; want %d, nil", id, got, err, slot)
		}
	}
	for _, id := range []teleop.DeviceID{"", "0", "xinput:", "xinput:-1", "xinput:00", "xinput:4", "other:0"} {
		if _, err := parseXInputID(id); !errors.Is(err, teleop.ErrUnavailable) {
			t.Errorf("parseXInputID(%q) error = %v, want ErrUnavailable", id, err)
		}
	}
}

func TestXInputResultError(t *testing.T) {
	t.Parallel()

	if err := xinputResultError(0); err != nil {
		t.Fatalf("success result error = %v", err)
	}
	if err := xinputResultError(errorDeviceNotConnected); !errors.Is(err, teleop.ErrDisconnected) {
		t.Fatalf("disconnect result error = %v, want ErrDisconnected", err)
	}
	const unexpected = uintptr(87)
	err := xinputResultError(unexpected)
	if !errors.Is(err, syscall.Errno(unexpected)) {
		t.Fatalf("unexpected result error = %v, want errno %d", err, unexpected)
	}
	if !errors.Is(err, teleop.ErrUnavailable) {
		t.Fatalf("unexpected result error = %v, want ErrUnavailable", err)
	}
	if errors.Is(err, teleop.ErrDisconnected) {
		t.Fatalf("unexpected result normalized as disconnected: %v", err)
	}
}

func TestXInputPacketGap(t *testing.T) {
	t.Parallel()

	if gap := xinputPacketGap(^uint32(0), 10); gap != nil {
		t.Fatalf("initial packet gap = %#v, want nil", gap)
	}
	if gap := xinputPacketGap(10, 11); gap != nil {
		t.Fatalf("adjacent packet gap = %#v, want nil", gap)
	}
	gap := xinputPacketGap(10, 14)
	if gap == nil || gap.Dropped != 3 {
		t.Fatalf("packet discontinuity = %#v, want three missed updates", gap)
	}
	gap = xinputPacketGap(^uint32(0)-1, 1)
	if gap == nil || gap.Dropped != 2 {
		t.Fatalf("wrapped packet discontinuity = %#v, want two missed updates", gap)
	}
}
