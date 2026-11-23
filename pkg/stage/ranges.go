package stage

import "math"

func DefaultParamRanges() ParamRanges {
	return ParamRanges{
		Red:       Range{Min: 0, Max: 255},
		Green:     Range{Min: 0, Max: 255},
		Blue:      Range{Min: 0, Max: 255},
		Intensity: Range{Min: 0, Max: 1},
		Pan:       Range{Min: -180, Max: 180},
		Tilt:      Range{Min: -180, Max: 180},
		Focus:     Range{Min: 0, Max: 100},
		ColorTemp: Range{Min: 2000, Max: 10000},
		StrobeHz:  Range{Min: 0, Max: 30},
	}
}

func ValidateRanges(r ParamRanges) error {
	checks := []struct {
		name string
		rng  Range
	}{
		{"ranges.red", r.Red},
		{"ranges.green", r.Green},
		{"ranges.blue", r.Blue},
		{"ranges.intensity", r.Intensity},
		{"ranges.pan", r.Pan},
		{"ranges.tilt", r.Tilt},
		{"ranges.focus", r.Focus},
		{"ranges.color_temp", r.ColorTemp},
		{"ranges.strobe_hz", r.StrobeHz},
	}
	for _, check := range checks {
		if !finite(check.rng.Min) || !finite(check.rng.Max) || check.rng.Min > check.rng.Max {
			return newError(CodeSchema, "invalid declared parameter range", "project.lights[].ranges", check.name)
		}
	}
	return nil
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func (r ParamRanges) fieldRange(field string) (Range, bool) {
	switch field {
	case "rgb.r":
		return r.Red, true
	case "rgb.g":
		return r.Green, true
	case "rgb.b":
		return r.Blue, true
	case "intensity":
		return r.Intensity, true
	case "pan":
		return r.Pan, true
	case "tilt":
		return r.Tilt, true
	case "focus":
		return r.Focus, true
	case "color_temp":
		return r.ColorTemp, true
	case "strobe_hz":
		return r.StrobeHz, true
	default:
		return Range{}, false
	}
}
