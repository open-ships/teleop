//go:build darwin && !cgo

package xbox

import (
	"context"
	"fmt"

	"github.com/open-ships/teleop"
)

func discoverPlatform(context.Context) ([]teleop.Descriptor, error) {
	return nil, fmt.Errorf("%w: Xbox discovery on macOS requires cgo", teleop.ErrUnavailable)
}

func openPlatform(context.Context, teleop.DeviceID) (teleop.InputSource, error) {
	return nil, fmt.Errorf("%w: Xbox input on macOS requires cgo", teleop.ErrUnavailable)
}
