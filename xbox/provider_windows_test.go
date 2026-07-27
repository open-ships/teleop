//go:build windows

package xbox

import (
	"testing"

	"github.com/open-ships/teleop"
)

func TestXInputMapping(t *testing.T) {
	t.Parallel()

	state := xinputTeleopState(xinputGamepad{
		Buttons:      xinputA | xinputLeftShoulder | xinputStart | xinputDPadUp,
		LeftTrigger:  255,
		RightTrigger: 128,
		ThumbLX:      32767,
		ThumbLY:      -32768,
	})
	if !state.Button(ButtonA) ||
		!state.Button(LeftBumper) ||
		!state.Button(Menu) ||
		!state.Button(teleop.DPadUp) {
		t.Fatalf("buttons = %#v, dpad = %#v", state.Buttons, state.DPad)
	}
	if state.LeftTrigger != 1 {
		t.Fatalf("left trigger = %f", state.LeftTrigger)
	}
	if state.LeftStick.X != 1 || state.LeftStick.Y != -1 {
		t.Fatalf("left stick = %#v", state.LeftStick)
	}
}
