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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := source.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Read error = %v, want context.DeadlineExceeded", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
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
