package generic

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/profiles"
)

// ControllerType identifies the generic provider family, independently of layout.
const ControllerType teleop.ControllerType = "generic"

// Option configures a generic provider before discovery or opening.
type Option func(*Provider)

// WithProfile selects a physical layout. Defaults to profiles.SNES. A layout
// supplies labels and expected controls; a matching device mapping is required.
func WithProfile(profile profiles.Profile) Option {
	return func(p *Provider) { p.profile = profile.Clone() }
}

// WithMapping registers an explicit device mapping. More than one matching
// mapping is rejected as ambiguous; there is no implicit registration globally.
func WithMapping(mapping Mapping) Option {
	return func(p *Provider) { p.mappings = append(p.mappings, mapping.clone()) }
}

// Provider discovers generic gamepads and returns ordinary teleop controllers.
// Xbox/Microsoft devices are reserved for xbox.Provider. Options are copied and
// immutable after construction; independent controllers can be opened concurrently.
type Provider struct {
	profile  profiles.Profile
	mappings []Mapping
	discover func(context.Context) ([]Device, error)
	open     func(context.Context, teleop.DeviceID) (RawSource, error)
}

// NewProvider constructs a provider with copied options and no background work.
func NewProvider(options ...Option) *Provider {
	p := &Provider{profile: profiles.SNES.Clone(), discover: discoverPlatform, open: openPlatform}
	for _, option := range options {
		if option != nil {
			option(p)
		}
	}
	return p
}
func (*Provider) Type() teleop.ControllerType { return ControllerType }

// Inspect returns raw input descriptions for configuration tools. Unmapped
// devices remain visible so users can configure them without guessing IDs.
func (p *Provider) Inspect(ctx context.Context) ([]Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateProfile(p.profile); err != nil {
		return nil, err
	}
	devices, err := p.discover(ctx)
	result := make([]Device, 0, len(devices))
	for _, device := range devices {
		if !xboxOwned(device.Descriptor) {
			result = append(result, device.Clone())
		}
	}
	slices.SortFunc(result, func(a, b Device) int {
		if a.Descriptor.ID < b.Descriptor.ID {
			return -1
		}
		if a.Descriptor.ID > b.Descriptor.ID {
			return 1
		}
		return 0
	})
	return result, err
}
func (p *Provider) resolve(d Device) (Mapping, error) {
	var matches []Mapping
	for _, m := range p.mappings {
		if m.matches(d) && m.Profile == p.profile.ID {
			matches = append(matches, m)
		}
	}
	if len(matches) == 0 {
		return Mapping{}, fmt.Errorf("%w: %s (%04x:%04x); configure the %s profile", ErrMappingRequired, d.Descriptor.Name, d.Descriptor.VendorID, d.Descriptor.ProductID, p.profile.ID)
	}
	if len(matches) > 1 {
		return Mapping{}, fmt.Errorf("%w: ambiguous mappings for %s", teleop.ErrInvalidState, d.Descriptor.Name)
	}
	if err := matches[0].validate(d, p.profile); err != nil {
		return Mapping{}, err
	}
	return matches[0], nil
}
func (p *Provider) descriptor(d Device, mapped bool) teleop.Descriptor {
	descriptor := d.Descriptor.Clone()
	descriptor.Type = ControllerType
	descriptor.Capability.Controls = nil
	descriptor.Capability.Rumble = false
	if descriptor.Properties == nil {
		descriptor.Properties = map[string]string{}
	}
	descriptor.Properties["profile"] = p.profile.ID
	descriptor.Properties["input_layout"] = d.Fingerprint()
	descriptor.Properties["mapping_status"] = "required"
	if mapped {
		descriptor.Capability.Controls = slices.Clone(p.profile.Controls)
		descriptor.Properties["mapping_status"] = "ready"
		if mapping, err := p.resolve(d); err == nil {
			encoded, _ := json.Marshal(mapping)
			descriptor.Properties["mapping_sha256"] = fmt.Sprintf("%x", sha256.Sum256(encoded))
		}
	}
	return descriptor
}
func (p *Provider) Discover(ctx context.Context) ([]teleop.Descriptor, error) {
	devices, err := p.Inspect(ctx)
	var descriptors []teleop.Descriptor
	for _, device := range devices {
		_, mappingErr := p.resolve(device)
		if mappingErr != nil && !errors.Is(mappingErr, ErrMappingRequired) {
			err = errors.Join(err, mappingErr)
		}
		descriptors = append(descriptors, p.descriptor(device, mappingErr == nil))
	}
	return descriptors, err
}

// OpenRaw opens a gamepad without applying a mapping, for inspection/setup.
func (p *Provider) OpenRaw(ctx context.Context, id teleop.DeviceID) (RawSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateProfile(p.profile); err != nil {
		return nil, err
	}
	source, err := p.open(ctx, id)
	if err != nil {
		return nil, err
	}
	if xboxOwned(source.Device().Descriptor) {
		_ = source.Close()
		return nil, fmt.Errorf("%w: Xbox devices belong to xbox.Provider", teleop.ErrUnsupported)
	}
	return source, nil
}
func (p *Provider) Open(ctx context.Context, id teleop.DeviceID, options ...teleop.OpenOption) (teleop.GameController, error) {
	raw, err := p.OpenRaw(ctx, id)
	if err != nil {
		return nil, err
	}
	device := raw.Device()
	mapping, err := p.resolve(device)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	source := &mappedSource{raw: raw, device: device, mapping: mapping, descriptor: p.descriptor(device, true)}
	opts := append([]teleop.OpenOption{teleop.WithContext(ctx)}, options...)
	return teleop.NewController(source, opts...)
}

// Watch reports attachment and mapping changes through the existing hotplug API.
func (p *Provider) Watch(ctx context.Context) <-chan teleop.DeviceEvent {
	events := make(chan teleop.DeviceEvent, 16)
	go func() {
		defer close(events)
		known := map[teleop.DeviceID]teleop.Descriptor{}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		send := func(e teleop.DeviceEvent) bool {
			select {
			case events <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			if ctx.Err() != nil {
				return
			}
			devices, err := p.Discover(ctx)
			now := time.Now()
			if err != nil {
				if !send(teleop.DeviceEvent{Kind: teleop.DeviceError, Error: err.Error(), ObservedAt: now}) {
					return
				}
			} else {
				current := map[teleop.DeviceID]teleop.Descriptor{}
				for _, d := range devices {
					current[d.ID] = d.Clone()
					old, ok := known[d.ID]
					if ok && reflect.DeepEqual(old, d) {
						continue
					}
					kind := teleop.DeviceAdded
					if ok {
						kind = teleop.DeviceUpdated
					}
					if !send(teleop.DeviceEvent{Kind: kind, Descriptor: d.Clone(), ObservedAt: now}) {
						return
					}
				}
				for id, d := range known {
					if _, ok := current[id]; !ok {
						if !send(teleop.DeviceEvent{Kind: teleop.DeviceRemoved, Descriptor: d.Clone(), ObservedAt: now}) {
							return
						}
					}
				}
				known = current
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return events
}

type mappedSource struct {
	raw        RawSource
	device     Device
	mapping    Mapping
	descriptor teleop.Descriptor
	previous   RawState
	healthMu   sync.RWMutex
	health     teleop.TransportHealth
}

func (s *mappedSource) Descriptor() teleop.Descriptor { return s.descriptor.Clone() }
func (s *mappedSource) TransportHealth() teleop.TransportHealth {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()
	return s.health
}
func (s *mappedSource) Read(ctx context.Context) (teleop.Observation, error) {
	for {
		raw, err := s.raw.Read(ctx)
		now := time.Now()
		// Successful samples include an independent OS attachment check. A timer
		// or unchanged operator input alone is never treated as connection evidence.
		if ctx.Err() == nil {
			known := err == nil || errors.Is(err, teleop.ErrDisconnected)
			s.healthMu.Lock()
			s.health = teleop.TransportHealth{Sequence: s.health.Sequence + 1, CheckedAt: now, Connected: err == nil, SilenceVerifiable: known}
			s.healthMu.Unlock()
		}
		if err != nil {
			return teleop.Observation{}, err
		}
		state, err := s.mapping.apply(s.device, raw)
		if err != nil {
			return teleop.Observation{}, err
		}
		if s.previous != nil && maps.Equal(s.previous, raw) {
			continue
		}
		s.previous = maps.Clone(raw)
		return teleop.Observation{State: state, ObservedAt: now, Native: teleop.NativeInput{Format: s.descriptor.Backend + "-sample", Fields: maps.Clone(raw)}}, nil
	}
}
func (s *mappedSource) Close() error { return s.raw.Close() }

var _ teleop.WatchingProvider = (*Provider)(nil)
var _ teleop.TransportHealthSource = (*mappedSource)(nil)
