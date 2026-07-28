package teleop

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Registry is an explicit, dependency-injected collection of providers. Its
// zero value is ready for Register.
type Registry struct {
	mu        sync.RWMutex
	providers map[ControllerType]Provider
}

// NewRegistry constructs an explicit provider registry.
func NewRegistry(providers ...Provider) *Registry {
	registry := &Registry{providers: make(map[ControllerType]Provider)}
	for _, provider := range providers {
		if provider != nil {
			registry.providers[provider.Type()] = provider
		}
	}
	return registry
}

// Register adds or replaces the provider for its controller type.
func (r *Registry) Register(provider Provider) {
	if provider == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.providers == nil {
		r.providers = make(map[ControllerType]Provider)
	}
	r.providers[provider.Type()] = provider
}

// Discover queries every provider, retaining successful results while joining
// provider-specific errors.
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

	var (
		devices   []Descriptor
		resultErr error
	)
	for _, provider := range providers {
		found, err := provider.Discover(ctx)
		for _, descriptor := range found {
			devices = append(devices, descriptor.Clone())
		}
		if err != nil {
			resultErr = errors.Join(
				resultErr,
				fmt.Errorf("discover %s controllers: %w", provider.Type(), err),
			)
			if ctx.Err() != nil {
				break
			}
		}
	}
	return devices, resultErr
}

// Open delegates to the registered provider for controllerType.
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
