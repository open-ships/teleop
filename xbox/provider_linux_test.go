//go:build linux

package xbox

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/open-ships/teleop"
)

func TestLinuxInputEventMatchesKernelABI(t *testing.T) {
	t.Parallel()

	if encoded, memory := binary.Size(linuxInputEvent{}), int(unsafe.Sizeof(linuxInputEvent{})); encoded != memory {
		t.Fatalf("encoded input_event size = %d, memory ABI size = %d", encoded, memory)
	}
}

func TestLinuxFFEffectMatchesKernelABI(t *testing.T) {
	t.Parallel()

	wantSize := uintptr(44)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		wantSize = 48
	}
	if got := unsafe.Sizeof(linuxFFEffect{}); got != wantSize {
		t.Fatalf("ff_effect size = %d, want %d", got, wantSize)
	}
	if got := unsafe.Offsetof(linuxFFEffect{}.Data); got != 16 {
		t.Fatalf("ff_effect union offset = %d, want 16", got)
	}
}

func TestLinuxIORUsesTheHostABI(t *testing.T) {
	t.Parallel()

	want := uintptr(0x80044502)
	switch runtime.GOARCH {
	case "ppc", "ppc64", "ppc64le", "mips", "mipsle", "mips64", "mips64le":
		want = 0x40044502
	}
	if got := linuxIOR('E', 2, 4); got != want {
		t.Fatalf("EVIOCGID request = %#x, want %#x", got, want)
	}
}

func TestLinuxIOWUsesTheHostABI(t *testing.T) {
	t.Parallel()

	size := unsafe.Sizeof(linuxFFEffect{})
	want := uintptr(1)<<30 | size<<16 | uintptr('E')<<8 | 0x80
	switch runtime.GOARCH {
	case "ppc", "ppc64", "ppc64le", "mips", "mipsle", "mips64", "mips64le":
		want = uintptr(1)<<29 | size<<16 | uintptr('E')<<8 | 0x80
	}
	if got := linuxIOW('E', 0x80, size); got != want {
		t.Fatalf("EVIOCSFF request = %#x, want %#x", got, want)
	}
}

func TestLinuxRumbleEffectMapping(t *testing.T) {
	t.Parallel()

	effect := newLinuxRumbleEffect(-1, teleop.Rumble{
		LowFrequency:  1,
		HighFrequency: 0.5,
	})
	if effect.Type != ffRumble || effect.ID != -1 {
		t.Fatalf("ff_effect header = type:%#x id:%d", effect.Type, effect.ID)
	}
	if effect.Replay.Length != 0 {
		t.Fatalf("ff_effect replay length = %d, want infinite (zero)", effect.Replay.Length)
	}
	if got := binary.NativeEndian.Uint16(effect.Data[0:2]); got != 65535 {
		t.Fatalf("strong magnitude = %d, want 65535", got)
	}
	if got := binary.NativeEndian.Uint16(effect.Data[2:4]); got != 32768 {
		t.Fatalf("weak magnitude = %d, want 32768", got)
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
	source.apply(linuxInputEvent{Type: evKey, Code: btnNorth, Value: 1})
	source.apply(linuxInputEvent{Type: evKey, Code: btnWest, Value: 1})
	source.apply(linuxInputEvent{Type: evKey, Code: keyRecord, Value: 1})
	source.apply(linuxInputEvent{Type: evKey, Code: btnGripLeft, Value: 1})
	source.apply(linuxInputEvent{Type: evAbs, Code: absX, Value: 32767})
	source.apply(linuxInputEvent{Type: evAbs, Code: absY, Value: -32768})
	source.apply(linuxInputEvent{Type: evAbs, Code: absZ, Value: 1023})
	source.apply(linuxInputEvent{Type: evAbs, Code: absHat0X, Value: -1})

	if !source.state.Button(ButtonA) ||
		!source.state.Button(ButtonX) ||
		!source.state.Button(ButtonY) ||
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

func TestLinuxXPadPhysicalFaceButtonMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		code    uint16
		control teleop.ControlID
	}{
		{name: "BTN_X alias is physical X", code: btnNorth, control: ButtonX},
		{name: "BTN_Y alias is physical Y", code: btnWest, control: ButtonY},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			control, ok := linuxKeyControl(test.code)
			if !ok {
				t.Fatalf("linuxKeyControl(%#x) is unsupported", test.code)
			}
			if control != test.control {
				t.Fatalf("linuxKeyControl(%#x) = %q, want %q", test.code, control, test.control)
			}
		})
	}
}

func TestLinuxReadCancellation(t *testing.T) {
	t.Parallel()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	source := &linuxSource{file: reader}
	// Close on every exit path. A t.Fatal below would otherwise leak the
	// descriptor until finalization, and these tests run in parallel, so a
	// recycled descriptor number shows up as a spurious readiness in another
	// test rather than as a failure here.
	defer source.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := source.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Read error = %v, want context.DeadlineExceeded", err)
	}
}

func TestLinuxReadDecodesEvdevReport(t *testing.T) {
	t.Parallel()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	report := []linuxInputEvent{
		{Type: evKey, Code: btnNorth, Value: 1},
		{Type: evSyn, Code: synReport},
	}
	for _, event := range report {
		if err := binary.Write(writer, binary.NativeEndian, event); err != nil {
			t.Fatal(err)
		}
	}

	source := &linuxSource{file: reader}
	observation, err := source.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !observation.State.Button(ButtonX) || observation.State.Button(ButtonY) {
		t.Fatalf("buttons = %#v, want physical X only", observation.State.Buttons)
	}
	wantBytes := len(report) * binary.Size(linuxInputEvent{})
	if len(observation.Native.Data) != wantBytes {
		t.Fatalf("native data length = %d, want %d", len(observation.Native.Data), wantBytes)
	}
}

func TestLinuxCloseInterruptsRead(t *testing.T) {
	t.Parallel()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	source := &linuxSource{file: reader}
	result := make(chan error, 1)
	go func() {
		_, err := source.Read(context.Background())
		result <- err
	}()

	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, teleop.ErrClosed) {
			t.Fatalf("Read error = %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read remained blocked after Close")
	}
}

func TestLinuxEOFMeansDisconnected(t *testing.T) {
	t.Parallel()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	source := &linuxSource{file: reader}
	if _, err := source.Read(context.Background()); !errors.Is(err, teleop.ErrDisconnected) {
		t.Fatalf("Read error = %v, want ErrDisconnected (raw EOF: %v)", err, io.EOF)
	}
}

func TestLinuxRawFrameIsBounded(t *testing.T) {
	t.Parallel()

	source := linuxSource{}
	source.raw.Write(make([]byte, maxLinuxRawBytes))
	source.appendRaw(linuxInputEvent{Type: evKey, Code: btnSouth, Value: 1})
	if !source.dropped || source.raw.Len() != 0 {
		t.Fatalf("raw overflow state = dropped:%t bytes:%d", source.dropped, source.raw.Len())
	}
}
