package dialogue

import (
	"fmt"
)

// GenerateOptions controls the deterministic construction of a test dialogue.
type GenerateOptions struct {
	Seed       int64
	DialogueID string
}

// GenerateProject constructs PCM, annotations, project metadata, and truth
// without reading any external asset. Truth is calculated here from the
// synthesis-side sample geometry; it is never copied from a reconstruction.
func GenerateProject(seed int64) (*Project, error) {
	return GenerateProjectWithOptions(GenerateOptions{Seed: seed, DialogueID: fmt.Sprintf("dlg-seed-%d", seed)})
}

func GenerateProjectWithOptions(options GenerateOptions) (*Project, error) {
	if options.Seed == 0 {
		options.Seed = 42
	}
	if options.DialogueID == "" {
		options.DialogueID = fmt.Sprintf("dlg-seed-%d", options.Seed)
	}
	p := &Project{
		ID:             options.DialogueID,
		SampleRate:     16000,
		TargetLoudness: 5600,
		GainLimit:      5000,
		GainMin:        4000,
		GainMax:        16000,
		FrameLength:    320,
		Hop:            160,
		Source:         SourceInfo{SchemaVersion: Version3, NativeTimeUnit: "sample", NativeGainScale: 1000, NativeLoudnessScale: 10000},
	}
	sceneA := Scene{ID: "scene-a", Name: "Arrival"}
	sceneB := Scene{ID: "scene-b", Name: "Briefing"}
	specs := []segmentSpec{
		{segmentID: "seg-01", length: 5120, sessionGain: 9000, parts: []partSpec{
			{length: 1280, kind: PhonemeVowel, weight: 12000, baseF0: 118000},
			{length: 800, kind: PhonemeConsonant, weight: 5200, baseF0: 132000},
			{length: 1120, kind: PhonemeVowel, weight: 10800, baseF0: 142000},
			{length: 640, kind: PhonemeSilence, weight: 0, baseF0: 100000},
			{length: 640, kind: PhonemeConsonant, weight: 6600, baseF0: 126000},
			{length: 640, kind: PhonemeVowel, weight: 9600, baseF0: 110000},
		}},
		{segmentID: "seg-02", length: 3840, sessionGain: 9500, parts: []partSpec{
			{length: 640, kind: PhonemeConsonant, weight: 4800, baseF0: 106000},
			{length: 960, kind: PhonemeVowel, weight: 12600, baseF0: 154000},
			{length: 480, kind: PhonemeSilence, weight: 0, baseF0: 100000},
			{length: 1120, kind: PhonemeVowel, weight: 11600, baseF0: 132000},
			{length: 640, kind: PhonemeConsonant, weight: 5900, baseF0: 118000},
		}},
		{segmentID: "seg-03", length: 4480, sessionGain: 8800, parts: []partSpec{
			{length: 1120, kind: PhonemeVowel, weight: 10400, baseF0: 124000},
			{length: 800, kind: PhonemeConsonant, weight: 5100, baseF0: 100000},
			{length: 560, kind: PhonemeSilence, weight: 0, baseF0: 90000},
			{length: 960, kind: PhonemeVowel, weight: 13600, baseF0: 168000},
			{length: 1040, kind: PhonemeConsonant, weight: 6200, baseF0: 112000},
		}},
		{segmentID: "seg-04", length: 5120, sessionGain: 9200, parts: []partSpec{
			{length: 1280, kind: PhonemeVowel, weight: 11800, baseF0: 138000},
			{length: 960, kind: PhonemeConsonant, weight: 5600, baseF0: 104000},
			{length: 1280, kind: PhonemeVowel, weight: 10100, baseF0: 116000},
			{length: 640, kind: PhonemeSilence, weight: 0, baseF0: 100000},
			{length: 960, kind: PhonemeVowel, weight: 12900, baseF0: 146000},
		}},
	}
	globalSample := int64(0)
	segmentOrder := 0
	for _, scene := range []*Scene{&sceneA, &sceneB} {
		for sceneSegment := 0; sceneSegment < 2; sceneSegment++ {
			spec := specs[segmentOrder]
			segment, err := buildSegment(spec, globalSample, scene.ID)
			if err != nil {
				return nil, err
			}
			scene.Segments = append(scene.Segments, segment)
			globalSample = segment.EndSample
			segmentOrder++
		}
	}
	p.Scenes = []Scene{sceneA, sceneB}
	p.DeclaredTotalSamples = globalSample
	p.PCM = make([]int16, p.DeclaredTotalSamples)
	for i, segment := range p.Segments() {
		synthesizeSegment(p, segment, options.Seed+int64(i)*7919)
	}
	truth, err := synthesizeTruth(p)
	if err != nil {
		return nil, err
	}
	p.Truth = truth
	if err := validateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

type partSpec struct {
	length int64
	kind   PhonemeKind
	weight int64
	baseF0 int64
}

type segmentSpec struct {
	segmentID   string
	length      int64
	sessionGain int64
	parts       []partSpec
}

func buildSegment(spec segmentSpec, start int64, sceneID string) (Segment, error) {
	var total int64
	for _, part := range spec.parts {
		total += part.length
	}
	if total != spec.length {
		return Segment{}, fmt.Errorf("generator segment length mismatch: %d != %d", total, spec.length)
	}
	segment := Segment{ID: spec.segmentID, SceneID: sceneID, StartSample: start, EndSample: start + spec.length, SessionGain: spec.sessionGain}
	cursor := start
	for i, part := range spec.parts {
		phoneme := Phoneme{
			ID: fmt.Sprintf("%s-p%02d", spec.segmentID, i+1), StartSample: cursor, EndSample: cursor + part.length,
			Kind: part.kind, Weight: part.weight, BaseF0MilliHz: part.baseF0,
		}
		phoneme.F0Points = []F0Point{
			{AtSample: phoneme.StartSample, HertzMilli: part.baseF0},
			{AtSample: phoneme.StartSample + part.length/2, HertzMilli: part.baseF0 + int64((i%3)-1)*9000},
			{AtSample: phoneme.EndSample - 160, HertzMilli: part.baseF0 - int64((i%2)+1)*4000},
		}
		segment.Phonemes = append(segment.Phonemes, phoneme)
		cursor += part.length
	}
	return segment, nil
}

func synthesizeSegment(project *Project, segment Segment, seed int64) {
	for _, phoneme := range segment.Phonemes {
		state := uint64(seed) ^ uint64(phoneme.StartSample*6364136223846793005)
		var phase uint64
		for sampleIndex := phoneme.StartSample; sampleIndex < phoneme.EndSample; sampleIndex++ {
			var sample int64
			switch phoneme.Kind {
			case PhonemeVowel:
				f0 := f0At(phoneme, sampleIndex)
				increment := f0 * (int64(1) << 32) / (1000 * project.SampleRate)
				phase += uint64(increment)
				wave := triangleWave(phase)
				harmonic := triangleWave(phase * 2)
				amplitude := int64(12000) * phoneme.Weight / InternalScale
				sample = (wave*amplitude + harmonic*amplitude/5) / 32768
			case PhonemeConsonant:
				state ^= state << 13
				state ^= state >> 7
				state ^= state << 17
				noise := int64(int16(state >> 48))
				amplitude := int64(10500) * phoneme.Weight / InternalScale
				sample = noise * amplitude / 32768
			default:
				sample = 0
			}
			if sample > 32767 {
				sample = 32767
			}
			if sample < -32768 {
				sample = -32768
			}
			project.PCM[sampleIndex] = int16(sample)
		}
	}
}

func triangleWave(phase uint64) int64 {
	x := int64((phase >> 16) & 0xffff)
	if x < 32768 {
		return x * 2
	}
	return (65535 - x) * 2
}

func f0At(phoneme Phoneme, sample int64) int64 {
	if len(phoneme.F0Points) == 0 {
		return phoneme.BaseF0MilliHz
	}
	previous := phoneme.F0Points[0]
	for _, point := range phoneme.F0Points {
		if point.AtSample >= sample {
			if point.AtSample == previous.AtSample {
				return point.HertzMilli
			}
			numer := (sample - previous.AtSample) * (point.HertzMilli - previous.HertzMilli)
			denom := point.AtSample - previous.AtSample
			return previous.HertzMilli + numer/denom
		}
		previous = point
	}
	return previous.HertzMilli
}

func synthesizeTruth(project *Project) (*Truth, error) {
	truth := &Truth{}
	for _, segment := range project.Segments() {
		frames, maxRaw, err := synthesizeFrameTruth(project, segment)
		if err != nil {
			return nil, err
		}
		for i := range frames {
			if maxRaw == 0 {
				frames[i].ExpectedEnvelope = 0
				continue
			}
			value, _, _, err := roundMulDiv(frames[i].ExpectedEnvelope, InternalScale, maxRaw, "truth_envelope", frames[i].ExpectedEnvelope, maxRaw)
			if err != nil {
				return nil, err
			}
			frames[i].ExpectedEnvelope = value
		}
		baseSum := int64(0)
		for _, frame := range frames {
			baseSum += frame.ExpectedEnvelope
		}
		baseLoudness := baseSum / int64(len(frames))
		requiredGain := segment.SessionGain
		if baseLoudness > 0 {
			value, _, _, err := roundMulDiv(project.TargetLoudness, InternalScale, baseLoudness, "truth_gain", project.TargetLoudness, baseLoudness)
			if err != nil {
				return nil, err
			}
			requiredGain = value
		}
		adjustment := requiredGain - segment.SessionGain
		if adjustment > project.GainLimit {
			adjustment = project.GainLimit
		}
		if adjustment < -project.GainLimit {
			adjustment = -project.GainLimit
		}
		applied := segment.SessionGain + adjustment
		if applied < project.GainMin {
			applied = project.GainMin
			adjustment = applied - segment.SessionGain
		}
		if applied > project.GainMax {
			applied = project.GainMax
			adjustment = applied - segment.SessionGain
		}
		var calibrated []int64
		var calibratedSum int64
		for _, frame := range frames {
			value, _, _, err := roundMulDiv(frame.ExpectedEnvelope, applied, InternalScale, "truth_calibrated", frame.ExpectedEnvelope, applied)
			if err != nil {
				return nil, err
			}
			calibrated = append(calibrated, value)
			calibratedSum += value
		}
		calibratedLoudness := calibratedSum / int64(len(calibrated))
		reached := abs64(calibratedLoudness-project.TargetLoudness) <= DefaultFrameTolerance
		truth.Segments = append(truth.Segments, SegmentTruth{
			SegmentID: segment.ID, GainAdjustment: adjustment, Calibrated: calibrated, Reached: reached,
		})
		for _, frame := range frames {
			segmentTruth := truth.Segments[len(truth.Segments)-1]
			frame.CalibratedEnvelope = segmentTruth.Calibrated[frame.FrameIndex]
			truth.Frames = append(truth.Frames, frame)
		}
	}
	return truth, nil
}

func synthesizeFrameTruth(project *Project, segment Segment) ([]FrameTruth, int64, error) {
	count := frameCount(segment, project.FrameLength, project.Hop)
	if count == 0 {
		return nil, 0, fmt.Errorf("segment %s has no frames", segment.ID)
	}
	weights := sampleWeights(segment)
	frames := make([]FrameTruth, 0, count)
	var maxRaw int64
	for index := 0; index < count; index++ {
		start := segment.StartSample + int64(index)*project.Hop
		end := start + project.FrameLength
		var energy int64
		var weighted int64
		var peakAbs int64
		peakPosition := 0
		for sampleIndex := start; sampleIndex < end; sampleIndex++ {
			sample := int64(project.PCM[sampleIndex])
			energy += sample * sample
			weight := weights[sampleIndex-start]
			weighted += sample * sample * weight
			abs := sample
			if abs < 0 {
				abs = -abs
			}
			if abs > peakAbs {
				peakAbs = abs
				peakPosition = int(sampleIndex - start)
			}
		}
		weightedMean := weighted / project.FrameLength
		weightedMean /= InternalScale
		raw := isqrt(weightedMean) * InternalScale / 32768
		if raw > maxRaw {
			maxRaw = raw
		}
		frames = append(frames, FrameTruth{
			SegmentID: segment.ID, FrameIndex: index, StartSample: start, EndSample: end,
			Energy: energy, PeakPosition: peakPosition, ExpectedEnvelope: raw,
		})
	}
	return frames, maxRaw, nil
}
