package generic

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/profiles"
)

type rawResult struct {
	state RawState
	err   error
}
type fakeRaw struct {
	device  Device
	results chan rawResult
	closed  chan struct{}
	once    sync.Once
}

func (f *fakeRaw) Device() Device { return f.device.Clone() }
func (f *fakeRaw) Read(ctx context.Context) (RawState, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.closed:
		return nil, teleop.ErrClosed
	case r := <-f.results:
		return maps.Clone(r.state), r.err
	}
}
func (f *fakeRaw) Close() error { f.once.Do(func() { close(f.closed) }); return nil }
func testProvider(options ...Option) (*Provider, *fakeRaw) {
	d, _, _ := fixture()
	raw := &fakeRaw{device: d, results: make(chan rawResult, 16), closed: make(chan struct{})}
	p := NewProvider(options...)
	p.discover = func(context.Context) ([]Device, error) { return []Device{d}, nil }
	p.open = func(context.Context, teleop.DeviceID) (RawSource, error) { return raw, nil }
	return p, raw
}
func TestUnmappedDiscoveryDoesNotGuessAndOpenClosesSource(t *testing.T) {
	p, raw := testProvider()
	ds, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Properties["mapping_status"] != "required" || len(ds[0].Capability.Controls) != 0 {
		t.Fatalf("discovery: %+v", ds)
	}
	if _, err := p.Open(context.Background(), ds[0].ID); !errors.Is(err, ErrMappingRequired) {
		t.Fatalf("open: %v", err)
	}
	select {
	case <-raw.closed:
	default:
		t.Fatal("unmapped Open leaked source")
	}
}
func TestProviderCopiesOptionsAndReportsOnlyProfileControls(t *testing.T) {
	_, m, _ := fixture()
	profile := profiles.SNES.Clone()
	p, _ := testProvider(WithMapping(m), WithProfile(profile))
	m.Bindings[0].Input = "changed"
	profile.Controls[0].Label = "changed"
	ds, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	caps := ds[0].Capability
	if caps.Supports(teleop.StickLeft) || caps.Supports(teleop.TriggerLeft) || caps.Rumble || len(caps.Controls) != 12 || caps.Controls[0].Label != "B" {
		t.Fatalf("capabilities: %+v", caps)
	}
	ds[0].Capability.Controls[0].Label = "mutated"
	next, _ := p.Discover(context.Background())
	if next[0].Capability.Controls[0].Label != "B" {
		t.Fatal("discovery descriptor alias")
	}
}
func TestRegistryGenericControllerEventsAndDisconnectRelease(t *testing.T) {
	_, mapping, raw := fixture()
	p, source := testProvider(WithMapping(mapping))
	registry := teleop.NewRegistry(p)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ds, err := registry.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := registry.Open(ctx, ds[0].Type, ds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	sub, err := controller.Subscribe(teleop.SubscriptionOptions{Delivery: teleop.DeliveryLossless, Buffer: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	raw["b1"] = 1
	raw["x"] = 255
	raw["y"] = 0
	source.results <- rawResult{state: raw}
	seen := map[teleop.ControlID]bool{}
	for len(seen) < 3 {
		event, err := sub.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if event, ok := event.(teleop.ButtonEvent); ok && event.Phase == teleop.PhasePressed {
			seen[event.Button] = true
		}
	}
	for _, id := range []teleop.ControlID{teleop.ButtonFaceSouth, teleop.DPadRight, teleop.DPadUp} {
		if !seen[id] {
			t.Fatalf("missing press %s: %v", id, seen)
		}
	}
	source.results <- rawResult{err: teleop.ErrDisconnected}
	released := map[teleop.ControlID]bool{}
	for len(released) < 3 {
		event, err := sub.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if event, ok := event.(teleop.ButtonEvent); ok && event.Phase == teleop.PhaseReleased {
			released[event.Button] = true
		}
	}
	for id := range seen {
		if !released[id] {
			t.Fatalf("disconnect failed to release %s", id)
		}
	}
}
func TestUnchangedSamplesRefreshTransportHealth(t *testing.T) {
	device, mapping, raw := fixture()
	fake := &fakeRaw{device: device, results: make(chan rawResult, 16), closed: make(chan struct{})}
	s := &mappedSource{raw: fake, device: device, mapping: mapping, descriptor: device.Descriptor}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.results <- rawResult{state: raw}
	if _, err := s.Read(ctx); err != nil {
		t.Fatal(err)
	}
	initial := s.TransportHealth()
	result := make(chan error, 1)
	go func() { _, err := s.Read(ctx); result <- err }()
	fake.results <- rawResult{state: raw}
	deadline := time.After(time.Second)
	for s.TransportHealth().Sequence == initial.Sequence {
		select {
		case <-deadline:
			t.Fatal("held state did not refresh health")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if h := s.TransportHealth(); !h.Connected || !h.SilenceVerifiable {
		t.Fatalf("health %+v", h)
	}
	select {
	case err := <-result:
		t.Fatalf("unchanged sample emitted observation: %v", err)
	default:
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestXboxOwnershipAndInvalidMapping(t *testing.T) {
	d, m, _ := fixture()
	p, raw := testProvider(WithMapping(m), WithMapping(m))
	if _, err := p.Open(context.Background(), d.Descriptor.ID); !errors.Is(err, teleop.ErrInvalidState) {
		t.Fatalf("ambiguous mapping: %v", err)
	}
	select {
	case <-raw.closed:
	default:
		t.Fatal("ambiguous mapping leaked source")
	}
	p, _ = testProvider()
	p.discover = func(context.Context) ([]Device, error) {
		x := d.Clone()
		x.Descriptor.VendorID = 0x045e
		return []Device{x, d}, nil
	}
	ds, err := p.Discover(context.Background())
	if err != nil || len(ds) != 1 {
		t.Fatalf("Xbox not excluded: %+v %v", ds, err)
	}
}
