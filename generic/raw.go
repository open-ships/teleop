// Package generic provides generic gamepad input with explicit device mappings
// and controller layout profiles. A profile never guesses raw button numbering.
package generic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-ships/teleop"
)

// RawKind identifies the OS representation of a physical input.
type RawKind string

const (
	RawButton RawKind = "button"
	RawAxis   RawKind = "axis"
	RawHat    RawKind = "hat"
)

// RawControl describes one backend-specific input. Hat values start at Minimum
// and progress clockwise from up; 4- and 8-position hats are supported. Values
// outside a hat's logical range represent its null (released) state.
type RawControl struct {
	ID      string  `json:"id"`
	Kind    RawKind `json:"kind"`
	Minimum int64   `json:"minimum"`
	Maximum int64   `json:"maximum"`
}

// Device describes a gamepad before a device mapping is applied.
type Device struct {
	Descriptor teleop.Descriptor `json:"descriptor"`
	Controls   []RawControl      `json:"controls"`
}

func (d Device) Clone() Device {
	d.Descriptor = d.Descriptor.Clone()
	d.Controls = slices.Clone(d.Controls)
	return d
}

// Fingerprint identifies a backend's input layout, independently of attachment
// paths. Mappings can survive reconnects without matching different wiring.
func (d Device) Fingerprint() string {
	controls := slices.Clone(d.Controls)
	slices.SortFunc(controls, func(a, b RawControl) int { return strings.Compare(a.ID, b.ID) })
	data, _ := json.Marshal(controls)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RawState contains a complete sampled state indexed by RawControl.ID.
type RawState map[string]int64

// RawSource supports device inspection and interactive mapping. Read returns a
// complete sample, including unchanged samples. Close may run concurrently.
type RawSource interface {
	Device() Device
	Read(context.Context) (RawState, error)
	Close() error
}

// sampledDevice serializes native access with Close. Platform poll functions
// must return promptly and check the retained OS attachment on every sample.
type sampledDevice struct {
	device Device
	mu     sync.Mutex
	closed bool
	poll   func() (RawState, error)
	close  func() error
	next   time.Time
}

func (s *sampledDevice) Device() Device { return s.device.Clone() }
func (s *sampledDevice) Read(ctx context.Context) (RawState, error) {
	// A RawSource has one reader, as does teleop.InputSource.
	if delay := time.Until(s.next); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, teleop.ErrClosed
	}
	state, err := s.poll()
	s.next = time.Now().Add(8 * time.Millisecond)
	return maps.Clone(state), err
}
func (s *sampledDevice) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.close()
}

func xboxOwned(d teleop.Descriptor) bool {
	name := strings.ToLower(d.Name)
	return d.VendorID == 0x045e || strings.Contains(name, "xbox") || strings.Contains(name, "x-box") || strings.Contains(name, "microsoft")
}
