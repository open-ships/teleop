//go:build !darwin && !linux && !windows

package xbox

import (
	"context"
	"fmt"
	"runtime"

	"github.com/open-ships/teleop"
)

func discoverPlatform(context.Context) ([]teleop.Descriptor, error) {
	return nil, fmt.Errorf("%w: xbox provider on %s", teleop.ErrUnsupported, runtime.GOOS)
}

func openPlatform(context.Context, teleop.DeviceID) (teleop.InputSource, error) {
	return nil, fmt.Errorf("%w: xbox provider on %s", teleop.ErrUnsupported, runtime.GOOS)
}
