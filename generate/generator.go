package generate

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"math/rand"

	"dialoguefit/model"
)

const DefaultSeed int64 = 20261126

var durationTable = [6][]int{
	{1120, 2240, 800, 1920, 1120, 800, 1760, 960, 640, 2240},
	{800, 2080, 1120, 1600, 960, 960, 2080, 960, 1120, 1600},
	{1120, 1920, 960, 2240, 1120, 960, 1920, 1120, 800, 2240},
	{960, 1920, 1120, 2240, 960, 960, 1920, 960, 800, 1920},
	{960, 1760, 800, 1760, 800, 1120, 1920, 1120, 1120, 2080},
	{1120, 1920, 1120, 1920, 1120, 1120, 2240, 800, 640, 1760},
}

var weightTable = [6][]float64{
	{0.47472426882897234, 1.836429113456267, 0.6062107614615975, 1.4128628477399547, 1, 0.4847938284458314, 1.83641676325592, 0.31064157761577643, 1, 1.33514796068424},
	{0.2883921229937639, 1.2729789627849162, 0.3814391963171283, 1.400542772486625, 1, 0.3396090669909209, 1.75586693577563, 0.3909286303889147, 1, 1.5850796179370905},
	{0.6177984887890429, 1.5999657495784405, 0.4184907713584457, 1.6763067160792418, 1, 0.37817715317923145, 1.4287238210635254, 0.6001468629834201, 1, 1.468345581686846},
	{0.6641899732336993, 1.462430734919973, 0.30648852772401786, 1.5347149630261845, 1, 0.36461159242693997, 1.5988078543138031, 0.28443178515646583, 1, 1.7773359725607212},
	{0.2969338664796079, 1.850482653396715, 0.3298588989254518, 1.315436203852172, 1, 0.25121871091115383, 1.2630820152766065, 0.5803130614154831, 1, 1.253616219681689},
	{0.41475058443941704, 1.8045930150878684, 0.4795077840450851, 1.555944594157901, 1, 0.47958215437105345, 1.3799060077644185, 0.6418139526658984, 1, 1.2041328108240879},
}

var paramsTable = []model.FitParams{
	{TargetSamples: 14187, MaxStretch: 1.20, MinSilenceSamples: 480, StressToleranceSamples: 24},
	{TargetSamples: 13960, MaxStretch: 1.25, MinSilenceSamples: 480, StressToleranceSamples: 24},
	{TargetSamples: 15302, MaxStretch: 1.30, MinSilenceSamples: 480, StressToleranceSamples: 24},
	{TargetSamples: 14296, MaxStretch: 1.20, MinSilenceSamples: 480, StressToleranceSamples: 24},
	{TargetSamples: 14056, MaxStretch: 1.25, MinSilenceSamples: 480, StressToleranceSamples: 24},
	{TargetSamples: 14566, MaxStretch: 1.30, MinSilenceSamples: 480, StressToleranceSamples: 24},
}

type frameConstruction struct {
	phonemeIndex int
	amplitude    int16
	stress       bool
}

func GenerateProject(seed int64) *model.Project {
	if seed == 0 {
		seed = DefaultSeed
	}
	cfg := model.EnvelopeConfig{FrameLength: 160, FrameShift: 160, SmoothWindow: 3}
	project := &model.Project{
		SchemaVersion:    "1",
		Seed:             seed,
		AlgorithmVersion: model.AlgorithmVersion,
		Envelope:         cfg,
		Dialogues:        make([]model.Dialogue, 0, 6),
	}
	for di := 0; di < 6; di++ {
		rng := rand.New(rand.NewSource(seed + int64(di)*1009))
		dialogueID := "dlg-" + twoDigit(di+1)
		d := model.Dialogue{ID: dialogueID, Params: paramsTable[di]}
		frames := make([]frameConstruction, 0)
		cursor := 0
		for pi := 0; pi < 10; pi++ {
			category := categoryFor(pi)
			duration := durationTable[di][pi]
			frameCount := duration / cfg.FrameShift
			stressFrame := -1
			if category == model.CategoryVowel {
				stressFrame = stressRelativeFrame(pi, frameCount)
			}
			ph := model.Phoneme{
				ID:             dialogueID + "-p" + twoDigit(pi),
				Index:          pi,
				Category:       category,
				StartSample:    cursor,
				EndSample:      cursor + duration,
				F0StepSamples:  cfg.FrameShift,
				F0:             make([]float64, frameCount),
				LoudnessWeight: weightTable[di][pi],
				Stretchable:    category != model.CategorySilence,
			}
			baseF0 := 96 + rng.Float64()*24
			for fi := 0; fi < frameCount; fi++ {
				amp := constructAmplitude(category, fi, frameCount, stressFrame, rng)
				frames = append(frames, frameConstruction{phonemeIndex: pi, amplitude: amp, stress: fi == stressFrame})
				if category == model.CategorySilence {
					ph.F0[fi] = 0
				} else if category == model.CategoryVowel {
					peak := math.Exp(-math.Pow(float64(fi-stressFrame), 2) / (2 * 2.2 * 2.2))
					ph.F0[fi] = baseF0 + 58*peak + rng.NormFloat64()*0.8
				} else {
					ph.F0[fi] = baseF0 + 18*math.Sin(float64(fi+pi)) + rng.NormFloat64()*0.7
				}
			}
			d.Phonemes = append(d.Phonemes, ph)
			cursor += duration
		}
		samples := make([]int16, cursor)
		rawBytes := make([]byte, 0, cursor*2)
		for fi, fc := range frames {
			for s := 0; s < cfg.FrameLength; s++ {
				global := fi*cfg.FrameLength + s
				value := fc.amplitude
				if global%2 == 1 {
					value = -value
				}
				samples[global] = value
				var u [2]byte
				binary.LittleEndian.PutUint16(u[:], uint16(value))
				rawBytes = append(rawBytes, u[0], u[1])
			}
		}
		d.PCM = model.PCMDescriptor{
			Encoding: "pcm_s16le", SampleRate: 48000, SampleCount: cursor,
			Data: base64.StdEncoding.EncodeToString(rawBytes), Samples: samples,
		}
		project.Dialogues = append(project.Dialogues, d)
	}
	return project
}

func GenerateTruth(project *model.Project) *model.TruthFile {
	truth := &model.TruthFile{SchemaVersion: "1", AlgorithmVersion: project.AlgorithmVersion}
	for _, d := range project.Dialogues {
		frameRaw := make([]float64, 0)
		for _, ph := range d.Phonemes {
			for fi := 0; fi < ph.Duration()/project.Envelope.FrameLength; fi++ {
				amp := analyticAmplitude(ph.Index, fi, ph.Duration()/project.Envelope.FrameLength)
				frameRaw = append(frameRaw, amp/32768.0)
			}
		}
		smoothed := analyticSmooth(frameRaw, project.Envelope.SmoothWindow)
		values := analyticNormalize(smoothed)
		stretched := constructStretched(d)
		stress := constructStress(d, stretched)
		td := model.TruthDialogue{
			DialogueID:       d.ID,
			SampleRate:       d.PCM.SampleRate,
			SampleCount:      d.PCM.SampleCount,
			ContentHash:      model.ContentHash(d),
			AlgorithmVersion: project.AlgorithmVersion,
			EnvelopeParams:   project.Envelope,
			FitParams:        d.Params,
			FrameValues:      values,
			Stress:           stress,
			Stretched:        stretched,
		}
		td.Fingerprint = model.TruthFingerprint(td)
		truth.Dialogues = append(truth.Dialogues, td)
	}
	return truth
}

func constructAmplitude(category model.Category, frame, frameCount, stressFrame int, rng *rand.Rand) int16 {
	_ = rng
	jitter := 0
	switch category {
	case model.CategorySilence:
		return clampInt16(2600 + jitter/4)
	case model.CategoryConsonant:
		base := 9000 + (frame%4)*1400 + jitter
		return clampInt16(base)
	default:
		if frame == stressFrame {
			return 31800
		}
		distance := math.Abs(float64(frame - stressFrame))
		level := 23500 - int(distance*1700) + jitter
		if level < 17000 {
			level = 17000
		}
		return clampInt16(level)
	}
}

// analyticAmplitude reconstructs the deterministic construction level without
// reading extractor output. The stress frame is the declared peak; ordinary
// frame levels use the same closed-form contour.
func analyticAmplitude(phonemeIndex, frame, frameCount int) float64 {
	category := categoryFor(phonemeIndex)
	stressFrame := -1
	if category == model.CategoryVowel {
		stressFrame = stressRelativeFrame(phonemeIndex, frameCount)
	}
	switch category {
	case model.CategorySilence:
		return 2600
	case model.CategoryConsonant:
		return float64(9000 + (frame%4)*1400)
	default:
		if frame == stressFrame {
			return 31800
		}
		distance := math.Abs(float64(frame - stressFrame))
		level := 23500 - int(distance*1700)
		if level < 17000 {
			level = 17000
		}
		return float64(level)
	}
}

func categoryFor(index int) model.Category {
	switch index {
	case 4, 8:
		return model.CategorySilence
	case 1, 3, 6, 9:
		return model.CategoryVowel
	default:
		return model.CategoryConsonant
	}
}

func stressRelativeFrame(index, frameCount int) int {
	switch index {
	case 1:
		return frameCount / 2
	case 3:
		return frameCount*2/3 - 1
	case 6:
		return frameCount/3 + 1
	default:
		return frameCount * 2 / 3
	}
}

func constructStress(d model.Dialogue, segments []model.StretchSegment) []model.TruthStress {
	out := make([]model.TruthStress, 0, 4)
	for _, ph := range d.Phonemes {
		if ph.Category != model.CategoryVowel {
			continue
		}
		rel := stressRelativeFrame(ph.Index, ph.Duration()/160)
		sample := ph.StartSample + rel*160
		aligned := warpConstructionSample(sample, d.Phonemes, segments)
		out = append(out, model.TruthStress{PhonemeID: ph.ID, Index: ph.Index, Sample: sample, AlignedSample: aligned})
	}
	return out
}

func constructStretched(d model.Dialogue) []model.StretchSegment {
	// Generator-owned construction oracle. It intentionally does not call
	// fit.Allocate, so truth is not inferred from processing output.
	n := len(d.Phonemes)
	added := make([]int, n)
	hit := make([]bool, n)
	basis := make([]string, n)
	gap := d.Params.TargetSamples - d.PCM.SampleCount
	reserve := 0
	for i, ph := range d.Phonemes {
		if ph.Category == model.CategorySilence {
			deficit := d.Params.MinSilenceSamples - ph.Duration()
			if deficit > 0 {
				added[i] = deficit
				reserve += deficit
				basis[i] = "silence-hold-minimum"
			} else {
				basis[i] = "silence-preserved"
			}
		}
	}
	remaining := gap - reserve
	type candidate struct{ index int }
	candidates := make([]candidate, 0)
	for i, ph := range d.Phonemes {
		if ph.Category != model.CategorySilence && ph.Stretchable && ph.LoudnessWeight > 0 {
			candidates = append(candidates, candidate{i})
		}
	}
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			a, b := candidates[i].index, candidates[j].index
			if d.Phonemes[b].LoudnessWeight > d.Phonemes[a].LoudnessWeight ||
				(d.Phonemes[b].LoudnessWeight == d.Phonemes[a].LoudnessWeight && b < a) {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	for _, c := range candidates {
		i := c.index
		if remaining <= 0 {
			basis[i] = "weighted-waterfall-no-gap"
			continue
		}
		ph := d.Phonemes[i]
		capExtra := int(float64(ph.Duration()) * (d.Params.MaxStretch - 1))
		take := capExtra
		if remaining < take {
			take = remaining
		}
		if take < 0 {
			take = 0
		}
		added[i] = take
		remaining -= take
		if take >= capExtra && capExtra > 0 {
			hit[i] = true
			basis[i] = "weighted-waterfall-cap-hit"
		} else if take > 0 {
			basis[i] = "weighted-waterfall-partial"
		} else {
			basis[i] = "weighted-waterfall-no-gap"
		}
	}
	segments := make([]model.StretchSegment, n)
	cursor := 0
	for i, ph := range d.Phonemes {
		if i == 0 {
			cursor = ph.StartSample
		} else {
			cursor += ph.StartSample - d.Phonemes[i-1].EndSample
		}
		duration := ph.Duration() + added[i]
		segments[i] = model.StretchSegment{
			PhonemeID: ph.ID, Index: ph.Index, SourceStart: ph.StartSample, SourceEnd: ph.EndSample,
			StartSample: cursor, EndSample: cursor + duration,
			StretchFactor: float64(duration) / float64(ph.Duration()), AddedSamples: added[i],
			HitCap: hit[i], Basis: basis[i],
		}
		cursor += duration
	}
	return segments
}

func warpConstructionSample(sample int, phonemes []model.Phoneme, segments []model.StretchSegment) int {
	for i, ph := range phonemes {
		if sample >= ph.StartSample && sample < ph.EndSample {
			offset := sample - ph.StartSample
			duration := ph.Duration()
			newDuration := segments[i].EndSample - segments[i].StartSample
			scaled := offset*newDuration*2 + duration
			return segments[i].StartSample + scaled/(2*duration)
		}
		if sample == ph.EndSample {
			return segments[i].EndSample
		}
	}
	return sample
}

func analyticSmooth(raw []float64, window int) []float64 {
	left := (window - 1) / 2
	right := window / 2
	out := make([]float64, len(raw))
	for i := range raw {
		lo := maxInt(0, i-left)
		hi := minInt(len(raw)-1, i+right)
		sum := 0.0
		n := 0
		for j := lo; j <= hi; j++ {
			sum += raw[j]
			n++
		}
		out[i] = sum / float64(n)
	}
	return out
}

func analyticNormalize(values []float64) []float64 {
	maxValue := 0.0
	for _, v := range values {
		if v > maxValue {
			maxValue = v
		}
	}
	out := make([]float64, len(values))
	if maxValue == 0 {
		return out
	}
	for i, v := range values {
		out[i] = v / maxValue
	}
	return out
}

func clampInt16(v int) int16 {
	if v > 32767 {
		return 32767
	}
	if v < 1 {
		return 1
	}
	return int16(v)
}

func twoDigit(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
