// Package testkit provides deterministic input sources for application and
// provider tests. It does not require controller hardware.
package testkit

import (
	"context"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/open-ships/teleop"
)

// FakeSource is a bounded, push-driven teleop.InputSource for tests.
type FakeSource struct {
	descriptor   teleop.Descriptor
	observations chan teleop.Observation
	done         chan struct{}
	closeOnce    sync.Once
	healthMu     sync.RWMutex
	health       teleop.TransportHealth
	rumbleMu     sync.RWMutex
	rumble       teleop.Rumble
}

// NewFakeSource returns a fake source with sensible virtual-device defaults.
func NewFakeSource(descriptor teleop.Descriptor, buffer int) *FakeSource {
	if buffer <= 0 {
		buffer = 64
	}
	if descriptor.ID == "" {
		descriptor.ID = "fake:0"
	}
	if descriptor.Name == "" {
		descriptor.Name = "Fake game controller"
	}
	if descriptor.Transport == "" {
		descriptor.Transport = teleop.TransportVirtual
	}
	if descriptor.Backend == "" {
		descriptor.Backend = "testkit"
	}
	return &FakeSource{
		descriptor:   descriptor.Clone(),
		observations: make(chan teleop.Observation, buffer),
		done:         make(chan struct{}),
	}
}

// Descriptor implements teleop.InputSource.
func (f *FakeSource) Descriptor() teleop.Descriptor {
	return f.descriptor.Clone()
}

// TransportHealth implements teleop.TransportHealthSource. The zero value is
// intentionally unverifiable; tests opt into stronger claims with
// SetTransportHealth and advance Sequence for each simulated successful
// transport check.
func (f *FakeSource) TransportHealth() teleop.TransportHealth {
	f.healthMu.RLock()
	defer f.healthMu.RUnlock()
	return f.health
}

// SetTransportHealth replaces the fake source's independently sampled health
// evidence. It is safe to call while the Controller is reading observations.
func (f *FakeSource) SetTransportHealth(health teleop.TransportHealth) {
	f.healthMu.Lock()
	f.health = health
	f.healthMu.Unlock()
}

func (f *FakeSource) Read(ctx context.Context) (teleop.Observation, error) {
	select {
	case <-ctx.Done():
		return teleop.Observation{}, ctx.Err()
	case <-f.done:
		return teleop.Observation{}, io.EOF
	case observation := <-f.observations:
		return observation, nil
	}
}

// Push queues a canonical state observed at the current wall-clock time.
func (f *FakeSource) Push(ctx context.Context, state teleop.State) error {
	return f.PushObservation(ctx, teleop.Observation{
		State:      state,
		ObservedAt: time.Now(),
		Native: teleop.NativeInput{
			Format: "testkit",
		},
	})
}

// PushObservation queues an exact observation for the controller ingest loop.
func (f *FakeSource) PushObservation(ctx context.Context, observation teleop.Observation) error {
	select {
	case <-f.done:
		return teleop.ErrClosed
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.done:
		return teleop.ErrClosed
	case f.observations <- observation:
		return nil
	}
}

// SetRumble implements teleop.RumbleSource when the fake descriptor advertises
// rumble. It retains the latest value for assertions through Rumble.
func (f *FakeSource) SetRumble(ctx context.Context, rumble teleop.Rumble) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !f.descriptor.Capability.Rumble {
		return teleop.ErrUnsupported
	}
	f.rumbleMu.Lock()
	defer f.rumbleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-f.done:
		return teleop.ErrClosed
	default:
	}
	f.rumble = rumble
	return nil
}

// Rumble returns the latest vibration applied to the fake source.
func (f *FakeSource) Rumble() teleop.Rumble {
	f.rumbleMu.RLock()
	defer f.rumbleMu.RUnlock()
	return f.rumble
}

// Close implements teleop.InputSource.
func (f *FakeSource) Close() error {
	f.closeOnce.Do(func() {
		f.rumbleMu.Lock()
		defer f.rumbleMu.Unlock()
		f.rumble = teleop.Rumble{}
		close(f.done)
	})
	return nil
}

// ReplaySource is a finite teleop.InputSource over recorded observations.
type ReplaySource struct {
	descriptor   teleop.Descriptor
	observations []teleop.Observation
	index        int
	mu           sync.Mutex
	closed       bool
}

// NewReplaySource copies descriptor and observations into a finite replay.
func NewReplaySource(descriptor teleop.Descriptor, observations []teleop.Observation) *ReplaySource {
	return &ReplaySource{
		descriptor:   descriptor.Clone(),
		observations: slices.Clone(observations),
	}
}

// Descriptor implements teleop.InputSource.
func (r *ReplaySource) Descriptor() teleop.Descriptor {
	return r.descriptor.Clone()
}

func (r *ReplaySource) Read(ctx context.Context) (teleop.Observation, error) {
	if err := ctx.Err(); err != nil {
		return teleop.Observation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.index >= len(r.observations) {
		return teleop.Observation{}, io.EOF
	}
	observation := r.observations[r.index]
	r.index++
	return observation, nil
}

// Close implements teleop.InputSource.
func (r *ReplaySource) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return nil
}
