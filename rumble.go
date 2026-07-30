package teleop

import (
	"context"
	"fmt"
	"math"
)

// Rumble describes the normalized strengths of a controller's two vibration
// components. LowFrequency requests the heavy motor and HighFrequency requests
// the light motor. Backends whose native API exposes intensity and sharpness
// instead of separate motors approximate the same low/high balance. Both
// values are in [0,1].
//
// Rumble remains active until it is replaced, the zero value is applied, or
// the controller session closes.
type Rumble struct {
	LowFrequency  float32 `json:"low_frequency"`
	HighFrequency float32 `json:"high_frequency"`
}

// RumbleSource is the optional output boundary implemented by input sources
// that can vibrate their controller. Implementations must permit SetRumble to
// run concurrently with Read and Close. Close must stop active rumble before
// releasing the device.
type RumbleSource interface {
	SetRumble(context.Context, Rumble) error
}

// SetRumble replaces the controller's current vibration. Applying Rumble{}
// stops both motors. The call is synchronous, but cancellation only bounds the
// operation itself; canceling ctx later does not stop rumble.
func (c *Controller) SetRumble(ctx context.Context, rumble Rumble) (err error) {
	if ctx == nil {
		return fmt.Errorf("%w: nil rumble context", ErrInvalidState)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateRumble(rumble); err != nil {
		return err
	}
	if !c.descriptor.Capability.Rumble {
		return fmt.Errorf("%w: controller does not support rumble", ErrUnsupported)
	}
	output, ok := c.source.(RumbleSource)
	if !ok {
		return fmt.Errorf("%w: controller source does not implement rumble", ErrUnsupported)
	}

	c.rumbleMu.Lock()
	defer c.rumbleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.sourceCtx.Done():
		if terminal := c.Err(); terminal != nil {
			return terminal
		}
		return ErrClosed
	default:
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: input source rumble: %v", ErrCallbackPanic, recovered)
		}
	}()
	if err := output.SetRumble(ctx, rumble); err != nil {
		return fmt.Errorf("set controller rumble: %w", err)
	}
	return nil
}

func validateRumble(rumble Rumble) error {
	for _, motor := range []struct {
		name     string
		strength float32
	}{
		{name: "low-frequency", strength: rumble.LowFrequency},
		{name: "high-frequency", strength: rumble.HighFrequency},
	} {
		name, strength := motor.name, motor.strength
		if math.IsNaN(float64(strength)) ||
			math.IsInf(float64(strength), 0) ||
			strength < 0 ||
			strength > 1 {
			return fmt.Errorf(
				"%w: rumble %s strength %v is outside [0,1]",
				ErrInvalidState,
				name,
				strength,
			)
		}
	}
	return nil
}
