package teleop

import "math"

func Clamp(value, minimum, maximum float32) float32 {
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
	center := (float64(minimum) + float64(maximum)) / 2
	halfRange := (float64(maximum) - float64(minimum)) / 2
	return Clamp(float32((float64(value)-center)/halfRange), -1, 1)
}

// NormalizeTrigger converts an integer absolute axis into [0,1].
func NormalizeTrigger(value, minimum, maximum int32) float32 {
	if maximum <= minimum {
		return 0
	}
	result := float32(float64(value-minimum) / float64(maximum-minimum))
	return Clamp(result, 0, 1)
}

// ApplyRadialDeadZone is an operational helper. The controller's canonical
// audit stream never applies it automatically.
func ApplyRadialDeadZone(stick Stick, deadZone float32) Stick {
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
