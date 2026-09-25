//go:build (!darwin && !linux && !windows) || (darwin && !cgo)

package generic

import (
	"context"
	"fmt"
	"github.com/open-ships/teleop"
)

func discoverPlatform(ctx context.Context) ([]Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: generic gamepads require macOS with cgo, Linux, or Windows", teleop.ErrUnsupported)
}
func openPlatform(ctx context.Context, _ teleop.DeviceID) (RawSource, error) {
	_, err := discoverPlatform(ctx)
	return nil, err
}
