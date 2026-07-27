package teleop

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Registry is an explicit, dependency-injected collection of providers.
type Registry struct {
	mu        sync.RWMutex
	providers map[ControllerType]Provider
}

func NewRegistry(providers ...Provider) *Registry {
	registry := &Registry{providers: make(map[ControllerType]Provider)}
	for _, provider := range providers {
		if provider != nil {
			registry.providers[provider.Type()] = provider
		}
	}
	return registry
}

func (r *Registry) Register(provider Provider) {
	if provider == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[provider.Type()] = provider
}

func (r *Registry) Discover(ctx context.Context) ([]Descriptor, error) {
	r.mu.RLock()
	types := make([]ControllerType, 0, len(r.providers))
	for controllerType := range r.providers {
		types = append(types, controllerType)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	providers := make([]Provider, 0, len(types))
	for _, controllerType := range types {
		providers = append(providers, r.providers[controllerType])
	}
	r.mu.RUnlock()

	var devices []Descriptor
	for _, provider := range providers {
		found, err := provider.Discover(ctx)
		if err != nil {
			return nil, fmt.Errorf("discover %s controllers: %w", provider.Type(), err)
		}
		devices = append(devices, found...)
	}
	return devices, nil
}

func (r *Registry) Open(
	ctx context.Context,
	controllerType ControllerType,
	id DeviceID,
	options ...OpenOption,
) (GameController, error) {
	r.mu.RLock()
	provider := r.providers[controllerType]
	r.mu.RUnlock()
	if provider == nil {
		return nil, fmt.Errorf("%w: controller type %q", ErrUnsupported, controllerType)
	}
	return provider.Open(ctx, id, options...)
}
