package teleop

import "math"

// Clamp limits value to the inclusive range from minimum to maximum.
func Clamp(value, minimum, maximum float32) float32 {
	if math.IsNaN(float64(value)) {
		if minimum <= 0 && maximum >= 0 {
			return 0
		}
		return minimum
	}
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

// NormalizeAxis converts an integer absolute axis into [-1,+1].
func NormalizeAxis(value, minimum, maximum int32) float32 {
	if maximum <= minimum {
		return 0
	}
	// Linux gamepad axes commonly expose -32768..32767. Treat zero as the
	// electrical center and scale each signed side independently so a centered
	// stick is exactly neutral rather than a small persistent deflection.
	if minimum < 0 && maximum > 0 {
		switch {
		case value == 0:
			return 0
		case value < 0:
			return Clamp(float32(float64(value)/-float64(minimum)), -1, 0)
		default:
			return Clamp(float32(float64(value)/float64(maximum)), 0, 1)
		}
	}
	center := (float64(minimum) + float64(maximum)) / 2
	halfRange := (float64(maximum) - float64(minimum)) / 2
	return Clamp(float32((float64(value)-center)/halfRange), -1, 1)
}

// NormalizeTrigger converts an integer absolute axis into [0,1].
func NormalizeTrigger(value, minimum, maximum int32) float32 {
	if maximum <= minimum {
		return 0
	}
	result := float32((float64(value) - float64(minimum)) / (float64(maximum) - float64(minimum)))
	return Clamp(result, 0, 1)
}

// ApplyRadialDeadZone is an operational helper. The controller's canonical
// audit stream never applies it automatically.
func ApplyRadialDeadZone(stick Stick, deadZone float32) Stick {
	if math.IsNaN(float64(stick.X)) ||
		math.IsNaN(float64(stick.Y)) ||
		math.IsInf(float64(stick.X), 0) ||
		math.IsInf(float64(stick.Y), 0) {
		return Stick{}
	}
	deadZone = Clamp(deadZone, 0, 0.99)
	magnitude := float32(math.Hypot(float64(stick.X), float64(stick.Y)))
	if magnitude <= deadZone || magnitude == 0 {
		return Stick{}
	}
	scaled := Clamp((magnitude-deadZone)/(1-deadZone), 0, 1)
	return Stick{
		X: Clamp(stick.X/magnitude*scaled, -1, 1),
		Y: Clamp(stick.Y/magnitude*scaled, -1, 1),
	}
}
