//go:build linux

package xbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/open-ships/teleop"
	"golang.org/x/sys/unix"
)

const (
	evSyn = 0x00
	evKey = 0x01
	evAbs = 0x03
	evFF  = 0x15

	synReport  = 0
	synDropped = 3

	absX     = 0x00
	absY     = 0x01
	absZ     = 0x02
	absRX    = 0x03
	absRY    = 0x04
	absRZ    = 0x05
	absHat0X = 0x10
	absHat0Y = 0x11

	btnSouth  = 0x130
	btnEast   = 0x131
	btnNorth  = 0x133
	btnWest   = 0x134
	btnTL     = 0x136
	btnTR     = 0x137
	btnSelect = 0x13a
	btnStart  = 0x13b
	btnMode   = 0x13c
	btnThumbL = 0x13d
	btnThumbR = 0x13e

	btnDPadUp    = 0x220
	btnDPadDown  = 0x221
	btnDPadLeft  = 0x222
	btnDPadRight = 0x223

	// Kernel 6.17 standardized these grip/paddle codes.
	btnGripLeft   = 0x224
	btnGripRight  = 0x225
	btnGripLeft2  = 0x226
	btnGripRight2 = 0x227

	keyF12    = 88
	keyRecord = 167

	busUSB       = 0x03
	busBluetooth = 0x05

	ffRumble = 0x50
	ffMax    = 0x7f

	linuxReadPollInterval = 100 * time.Millisecond
	// maxLinuxRawBytes bounds a malformed evdev frame that never reaches
	// SYN_REPORT. Native input is diagnostic data, not unbounded storage.
	maxLinuxRawBytes = 1 << 20
)

type linuxInputID struct {
	BusType uint16
	Vendor  uint16
	Product uint16
	Version uint16
}

type linuxAbsInfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

type linuxInputEvent struct {
	Time  syscall.Timeval
	Type  uint16
	Code  uint16
	Value int32
}

type linuxFFTrigger struct {
	Button   uint16
	Interval uint16
}

type linuxFFReplay struct {
	Length uint16
	Delay  uint16
}

const linuxFFEffectUnionSize = 24 + int(unsafe.Sizeof(uintptr(0)))

// linuxFFEffect mirrors Linux's struct ff_effect. Its union is represented as
// raw bytes because rumble only uses the first four; the union is 28 bytes on
// 32-bit Linux and 32 bytes on 64-bit Linux due to ff_periodic_effect's pointer.
type linuxFFEffect struct {
	Type      uint16
	ID        int16
	Direction uint16
	Trigger   linuxFFTrigger
	Replay    linuxFFReplay
	Padding   uint16
	Data      [linuxFFEffectUnionSize]byte
}

func discoverPlatform(ctx context.Context) ([]teleop.Descriptor, error) {
	paths, err := linuxInputPaths()
	if err != nil {
		return nil, err
	}
	var (
		devices       []teleop.Descriptor
		permissionErr error
	)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		descriptor, xboxDevice, err := inspectLinuxDevice(path)
		if err != nil {
			if errors.Is(err, os.ErrPermission) {
				permissionErr = err
			}
			continue
		}
		if xboxDevice {
			devices = append(devices, descriptor)
		}
	}
	if len(devices) == 0 && permissionErr != nil {
		return nil, fmt.Errorf("%w: read /dev/input devices: %v", teleop.ErrPermission, permissionErr)
	}
	return devices, nil
}

func linuxInputPaths() ([]string, error) {
	stable, err := filepath.Glob("/dev/input/by-id/*-event-joystick")
	if err != nil {
		return nil, fmt.Errorf("discover stable input paths: %w", err)
	}
	fallback, err := filepath.Glob("/dev/input/event*")
	if err != nil {
		return nil, fmt.Errorf("discover input paths: %w", err)
	}
	seen := make(map[string]bool)
	var paths []string
	for _, path := range append(stable, fallback...) {
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		if seen[realPath] {
			continue
		}
		seen[realPath] = true
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths, nil
}

func inspectLinuxDevice(path string) (teleop.Descriptor, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return teleop.Descriptor{}, false, err
	}
	defer file.Close()

	name, err := linuxDeviceString(file.Fd(), 0x06)
	if err != nil {
		return teleop.Descriptor{}, false, err
	}
	lowerName := strings.ToLower(name)
	if !strings.Contains(lowerName, "xbox") &&
		!strings.Contains(lowerName, "x-box") &&
		!strings.Contains(lowerName, "microsoft") {
		return teleop.Descriptor{}, false, nil
	}

	var inputID linuxInputID
	if err := linuxIOCTL(file.Fd(), linuxIOR('E', 0x02, unsafe.Sizeof(inputID)), unsafe.Pointer(&inputID)); err != nil {
		return teleop.Descriptor{}, false, err
	}
	transport := teleop.TransportUnknown
	switch inputID.BusType {
	case busBluetooth:
		transport = teleop.TransportBluetooth
	case busUSB:
		transport = teleop.TransportUSB
	}
	realPath, _ := filepath.EvalSymlinks(path)
	supported, _, err := linuxCapabilities(file)
	if err != nil {
		return teleop.Descriptor{}, false, err
	}
	if !supported[ButtonA] || !supported[teleop.StickLeft] {
		return teleop.Descriptor{}, false, nil
	}
	capability := capabilities(teleop.AuditExactBackendStream, supported)
	capability.Rumble = linuxRumbleSupported(file)
	return teleop.Descriptor{
		ID:        teleop.DeviceID(path),
		Type:      teleop.ControllerXbox,
		Name:      name,
		Transport: transport,
		Backend:   "linux-evdev",
		VendorID:  inputID.Vendor,
		ProductID: inputID.Product,
		Properties: map[string]string{
			"path":                   path,
			"real_path":              realPath,
			"transport_health_scope": "fresh EVIOCGID evdev attachment check; not physical link response",
		},
		Capability: capability,
	}, true, nil
}

func openPlatform(ctx context.Context, id teleop.DeviceID) (teleop.InputSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := string(id)
	descriptor, xboxDevice, err := inspectLinuxDevice(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w: open %s: %v", teleop.ErrPermission, path, err)
		}
		return nil, err
	}
	if !xboxDevice {
		return nil, fmt.Errorf("%w: %s is not an Xbox controller", teleop.ErrUnavailable, path)
	}
	var file *os.File
	if descriptor.Capability.Rumble {
		file, err = os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			// Input remains useful when the process has read permission but not
			// the write permission required by force feedback.
			file, err = os.Open(path)
			descriptor.Capability.Rumble = false
		}
	} else {
		file, err = os.Open(path)
	}
	if err != nil {
		return nil, err
	}
	_, ranges, err := linuxCapabilities(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	source := &linuxSource{
		file:         file,
		descriptor:   descriptor,
		ranges:       ranges,
		initial:      true,
		rumbleEffect: -1,
	}
	source.probeAttachment = func() error {
		return linuxProbeAttachment(file)
	}
	if err := source.resync(); err != nil {
		file.Close()
		return nil, err
	}
	// Establish the capability before ingest begins. A busy controller may keep
	// poll continuously readable and therefore never reach the timeout branch
	// that performs periodic silence probes.
	if err := source.checkEvdevAttachment(); err != nil {
		file.Close()
		return nil, fmt.Errorf("initial evdev attachment probe: %w", err)
	}
	return source, nil
}

type linuxSource struct {
	file       *os.File
	descriptor teleop.Descriptor
	ranges     map[uint16]linuxAbsInfo
	state      teleop.State
	raw        bytes.Buffer
	dropped    bool
	initial    bool
	closeOnce  sync.Once
	closeMu    sync.RWMutex
	closed     bool
	closeErr   error
	healthMu   sync.RWMutex
	health     teleop.TransportHealth
	// probeAttachment performs a fresh ioctl against the retained evdev file.
	// It checks only that the kernel still recognizes that evdev attachment;
	// it is not a challenge/response with a USB or wireless controller.
	probeAttachment func() error
	rumbleMu        sync.Mutex
	rumbleEffect    int16

	// pending holds bytes read from the device that do not yet form a complete
	// event. The kernel can split an event across reads, so a decoder that
	// assumed each read yielded whole events would either lose the remainder
	// or block waiting for it.
	pending bytes.Buffer
	scratch []byte
}

func (s *linuxSource) Descriptor() teleop.Descriptor {
	return s.descriptor.Clone()
}

func (s *linuxSource) TransportHealth() teleop.TransportHealth {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()
	return s.health
}

func (s *linuxSource) updateTransportHealth(
	checkedAt time.Time,
	connected bool,
	verifiable bool,
) {
	s.healthMu.Lock()
	s.health.Sequence++
	s.health.CheckedAt = checkedAt
	s.health.Connected = connected
	s.health.SilenceVerifiable = verifiable
	s.healthMu.Unlock()
}

func linuxProbeAttachment(file *os.File) error {
	if file == nil {
		return os.ErrInvalid
	}
	var inputID linuxInputID
	return linuxIOCTL(
		file.Fd(),
		linuxIOR('E', 0x02, unsafe.Sizeof(inputID)),
		unsafe.Pointer(&inputID),
	)
}

func (s *linuxSource) checkEvdevAttachment() error {
	if s.probeAttachment == nil {
		// A test or alternative construction without a real evdev probe must not
		// turn timer expiry into a verifiable health claim.
		s.healthMu.Lock()
		s.health.SilenceVerifiable = false
		s.healthMu.Unlock()
		return nil
	}
	err := s.probeAttachment()
	checkedAt := time.Now()
	s.closeMu.RLock()
	closed := s.closed
	if err == nil {
		s.updateTransportHealth(checkedAt, !closed, true)
		s.closeMu.RUnlock()
		if closed {
			return teleop.ErrClosed
		}
		return nil
	}
	knownOutcome := closed ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, syscall.EBADF) ||
		errors.Is(err, syscall.ENODEV) ||
		errors.Is(err, syscall.ENXIO) ||
		errors.Is(err, syscall.EIO)
	s.updateTransportHealth(checkedAt, false, knownOutcome)
	s.closeMu.RUnlock()
	return s.normalizeReadError(err)
}

func (s *linuxSource) Read(ctx context.Context) (teleop.Observation, error) {
	if err := ctx.Err(); err != nil {
		return teleop.Observation{}, err
	}
	if s.isClosed() {
		return teleop.Observation{}, teleop.ErrClosed
	}
	if s.initial {
		s.initial = false
		return teleop.Observation{
			State:      s.state.Clone(),
			ObservedAt: time.Now(),
			Native: teleop.NativeInput{
				Format: "linux-evdev-initial-state",
			},
		}, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return teleop.Observation{}, err
		}
		event, err := s.nextEvent(ctx)
		if err != nil {
			return teleop.Observation{}, err
		}
		s.appendRaw(event)

		if event.Type == evSyn && event.Code == synDropped {
			s.dropped = true
			continue
		}
		if s.dropped {
			if event.Type == evSyn && event.Code == synReport {
				if err := s.resync(); err != nil {
					return teleop.Observation{}, s.normalizeReadError(err)
				}
				raw := slices.Clone(s.raw.Bytes())
				s.raw.Reset()
				s.dropped = false
				return teleop.Observation{
					State:           s.state.Clone(),
					ObservedAt:      linuxEventTime(event),
					DeviceTimestamp: int64(event.Time.Sec)*1_000_000_000 + int64(event.Time.Usec)*1_000,
					Native: teleop.NativeInput{
						Format: "linux-evdev",
						Data:   raw,
					},
					Gap: &teleop.SourceGap{
						Dropped: 1, // SYN_DROPPED proves at least one event was lost.
						Reason:  "evdev SYN_DROPPED buffer overrun",
					},
				}, nil
			}
			continue
		}

		s.apply(event)
		if event.Type == evSyn && event.Code == synReport {
			raw := slices.Clone(s.raw.Bytes())
			s.raw.Reset()
			return teleop.Observation{
				State:           s.state.Clone(),
				ObservedAt:      linuxEventTime(event),
				DeviceTimestamp: int64(event.Time.Sec)*1_000_000_000 + int64(event.Time.Usec)*1_000,
				Native: teleop.NativeInput{
					Format: "linux-evdev",
					Data:   raw,
				},
			}, nil
		}
	}
}

// linuxReadBatchEvents bounds one read syscall. Reading several events at once
// keeps syscall overhead low at high report rates without letting a burst
// allocate without limit.
const linuxReadBatchEvents = 64

// nextEvent returns the next complete evdev event.
//
// Poll readiness guarantees that at least one byte can be read, not that a
// whole event is available. Demanding a fixed-size read after a readiness
// signal, as io.ReadFull does, blocks in a syscall that neither the context
// nor Close can interrupt until the remainder of a split event arrives — and
// for a partial event at the tail of a kernel buffer, that can be never.
//
// Reading whatever is available and decoding only complete records keeps poll
// the single blocking point, so cancellation and Close stay effective.
func (s *linuxSource) nextEvent(ctx context.Context) (linuxInputEvent, error) {
	size := binary.Size(linuxInputEvent{})
	if size <= 0 {
		return linuxInputEvent{}, fmt.Errorf("evdev event has no fixed wire size")
	}
	for {
		if s.pending.Len() >= size {
			var event linuxInputEvent
			if _, err := binary.Decode(
				s.pending.Next(size),
				binary.NativeEndian,
				&event,
			); err != nil {
				return linuxInputEvent{}, fmt.Errorf("decode evdev event: %w", err)
			}
			return event, nil
		}
		if err := ctx.Err(); err != nil {
			return linuxInputEvent{}, err
		}
		if err := s.waitReadable(ctx); err != nil {
			return linuxInputEvent{}, err
		}
		if s.scratch == nil {
			s.scratch = make([]byte, size*linuxReadBatchEvents)
		}
		// Bound the read when the descriptor supports deadlines. This is best
		// effort: an evdev device does not, because the capability ioctls need
		// a raw descriptor and take it out of the runtime poller. Clearing a
		// stale deadline matters as much as setting one, since a deadline left
		// over from an earlier canceled call would fire against a live read.
		if deadline, ok := ctx.Deadline(); ok {
			_ = s.file.SetReadDeadline(deadline)
		} else {
			_ = s.file.SetReadDeadline(time.Time{})
		}
		count, err := s.file.Read(s.scratch)
		if count > 0 {
			s.pending.Write(s.scratch[:count])
		}
		if err != nil {
			// Bytes that completed an event are still usable; report the error
			// only once the buffered remainder is exhausted.
			if s.pending.Len() >= size {
				continue
			}
			// The deadline was derived from ctx, so its expiry is a
			// cancellation rather than a device fault. ctx.Err may not be set
			// yet when both fire at once, so do not depend on it.
			if errors.Is(err, os.ErrDeadlineExceeded) {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return linuxInputEvent{}, ctxErr
				}
				return linuxInputEvent{}, context.DeadlineExceeded
			}
			// Poll reported readiness that did not survive to the read. Treat
			// it as spurious and wait again rather than reporting a fault.
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				continue
			}
			return linuxInputEvent{}, s.normalizeReadError(err)
		}
		if count == 0 && s.pending.Len() < size {
			// A descriptor that reports readable but yields nothing has
			// reached end of file: the device detached.
			return linuxInputEvent{}, s.normalizeReadError(io.EOF)
		}
	}
}

func (s *linuxSource) appendRaw(event linuxInputEvent) {
	_ = binary.Write(&s.raw, binary.NativeEndian, event)
	if s.raw.Len() > maxLinuxRawBytes {
		s.raw.Reset()
		s.dropped = true
	}
}

func (s *linuxSource) apply(event linuxInputEvent) {
	switch event.Type {
	case evKey:
		if control, ok := linuxKeyControl(event.Code); ok {
			s.state.SetButton(control, event.Value != 0)
		}
	case evAbs:
		info, known := s.ranges[event.Code]
		if !known {
			return
		}
		switch event.Code {
		case absX:
			s.state.LeftStick.X = teleop.NormalizeAxis(event.Value, info.Minimum, info.Maximum)
		case absY:
			s.state.LeftStick.Y = -teleop.NormalizeAxis(event.Value, info.Minimum, info.Maximum)
		case absRX:
			s.state.RightStick.X = teleop.NormalizeAxis(event.Value, info.Minimum, info.Maximum)
		case absRY:
			s.state.RightStick.Y = -teleop.NormalizeAxis(event.Value, info.Minimum, info.Maximum)
		case absZ:
			s.state.LeftTrigger = teleop.NormalizeTrigger(event.Value, info.Minimum, info.Maximum)
		case absRZ:
			s.state.RightTrigger = teleop.NormalizeTrigger(event.Value, info.Minimum, info.Maximum)
		case absHat0X:
			s.state.DPad.Left = event.Value < 0
			s.state.DPad.Right = event.Value > 0
		case absHat0Y:
			s.state.DPad.Up = event.Value < 0
			s.state.DPad.Down = event.Value > 0
		}
	}
}

func (s *linuxSource) SetRumble(ctx context.Context, rumble teleop.Rumble) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.rumbleMu.Lock()
	defer s.rumbleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.isClosed() {
		return teleop.ErrClosed
	}
	if !s.descriptor.Capability.Rumble {
		return teleop.ErrUnsupported
	}
	if rumble.LowFrequency == 0 && rumble.HighFrequency == 0 {
		if err := s.stopRumbleLocked(false); err != nil {
			return s.normalizeRumbleError(err)
		}
		return nil
	}

	effect := newLinuxRumbleEffect(s.rumbleEffect, rumble)
	if err := linuxIOCTL(
		s.file.Fd(),
		linuxIOW('E', 0x80, unsafe.Sizeof(effect)),
		unsafe.Pointer(&effect),
	); err != nil {
		return s.normalizeRumbleError(err)
	}
	if effect.ID < 0 {
		return fmt.Errorf("%w: evdev did not allocate a rumble effect", teleop.ErrUnavailable)
	}
	s.rumbleEffect = effect.ID
	if err := s.writeRumbleEvent(effect.ID, 1); err != nil {
		return s.normalizeRumbleError(err)
	}
	return nil
}

func newLinuxRumbleEffect(id int16, rumble teleop.Rumble) linuxFFEffect {
	effect := linuxFFEffect{
		Type: ffRumble,
		ID:   id,
		// A zero replay length is the evdev representation of an effect that
		// remains active until an EV_FF stop event is written.
		Replay: linuxFFReplay{Length: 0},
	}
	binary.NativeEndian.PutUint16(effect.Data[0:2], rumbleMagnitude(rumble.LowFrequency))
	binary.NativeEndian.PutUint16(effect.Data[2:4], rumbleMagnitude(rumble.HighFrequency))
	return effect
}

func (s *linuxSource) writeRumbleEvent(effectID int16, value int32) error {
	event := linuxInputEvent{
		Type:  evFF,
		Code:  uint16(effectID),
		Value: value,
	}
	var encoded bytes.Buffer
	if err := binary.Write(&encoded, binary.NativeEndian, event); err != nil {
		return err
	}
	written, err := s.file.Write(encoded.Bytes())
	if err != nil {
		return err
	}
	if written != encoded.Len() {
		return io.ErrShortWrite
	}
	return nil
}

func (s *linuxSource) stopRumbleLocked(remove bool) error {
	if !s.descriptor.Capability.Rumble || s.rumbleEffect < 0 {
		return nil
	}
	var result error
	if err := s.writeRumbleEvent(s.rumbleEffect, 0); err != nil {
		result = errors.Join(result, err)
	}
	if remove {
		request := linuxIOW('E', 0x81, unsafe.Sizeof(int32(0)))
		if err := linuxIOCTLValue(s.file.Fd(), request, uintptr(s.rumbleEffect)); err != nil {
			result = errors.Join(result, err)
		} else {
			s.rumbleEffect = -1
		}
	}
	return result
}

func (s *linuxSource) normalizeRumbleError(err error) error {
	switch {
	case err == nil:
		return nil
	case s.isClosed() || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EBADF):
		return teleop.ErrClosed
	case errors.Is(err, syscall.ENODEV),
		errors.Is(err, syscall.ENXIO),
		errors.Is(err, syscall.EIO):
		return fmt.Errorf("%w: evdev rumble: %v", teleop.ErrDisconnected, err)
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return fmt.Errorf("%w: evdev rumble: %v", teleop.ErrPermission, err)
	default:
		return fmt.Errorf("%w: evdev rumble: %v", teleop.ErrUnavailable, err)
	}
}

func (s *linuxSource) resync() error {
	keys := make([]byte, 96)
	if err := linuxIOCTLBytes(s.file.Fd(), linuxIOR('E', 0x18, uintptr(len(keys))), keys); err != nil {
		return fmt.Errorf("query controller key state: %w", err)
	}
	digital := make(map[teleop.ControlID]bool)
	for code, control := range linuxKeyControls() {
		digital[control] = digital[control] || linuxBit(keys, code)
	}
	for control, pressed := range digital {
		s.state.SetButton(control, pressed)
	}
	for code := range s.ranges {
		var info linuxAbsInfo
		if err := linuxIOCTL(
			s.file.Fd(),
			linuxIOR('E', uintptr(0x40+code), unsafe.Sizeof(info)),
			unsafe.Pointer(&info),
		); err != nil {
			continue
		}
		s.ranges[code] = info
		s.apply(linuxInputEvent{Type: evAbs, Code: code, Value: info.Value})
	}
	return nil
}

func (s *linuxSource) Close() error {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		s.closed = true
		s.closeMu.Unlock()
		s.updateTransportHealth(time.Now(), false, s.probeAttachment != nil)
		s.rumbleMu.Lock()
		rumbleErr := s.stopRumbleLocked(true)
		s.rumbleMu.Unlock()
		if errors.Is(rumbleErr, syscall.ENODEV) ||
			errors.Is(rumbleErr, syscall.ENXIO) ||
			errors.Is(rumbleErr, syscall.EIO) {
			rumbleErr = nil
		}
		closeErr := s.file.Close()
		s.closeMu.Lock()
		s.closeErr = errors.Join(rumbleErr, closeErr)
		s.closeMu.Unlock()
	})
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	return s.closeErr
}

func (s *linuxSource) isClosed() bool {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	return s.closed
}

func (s *linuxSource) waitReadable(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.isClosed() {
			return teleop.ErrClosed
		}

		timeout := linuxReadPollInterval
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
				return context.DeadlineExceeded
			}
			timeout = min(timeout, remaining)
		}
		timeoutMillis := int((timeout + time.Millisecond - 1) / time.Millisecond)
		pollFDs := []unix.PollFd{{
			Fd:     int32(s.file.Fd()),
			Events: unix.POLLIN,
		}}
		ready, err := unix.Poll(pollFDs, timeoutMillis)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return s.normalizeReadError(err)
		}
		if ready == 0 {
			if err := s.checkEvdevAttachment(); err != nil {
				return err
			}
			continue
		}
		revents := pollFDs[0].Revents
		if revents&unix.POLLNVAL != 0 {
			s.updateTransportHealth(time.Now(), false, s.probeAttachment != nil)
			if s.isClosed() {
				return teleop.ErrClosed
			}
			return fmt.Errorf("%w: evdev descriptor is invalid", teleop.ErrDisconnected)
		}
		if revents&unix.POLLIN != 0 {
			// A busy device may never reach the quiet-poll branch above. Keep the
			// independent attachment evidence fresh before consuming that input too.
			if err := s.checkEvdevAttachment(); err != nil {
				return err
			}
		}
		if revents&(unix.POLLIN|unix.POLLERR|unix.POLLHUP) != 0 {
			if revents&(unix.POLLERR|unix.POLLHUP) != 0 {
				s.updateTransportHealth(time.Now(), false, s.probeAttachment != nil)
			}
			return nil
		}
	}
}

func (s *linuxSource) normalizeReadError(err error) error {
	if err == nil {
		return nil
	}
	if s.isClosed() || errors.Is(err, os.ErrClosed) {
		return teleop.ErrClosed
	}
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ENODEV) ||
		errors.Is(err, syscall.ENXIO) ||
		errors.Is(err, syscall.EIO) {
		return fmt.Errorf("%w: evdev read: %v", teleop.ErrDisconnected, err)
	}
	return err
}

func linuxCapabilities(file *os.File) (map[teleop.ControlID]bool, map[uint16]linuxAbsInfo, error) {
	keys := make([]byte, 96)
	if err := linuxIOCTLBytes(file.Fd(), linuxIOR('E', 0x20+evKey, uintptr(len(keys))), keys); err != nil {
		return nil, nil, err
	}
	absolute := make([]byte, 16)
	if err := linuxIOCTLBytes(file.Fd(), linuxIOR('E', 0x20+evAbs, uintptr(len(absolute))), absolute); err != nil {
		return nil, nil, err
	}
	supported := make(map[teleop.ControlID]bool)
	for code, control := range linuxKeyControls() {
		if linuxBit(keys, code) {
			supported[control] = true
		}
	}
	ranges := make(map[uint16]linuxAbsInfo)
	for _, code := range []uint16{absX, absY, absZ, absRX, absRY, absRZ, absHat0X, absHat0Y} {
		if !linuxBit(absolute, int(code)) {
			continue
		}
		var info linuxAbsInfo
		if err := linuxIOCTL(
			file.Fd(),
			linuxIOR('E', uintptr(0x40+code), unsafe.Sizeof(info)),
			unsafe.Pointer(&info),
		); err == nil {
			ranges[code] = info
		}
	}
	if _, ok := ranges[absX]; ok {
		supported[teleop.StickLeft] = true
	}
	if _, ok := ranges[absRX]; ok {
		supported[teleop.StickRight] = true
	}
	if _, ok := ranges[absZ]; ok {
		supported[teleop.TriggerLeft] = true
	}
	if _, ok := ranges[absRZ]; ok {
		supported[teleop.TriggerRight] = true
	}
	if _, ok := ranges[absHat0X]; ok {
		supported[teleop.DPadLeft] = true
		supported[teleop.DPadRight] = true
	}
	if _, ok := ranges[absHat0Y]; ok {
		supported[teleop.DPadUp] = true
		supported[teleop.DPadDown] = true
	}
	return supported, ranges, nil
}

func linuxRumbleSupported(file *os.File) bool {
	feedback := make([]byte, ffMax/8+1)
	if err := linuxIOCTLBytes(
		file.Fd(),
		linuxIOR('E', 0x20+evFF, uintptr(len(feedback))),
		feedback,
	); err != nil {
		return false
	}
	return linuxBit(feedback, ffRumble)
}

var linuxControls = map[int]teleop.ControlID{
	btnSouth: ButtonA,
	btnEast:  ButtonB,
	// Xbox's xpad driver emits the legacy BTN_X/BTN_Y aliases. Those aliases
	// share numeric values with BTN_NORTH/BTN_WEST, respectively, so their
	// physical labels are the inverse of the geometric names.
	btnNorth:      ButtonX,
	btnWest:       ButtonY,
	btnTL:         LeftBumper,
	btnTR:         RightBumper,
	btnThumbL:     LeftStick,
	btnThumbR:     RightStick,
	btnStart:      Menu,
	btnSelect:     View,
	btnMode:       Xbox,
	keyF12:        Share,
	keyRecord:     Share,
	btnDPadUp:     teleop.DPadUp,
	btnDPadDown:   teleop.DPadDown,
	btnDPadLeft:   teleop.DPadLeft,
	btnDPadRight:  teleop.DPadRight,
	btnGripLeft:   Paddle1,
	btnGripRight:  Paddle2,
	btnGripLeft2:  Paddle3,
	btnGripRight2: Paddle4,
}

func linuxKeyControls() map[int]teleop.ControlID { return linuxControls }

func linuxKeyControl(code uint16) (teleop.ControlID, bool) {
	control, ok := linuxKeyControls()[int(code)]
	return control, ok
}

func linuxDeviceString(fd uintptr, number uintptr) (string, error) {
	buffer := make([]byte, 256)
	if err := linuxIOCTLBytes(fd, linuxIOR('E', number, uintptr(len(buffer))), buffer); err != nil {
		return "", err
	}
	if index := bytes.IndexByte(buffer, 0); index >= 0 {
		buffer = buffer[:index]
	}
	return string(buffer), nil
}

func linuxBit(buffer []byte, bit int) bool {
	index := bit / 8
	return index >= 0 && index < len(buffer) && buffer[index]&(1<<uint(bit%8)) != 0
}

func linuxIOCTL(fd, request uintptr, value unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}

func linuxIOCTLBytes(fd, request uintptr, value []byte) error {
	if len(value) == 0 {
		return nil
	}
	return linuxIOCTL(fd, request, unsafe.Pointer(&value[0]))
}

func linuxIOCTLValue(fd, request, value uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, value)
	if errno != 0 {
		return errno
	}
	return nil
}

func linuxEventTime(event linuxInputEvent) time.Time {
	if event.Time.Sec == 0 {
		return time.Now()
	}
	return time.Unix(int64(event.Time.Sec), int64(event.Time.Usec)*1_000)
}
