package teleop

import (
	"context"
	"errors"
	"testing"
)

var errDiscovery = errors.New("discovery failed")

type registryTestProvider struct {
	controllerType ControllerType
	devices        []Descriptor
	err            error
}

func (provider registryTestProvider) Type() ControllerType {
	return provider.controllerType
}

func (provider registryTestProvider) Discover(context.Context) ([]Descriptor, error) {
	return provider.devices, provider.err
}

func (registryTestProvider) Open(
	context.Context,
	DeviceID,
	...OpenOption,
) (GameController, error) {
	return nil, ErrUnsupported
}

func TestRegistryDiscoverRetainsDevicesWhenAnotherProviderFails(t *testing.T) {
	registry := NewRegistry(
		registryTestProvider{
			controllerType: "broken",
			err:            errDiscovery,
		},
		registryTestProvider{
			controllerType: "working",
			devices: []Descriptor{{
				ID:   "working:0",
				Name: "Working controller",
			}},
		},
	)

	devices, err := registry.Discover(context.Background())
	if !errors.Is(err, errDiscovery) {
		t.Fatalf("Discover error = %v, want provider error", err)
	}
	if len(devices) != 1 || devices[0].ID != "working:0" {
		t.Fatalf("Discover devices = %#v, want working provider result", devices)
	}
}

func TestZeroRegistryCanRegisterAndOpen(t *testing.T) {
	var registry Registry
	provider := registryTestProvider{controllerType: "working"}
	registry.Register(provider)
	if _, err := registry.Open(context.Background(), "working", "working:0"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open error = %v, want provider error", err)
	}
}
