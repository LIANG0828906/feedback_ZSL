package stage

import (
	"fmt"
	"math"
	"sort"
)

func outputDifference(left, right LightOutput) float64 {
	values := []float64{
		left.RGB.R - right.RGB.R,
		left.RGB.G - right.RGB.G,
		left.RGB.B - right.RGB.B,
		left.Intensity - right.Intensity,
		left.Pan - right.Pan,
		left.Tilt - right.Tilt,
		left.Focus - right.Focus,
		left.ColorTemp - right.ColorTemp,
		left.StrobeHz - right.StrobeHz,
		left.X - right.X,
		left.Y - right.Y,
		left.Z - right.Z,
	}
	max := 0.0
	for _, value := range values {
		if math.Abs(value) > max {
			max = math.Abs(value)
		}
	}
	return max
}

func CompareFrameSequences(left, right []FrameOutput) error {
	max := len(left)
	if len(right) < max {
		max = len(right)
	}
	for i := 0; i < max; i++ {
		if left[i].Frame != right[i].Frame || left[i].TimeUS != right[i].TimeUS {
			return divergence(left[i].Frame, "", "frame", fmt.Sprint(left[i].Frame), fmt.Sprint(right[i].Frame), left[i].TimeUS/1000)
		}
		if err := compareFrameOutput(left[i], right[i]); err != nil {
			return err
		}
	}
	if len(left) != len(right) {
		frame := 0
		if len(left) > 0 {
			frame = left[len(left)-1].Frame + 1
		}
		return divergence(frame, "", "frame_count", fmt.Sprint(len(left)), fmt.Sprint(len(right)), 0)
	}
	return nil
}

func compareFrameOutput(left, right FrameOutput) error {
	leftLights := make(map[string]LightOutput, len(left.Lights))
	rightLights := make(map[string]LightOutput, len(right.Lights))
	for _, light := range left.Lights {
		leftLights[light.LightID] = light
	}
	for _, light := range right.Lights {
		rightLights[light.LightID] = light
	}
	ids := make([]string, 0, len(leftLights))
	for id := range leftLights {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		l, lok := leftLights[id]
		r, rok := rightLights[id]
		if !lok || !rok {
			return divergence(left.Frame, id, "light_presence", fmt.Sprint(lok), fmt.Sprint(rok), left.TimeUS/1000)
		}
		if _, field := firstOutputDiff(l, r); field != "" {
			return divergence(left.Frame, id, field, outputFieldValue(l, field), outputFieldValue(r, field), left.TimeUS/1000)
		}
	}
	for id := range rightLights {
		if _, ok := leftLights[id]; !ok {
			return divergence(left.Frame, id, "light_presence", "false", "true", left.TimeUS/1000)
		}
	}
	return nil
}

func firstOutputDiff(left, right LightOutput) (float64, string) {
	type fieldValue struct {
		name  string
		left  float64
		right float64
	}
	fields := []fieldValue{
		{"rgb.r", left.RGB.R, right.RGB.R},
		{"rgb.g", left.RGB.G, right.RGB.G},
		{"rgb.b", left.RGB.B, right.RGB.B},
		{"raw_rgb.r", left.RawRGB.R, right.RawRGB.R},
		{"raw_rgb.g", left.RawRGB.G, right.RawRGB.G},
		{"raw_rgb.b", left.RawRGB.B, right.RawRGB.B},
		{"intensity", left.Intensity, right.Intensity},
		{"pan", left.Pan, right.Pan},
		{"tilt", left.Tilt, right.Tilt},
		{"focus", left.Focus, right.Focus},
		{"color_temp", left.ColorTemp, right.ColorTemp},
		{"strobe_hz", left.StrobeHz, right.StrobeHz},
		{"x", left.X, right.X},
		{"y", left.Y, right.Y},
		{"z", left.Z, right.Z},
	}
	for _, field := range fields {
		if math.Abs(field.left-field.right) > 1e-9 {
			return math.Abs(field.left - field.right), field.name
		}
	}
	if left.StrobeOpen != right.StrobeOpen {
		return 1, "strobe_open"
	}
	return 0, ""
}

func outputFieldValue(output LightOutput, field string) string {
	switch field {
	case "rgb.r":
		return fmt.Sprintf("%.6f", output.RGB.R)
	case "rgb.g":
		return fmt.Sprintf("%.6f", output.RGB.G)
	case "rgb.b":
		return fmt.Sprintf("%.6f", output.RGB.B)
	case "raw_rgb.r":
		return fmt.Sprintf("%.6f", output.RawRGB.R)
	case "raw_rgb.g":
		return fmt.Sprintf("%.6f", output.RawRGB.G)
	case "raw_rgb.b":
		return fmt.Sprintf("%.6f", output.RawRGB.B)
	case "intensity":
		return fmt.Sprintf("%.6f", output.Intensity)
	case "pan":
		return fmt.Sprintf("%.6f", output.Pan)
	case "tilt":
		return fmt.Sprintf("%.6f", output.Tilt)
	case "focus":
		return fmt.Sprintf("%.6f", output.Focus)
	case "color_temp":
		return fmt.Sprintf("%.6f", output.ColorTemp)
	case "strobe_hz":
		return fmt.Sprintf("%.6f", output.StrobeHz)
	case "strobe_open":
		return fmt.Sprint(output.StrobeOpen)
	case "x":
		return fmt.Sprintf("%.6f", output.X)
	case "y":
		return fmt.Sprintf("%.6f", output.Y)
	case "z":
		return fmt.Sprintf("%.6f", output.Z)
	default:
		return ""
	}
}

func divergence(frame int, lightID, field, left, right string, timeMS int64) *DomainError {
	err := newError(CodeDiverge, "path outputs diverge", "frame", field)
	err.Details = append(err.Details, ErrorDetail{
		Frame:   frame,
		TimeMS:  timeMS,
		LightID: lightID,
		Field:   field,
		Left:    left,
		Right:   right,
	})
	return err
}

type PathSnapshot struct {
	Frames            []FrameOutput `json:"frames"`
	WindowFirstFrames []int         `json:"window_first_frames"`
	CueOffsets        []int         `json:"cue_offsets"`
	CommitOrder       []int         `json:"commit_order"`
}

func ComparePathSnapshots(left, right PathSnapshot) error {
	if err := CompareFrameSequences(left.Frames, right.Frames); err != nil {
		return err
	}
	max := len(left.CueOffsets)
	if len(right.CueOffsets) < max {
		max = len(right.CueOffsets)
	}
	for i := 0; i < max; i++ {
		if left.CueOffsets[i] != right.CueOffsets[i] {
			frame := windowFrame(left, i)
			return divergence(frame, "", "cue_offset", fmt.Sprint(left.CueOffsets[i]), fmt.Sprint(right.CueOffsets[i]), 0)
		}
	}
	if len(left.CueOffsets) != len(right.CueOffsets) {
		return divergence(0, "", "cue_offset_count", fmt.Sprint(len(left.CueOffsets)), fmt.Sprint(len(right.CueOffsets)), 0)
	}
	max = len(left.CommitOrder)
	if len(right.CommitOrder) < max {
		max = len(right.CommitOrder)
	}
	for i := 0; i < max; i++ {
		if left.CommitOrder[i] != right.CommitOrder[i] {
			return divergence(windowFrame(left, left.CommitOrder[i]), "", "commit_order", fmt.Sprint(left.CommitOrder[i]), fmt.Sprint(right.CommitOrder[i]), 0)
		}
	}
	if len(left.CommitOrder) != len(right.CommitOrder) {
		return divergence(0, "", "commit_order_count", fmt.Sprint(len(left.CommitOrder)), fmt.Sprint(len(right.CommitOrder)), 0)
	}
	return nil
}

func windowFrame(snapshot PathSnapshot, windowIndex int) int {
	if windowIndex >= 0 && windowIndex < len(snapshot.WindowFirstFrames) {
		return snapshot.WindowFirstFrames[windowIndex]
	}
	if len(snapshot.Frames) == 0 {
		return 0
	}
	for _, frame := range snapshot.Frames {
		if frame.Frame >= windowIndex {
			return frame.Frame
		}
	}
	return snapshot.Frames[len(snapshot.Frames)-1].Frame
}

func VerifyTruth(project *Project, evaluator *Evaluator) error {
	tolerance := project.Truth.Tolerance
	for _, sample := range project.Truth.Samples {
		actual := evaluator.Frame(sample.Frame)
		actualByID := make(map[string]LightOutput, len(actual.Lights))
		for _, light := range actual.Lights {
			actualByID[light.LightID] = light
		}
		for _, expected := range sample.Lights {
			got, ok := actualByID[expected.LightID]
			if !ok {
				return scaleError(sample.TimeMS, expected.LightID, "light_presence", "present", "missing")
			}
			if err := compareTruthLight(sample.TimeMS, expected, got, tolerance, 1); err != nil {
				return err
			}
		}
	}
	return nil
}

func compareTruthLight(timeMS int64, expected TruthLight, actual LightOutput, tolerance Tolerance, factor float64) error {
	checks := []struct {
		field    string
		expected float64
		actual   float64
		tol      float64
	}{
		{"rgb.r", expected.RGB.R, actual.RGB.R, tolerance.Color},
		{"rgb.g", expected.RGB.G, actual.RGB.G, tolerance.Color},
		{"rgb.b", expected.RGB.B, actual.RGB.B, tolerance.Color},
		{"intensity", expected.Intensity, actual.Intensity, tolerance.Intensity},
		{"pan", expected.Pan, actual.Pan, tolerance.Position},
		{"tilt", expected.Tilt, actual.Tilt, tolerance.Position},
		{"focus", expected.Focus, actual.Focus, tolerance.Focus},
		{"color_temp", expected.ColorTemp, actual.ColorTemp, tolerance.ColorTemp},
		{"strobe_hz", expected.StrobeHz, actual.StrobeHz, tolerance.StrobeHz},
	}
	for _, check := range checks {
		if math.Abs(check.actual-check.expected) > check.tol*factor+1e-9 {
			return scaleError(timeMS, expected.LightID, check.field,
				fmt.Sprintf("%.6f", check.expected), fmt.Sprintf("%.6f", check.actual))
		}
	}
	return nil
}

func scaleError(timeMS int64, lightID, field, expected, actual string) *DomainError {
	err := newError(CodeScale, "sample output exceeds declared tolerance", "light:"+lightID, field)
	err.Details = append(err.Details, ErrorDetail{
		TimeMS:  timeMS,
		LightID: lightID,
		Field:   field,
		Left:    expected,
		Right:   actual,
	})
	return err
}

func CheckCrossModeSamples(project *Project, params Params, exact *Evaluator) error {
	params = params.WithDefaults(project)
	decision := SelectMode(project, params)
	mergedDecision := decision
	mergedDecision.Mode = ModeMerged
	specs := BuildWindowSpecs(project, params)
	cache := map[int]WindowResult{}
	for _, sample := range project.Truth.Samples {
		windowIndex := windowIndexForTime(int64(sample.Frame)*int64(1_000_000/project.FPS), params.WindowMS*1000, len(specs))
		windowIndex = windowIndexForFrame(project, params, sample.Frame, len(specs))
		window, ok := cache[windowIndex]
		if !ok {
			var err error
			window, err = ComputeWindow(project, exact, specs[windowIndex], mergedDecision)
			if err != nil {
				return err
			}
			cache[windowIndex] = window
		}
		exactFrame := exact.Frame(sample.Frame)
		mergedByID := make(map[string]LightOutput)
		for i := range window.Frames {
			if window.Frames[i].Frame != sample.Frame {
				continue
			}
			for _, light := range window.Frames[i].Lights {
				mergedByID[light.LightID] = light
			}
		}
		if len(mergedByID) == 0 {
			return scaleError(sample.TimeMS, "", "frame_presence", "present", "missing")
		}
		for _, exactLight := range exactFrame.Lights {
			mergedLight, ok := mergedByID[exactLight.LightID]
			if !ok {
				return scaleError(sample.TimeMS, exactLight.LightID, "light_presence", "present", "missing")
			}
			if err := compareOutputsWithTolerance(sample.TimeMS, exactLight, mergedLight, params); err != nil {
				return err
			}
		}
	}
	return nil
}

func compareOutputsWithTolerance(timeMS int64, exact, approx LightOutput, params Params) error {
	tolerance := projectTolerance(params)
	checks := []struct {
		field     string
		exact     float64
		approx    float64
		tolerance float64
	}{
		{"rgb.r", exact.RGB.R, approx.RGB.R, tolerance.Color},
		{"rgb.g", exact.RGB.G, approx.RGB.G, tolerance.Color},
		{"rgb.b", exact.RGB.B, approx.RGB.B, tolerance.Color},
		{"intensity", exact.Intensity, approx.Intensity, tolerance.Intensity},
		{"pan", exact.Pan, approx.Pan, tolerance.Position},
		{"tilt", exact.Tilt, approx.Tilt, tolerance.Position},
		{"focus", exact.Focus, approx.Focus, tolerance.Focus},
		{"color_temp", exact.ColorTemp, approx.ColorTemp, tolerance.ColorTemp},
		{"strobe_hz", exact.StrobeHz, approx.StrobeHz, tolerance.StrobeHz},
	}
	for _, check := range checks {
		if math.Abs(check.exact-check.approx) > check.tolerance+1e-9 {
			return scaleError(timeMS, exact.LightID, check.field,
				fmt.Sprintf("%.6f", check.exact), fmt.Sprintf("%.6f", check.approx))
		}
	}
	return nil
}

func projectTolerance(params Params) Tolerance {
	tolerance := DefaultTolerance()
	tolerance.Color = params.ColorTolerance
	tolerance.Position = params.PositionTolerance
	tolerance.Intensity = params.OtherTolerance
	tolerance.Focus = params.OtherTolerance
	tolerance.ColorTemp = params.OtherTolerance * 500
	tolerance.StrobeHz = params.OtherTolerance
	return tolerance
}

func windowIndexForFrame(project *Project, params Params, frame, total int) int {
	return windowIndexForTime(FrameTimeUS(frame, project.FPS), params.WindowMS*1000, total)
}
