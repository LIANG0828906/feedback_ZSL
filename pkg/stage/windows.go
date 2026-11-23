package stage

import (
	"math"
	"sort"
)

type WindowSpec struct {
	Index      int
	StartUS    int64
	EndUS      int64
	FrameStart int
	FrameEnd   int
	CueStart   int
	CueOffset  int
}

func BuildWindowSpecs(p *Project, params Params) []WindowSpec {
	params = params.WithDefaults(p)
	total := WindowCount(p.DurationMS, params.WindowMS)
	specs := make([]WindowSpec, total)
	for i := range specs {
		start, end := WindowBounds(p.DurationMS, params.WindowMS, i)
		specs[i] = WindowSpec{Index: i, StartUS: start, EndUS: end, FrameStart: -1, FrameEnd: -1}
	}
	frameCount := FrameCount(p.DurationMS, p.FPS)
	windowUS := params.WindowMS * 1000
	for frame := 0; frame < frameCount; frame++ {
		index := windowIndexForTime(FrameTimeUS(frame, p.FPS), windowUS, total)
		if specs[index].FrameStart == -1 {
			specs[index].FrameStart = frame
		}
		specs[index].FrameEnd = frame + 1
	}
	for i := range specs {
		if specs[i].FrameStart == -1 {
			specs[i].FrameStart = 0
			specs[i].FrameEnd = 0
		}
		specs[i].CueStart = cueOffsetBefore(p, specs[i].StartUS)
		specs[i].CueOffset = cueOffsetBefore(p, specs[i].EndUS)
	}
	return specs
}

func cueOffsetBefore(p *Project, endUS int64) int {
	return sort.Search(len(p.Cues), func(i int) bool {
		return p.Cues[i].TimeMS*1000 >= endUS
	})
}

func ComputeWindow(p *Project, evaluator *Evaluator, spec WindowSpec, decision ModeDecision) (WindowResult, error) {
	result := WindowResult{
		Index:            spec.Index,
		StartUS:          spec.StartUS,
		EndUS:            spec.EndUS,
		State:            WindowScheduling,
		CueStart:         spec.CueStart,
		CueOffset:        spec.CueOffset,
		ConsumedCues:     spec.CueOffset - spec.CueStart,
		Groups:           evaluator.GroupSnapshotsBefore(spec.EndUS),
		Decisions:        evaluator.DecisionsInRange(spec.StartUS, spec.EndUS),
		ContentHash:      evaluator.ContentHash(),
		ParamVersion:     evaluator.ParamVersion(),
		AlgorithmVersion: AlgorithmVersion,
	}
	if result.ConsumedCues < 0 {
		return result, newError(CodeState, "window cue offsets are inconsistent", "window", "")
	}
	if decision.Mode == ModeMerged {
		frames, merge, err := computeMergedWindow(p, evaluator, spec, decision)
		if err != nil {
			return result, err
		}
		result.Frames = frames
		result.Merge = merge
	} else {
		frames := make([]FrameOutput, 0, spec.FrameEnd-spec.FrameStart)
		for frame := spec.FrameStart; frame < spec.FrameEnd; frame++ {
			frames = append(frames, evaluator.Frame(frame))
		}
		result.Frames = frames
	}
	return result, nil
}

func computeMergedWindow(p *Project, evaluator *Evaluator, spec WindowSpec, decision ModeDecision) ([]FrameOutput, *MergeInfo, error) {
	if spec.FrameEnd <= spec.FrameStart {
		return []FrameOutput{}, &MergeInfo{Mode: ModeMerged, Basis: joinReasons(decision.Reasons), PreviewFPS: decision.PreviewFPS, FrameStride: 1}, nil
	}
	stride := int(math.Ceil(float64(p.FPS) / float64(decision.PreviewFPS)))
	if stride < 1 {
		stride = 1
	}
	keySet := map[int]struct{}{
		spec.FrameStart:   {},
		spec.FrameEnd - 1: {},
	}
	for frame := spec.FrameStart; frame < spec.FrameEnd; frame += stride {
		keySet[frame] = struct{}{}
	}
	for cueIndex := spec.CueStart; cueIndex < spec.CueOffset; cueIndex++ {
		cue := p.Cues[cueIndex]
		frame := int(math.Ceil(float64(cue.TimeMS*1000) * float64(p.FPS) / 1_000_000))
		if frame < spec.FrameStart {
			frame = spec.FrameStart
		}
		if frame >= spec.FrameEnd {
			frame = spec.FrameEnd - 1
		}
		keySet[frame] = struct{}{}
		if frame > spec.FrameStart {
			keySet[frame-1] = struct{}{}
		}
	}
	for _, sample := range p.Truth.Samples {
		if sample.Frame >= spec.FrameStart && sample.Frame < spec.FrameEnd {
			keySet[sample.Frame] = struct{}{}
		}
	}
	keys := make([]int, 0, len(keySet))
	for frame := range keySet {
		keys = append(keys, frame)
	}
	sort.Ints(keys)
	keyOutputs := make(map[int]FrameOutput, len(keys))
	for _, frame := range keys {
		keyOutputs[frame] = evaluator.Frame(frame)
	}
	frames := make([]FrameOutput, 0, spec.FrameEnd-spec.FrameStart)
	for frame := spec.FrameStart; frame < spec.FrameEnd; frame++ {
		if exact, ok := keyOutputs[frame]; ok {
			frames = append(frames, exact)
			continue
		}
		nextIndex := sort.SearchInts(keys, frame)
		if nextIndex <= 0 || nextIndex >= len(keys) {
			frames = append(frames, evaluator.Frame(frame))
			continue
		}
		left := keys[nextIndex-1]
		right := keys[nextIndex]
		frames = append(frames, interpolateFrames(keyOutputs[left], keyOutputs[right], frame, p.FPS))
	}
	merge := &MergeInfo{
		Mode:        ModeMerged,
		Basis:       joinReasons(decision.Reasons),
		PreviewFPS:  decision.PreviewFPS,
		FrameStride: stride,
	}
	maxBound := 0.0
	for i := 0; i+1 < len(keys); i++ {
		startFrame, endFrame := keys[i], keys[i+1]
		startUS := FrameTimeUS(startFrame, p.FPS)
		endUS := FrameTimeUS(endFrame, p.FPS)
		firstCue, lastCue := -1, -1
		for cueIndex := spec.CueStart; cueIndex < spec.CueOffset; cueIndex++ {
			cueUS := p.Cues[cueIndex].TimeMS * 1000
			if cueUS >= startUS && cueUS < endUS {
				if firstCue == -1 {
					firstCue = cueIndex
				}
				lastCue = cueIndex
			}
		}
		if firstCue == -1 {
			continue
		}
		bound := windowErrorBound(p)
		if bound > maxBound {
			maxBound = bound
		}
		merge.Spans = append(merge.Spans, MergeSpan{
			FirstCueIndex: firstCue,
			LastCueIndex:  lastCue,
			FirstCueID:    p.Cues[firstCue].ID,
			LastCueID:     p.Cues[lastCue].ID,
			StartUS:       startUS,
			EndUS:         endUS,
			KeyFrames:     []int{startFrame, endFrame},
			ErrorBound:    bound,
		})
	}
	merge.MaxObservedError = measureObservedError(p, evaluator, spec, frames, keys)
	return frames, merge, nil
}

func windowErrorBound(p *Project) float64 {
	bound := 0.0
	for _, light := range p.Lights {
		values := []struct {
			rng Range
		}{
			{light.Ranges.Red}, {light.Ranges.Green}, {light.Ranges.Blue},
			{light.Ranges.Intensity}, {light.Ranges.Pan}, {light.Ranges.Tilt},
			{light.Ranges.Focus}, {light.Ranges.ColorTemp}, {light.Ranges.StrobeHz},
		}
		for _, value := range values {
			if span := value.rng.Max - value.rng.Min; span > bound {
				bound = span
			}
		}
	}
	return bound
}

func measureObservedError(p *Project, evaluator *Evaluator, spec WindowSpec, frames []FrameOutput, keys []int) float64 {
	if len(keys) < 2 {
		return 0
	}
	maxError := 0.0
	keySet := make(map[int]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	for i := 0; i+1 < len(keys); i++ {
		mid := (keys[i] + keys[i+1]) / 2
		if mid == keys[i] || mid == keys[i+1] {
			continue
		}
		if _, isKey := keySet[mid]; isKey {
			continue
		}
		approxIndex := mid - spec.FrameStart
		if approxIndex < 0 || approxIndex >= len(frames) {
			continue
		}
		exact := evaluator.Frame(mid)
		for j := range exact.Lights {
			if j >= len(frames[approxIndex].Lights) {
				break
			}
			err := outputDifference(frames[approxIndex].Lights[j], exact.Lights[j])
			if err > maxError {
				maxError = err
			}
		}
	}
	return round6(maxError)
}

func interpolateFrames(left, right FrameOutput, frame, fps int) FrameOutput {
	if left.TimeUS == right.TimeUS {
		return left
	}
	frameTime := FrameTimeUS(frame, fps)
	u := float64(frameTime-left.TimeUS) / float64(right.TimeUS-left.TimeUS)
	out := FrameOutput{Frame: frame, TimeUS: frameTime, Lights: make([]LightOutput, len(left.Lights))}
	for i := range left.Lights {
		a, b := left.Lights[i], right.Lights[i]
		out.Lights[i] = LightOutput{
			LightID:    a.LightID,
			RGB:        RGB{round6(lerp(a.RGB.R, b.RGB.R, u)), round6(lerp(a.RGB.G, b.RGB.G, u)), round6(lerp(a.RGB.B, b.RGB.B, u))},
			RawRGB:     RGB{round6(lerp(a.RawRGB.R, b.RawRGB.R, u)), round6(lerp(a.RawRGB.G, b.RawRGB.G, u)), round6(lerp(a.RawRGB.B, b.RawRGB.B, u))},
			Intensity:  round6(lerp(a.Intensity, b.Intensity, u)),
			Pan:        round6(lerp(a.Pan, b.Pan, u)),
			Tilt:       round6(lerp(a.Tilt, b.Tilt, u)),
			Focus:      round6(lerp(a.Focus, b.Focus, u)),
			ColorTemp:  round6(lerp(a.ColorTemp, b.ColorTemp, u)),
			StrobeHz:   round6(lerp(a.StrobeHz, b.StrobeHz, u)),
			StrobeOpen: a.StrobeOpen,
			X:          round6(lerp(a.X, b.X, u)),
			Y:          round6(lerp(a.Y, b.Y, u)),
			Z:          round6(lerp(a.Z, b.Z, u)),
		}
	}
	return out
}

func lerp(a, b, u float64) float64 { return a + (b-a)*u }

func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	out := reasons[0]
	for _, reason := range reasons[1:] {
		out += "; " + reason
	}
	return out
}
