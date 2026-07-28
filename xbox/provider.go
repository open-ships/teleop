package xbox

import (
	"context"
	"reflect"
	"time"

	"github.com/open-ships/teleop"
)

type Provider struct{}

func NewProvider() *Provider {
	return &Provider{}
}

func (*Provider) Type() teleop.ControllerType {
	return teleop.ControllerXbox
}

func (*Provider) Discover(ctx context.Context) ([]teleop.Descriptor, error) {
	return discoverPlatform(ctx)
}

func (*Provider) Open(
	ctx context.Context,
	id teleop.DeviceID,
	options ...teleop.OpenOption,
) (teleop.GameController, error) {
	source, err := openPlatform(ctx, id)
	if err != nil {
		return nil, err
	}
	// Provider.Open's context owns both discovery/opening and the resulting
	// session. A caller may still override it explicitly with a later
	// teleop.WithContext option.
	openOptions := append([]teleop.OpenOption{teleop.WithContext(ctx)}, options...)
	return teleop.NewController(source, openOptions...)
}

// Watch polls the platform's controller registry and publishes hotplug changes.
// It is intentionally explicit; no discovery goroutine exists until called.
func (p *Provider) Watch(ctx context.Context) <-chan teleop.DeviceEvent {
	events := make(chan teleop.DeviceEvent, 16)
	go func() {
		defer close(events)
		known := make(map[teleop.DeviceID]teleop.Descriptor)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			devices, err := p.Discover(ctx)
			now := time.Now()
			if err != nil {
				select {
				case events <- teleop.DeviceEvent{
					Kind:       teleop.DeviceError,
					ObservedAt: now,
					Error:      err.Error(),
				}:
				case <-ctx.Done():
					return
				}
			} else {
				current := make(map[teleop.DeviceID]teleop.Descriptor, len(devices))
				for _, device := range devices {
					current[device.ID] = device.Clone()
					previous, exists := known[device.ID]
					kind := teleop.DeviceAdded
					if exists {
						if reflect.DeepEqual(previous, device) {
							continue
						}
						kind = teleop.DeviceUpdated
					}
					select {
					case events <- teleop.DeviceEvent{
						Kind:       kind,
						Descriptor: device.Clone(),
						ObservedAt: now,
					}:
					case <-ctx.Done():
						return
					}
				}
				for id, device := range known {
					if _, exists := current[id]; exists {
						continue
					}
					select {
					case events <- teleop.DeviceEvent{
						Kind:       teleop.DeviceRemoved,
						Descriptor: device.Clone(),
						ObservedAt: now,
					}:
					case <-ctx.Done():
						return
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

func capabilities(auditGrade teleop.AuditGrade, supported map[teleop.ControlID]bool) teleop.Capabilities {
	controls := []teleop.ControlDescriptor{
		{ID: ButtonA, Kind: teleop.ControlButton, Label: "A"},
		{ID: ButtonB, Kind: teleop.ControlButton, Label: "B"},
		{ID: ButtonX, Kind: teleop.ControlButton, Label: "X"},
		{ID: ButtonY, Kind: teleop.ControlButton, Label: "Y"},
		{ID: LeftBumper, Kind: teleop.ControlButton, Label: "LB"},
		{ID: RightBumper, Kind: teleop.ControlButton, Label: "RB"},
		{ID: LeftStick, Kind: teleop.ControlButton, Label: "LS"},
		{ID: RightStick, Kind: teleop.ControlButton, Label: "RS"},
		{ID: Menu, Kind: teleop.ControlButton, Label: "Menu"},
		{ID: View, Kind: teleop.ControlButton, Label: "View"},
		{ID: Xbox, Kind: teleop.ControlButton, Label: "Xbox"},
		{ID: Share, Kind: teleop.ControlButton, Label: "Share"},
		{ID: Paddle1, Kind: teleop.ControlButton, Label: "P1"},
		{ID: Paddle2, Kind: teleop.ControlButton, Label: "P2"},
		{ID: Paddle3, Kind: teleop.ControlButton, Label: "P3"},
		{ID: Paddle4, Kind: teleop.ControlButton, Label: "P4"},
		{ID: teleop.DPadUp, Kind: teleop.ControlDPad, Label: "D-pad up"},
		{ID: teleop.DPadDown, Kind: teleop.ControlDPad, Label: "D-pad down"},
		{ID: teleop.DPadLeft, Kind: teleop.ControlDPad, Label: "D-pad left"},
		{ID: teleop.DPadRight, Kind: teleop.ControlDPad, Label: "D-pad right"},
		{ID: teleop.StickLeft, Kind: teleop.ControlStick, Label: "Left stick"},
		{ID: teleop.StickRight, Kind: teleop.ControlStick, Label: "Right stick"},
		{ID: teleop.TriggerLeft, Kind: teleop.ControlTrigger, Label: "LT"},
		{ID: teleop.TriggerRight, Kind: teleop.ControlTrigger, Label: "RT"},
	}
	filtered := controls[:0]
	for _, control := range controls {
		if supported == nil || supported[control.ID] {
			filtered = append(filtered, control)
		}
	}
	return teleop.Capabilities{
		Controls:   filtered,
		AuditGrade: auditGrade,
	}
}
