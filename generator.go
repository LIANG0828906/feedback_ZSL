package dialoguecal

import (
	"fmt"
	"sort"
)

type GenerateOptions struct {
	Seed                int64
	SampleRate          int
	FrameRateHz         int
	DialogueCount       int
	SegmentsPerDialogue int
	PhonemesPerSegment  int
	BaseFrames          int
}

func DefaultGenerateOptions(seed int64) GenerateOptions {
	return GenerateOptions{
		Seed: seed, SampleRate: 48000, FrameRateHz: 50,
		DialogueCount: 3, SegmentsPerDialogue: 3, PhonemesPerSegment: 2,
		BaseFrames: 20,
	}
}

type genRand struct {
	state uint64
}

func newGenRand(seed int64) *genRand {
	// SplitMix64 initialization ensures even tiny seed changes diverge while
	// keeping the sequence entirely local and reproducible.
	x := uint64(seed) + 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	x ^= x >> 31
	if x == 0 {
		x = 0x9e3779b97f4a7c15
	}
	return &genRand{state: x}
}

func (r *genRand) next() uint64 {
	x := r.state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.state = x
	return x
}

func (r *genRand) intn(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(r.next() % uint64(n))
}

func classFor(phonemeIndex int) (string, int64) {
	switch phonemeIndex % 4 {
	case 0:
		return "vowel", 2400
	case 1:
		return "fricative", 1100
	case 2:
		return "plosive", 3300
	default:
		return "nasal", 1700
	}
}

func distribute(total, parts int) []int {
	if parts <= 0 {
		return nil
	}
	out := make([]int, parts)
	base := total / parts
	rem := total % parts
	for i := range out {
		out[i] = base
		if i < rem {
			out[i]++
		}
	}
	return out
}

// Generate creates the deterministic fixture and its independent per-frame
// truth list. PCM is synthesized as mono signed 16-bit; no external asset is
// read or written here.
func Generate(opts GenerateOptions) (*GeneratedProject, error) {
	if opts.SampleRate <= 0 || opts.FrameRateHz <= 0 || opts.DialogueCount <= 0 ||
		opts.SegmentsPerDialogue <= 0 || opts.PhonemesPerSegment <= 0 || opts.BaseFrames <= 0 {
		return nil, &Error{Code: CodeCal, Path: "generate.options", Message: "invalid generator options"}
	}
	frameSamples := opts.SampleRate / opts.FrameRateHz
	if frameSamples <= 0 {
		return nil, &Error{Code: CodeCal, Field: "frameRateHz", OperandA: int64(opts.FrameRateHz), OperandB: int64(opts.SampleRate), Message: "frame is shorter than one sample"}
	}

	p := &Project{
		ID:                  fmt.Sprintf("generated-%d", opts.Seed),
		SourceVersion:       VersionV3,
		FrameRateHz:         opts.FrameRateHz,
		SampleRate:          opts.SampleRate,
		SessionGain:         300,
		GainUpperBound:      120000,
		Tolerance:           200,
		MaxParallelism:      2,
		WorkerBufferSamples: 4096,
		QueueBudget:         4,
		QuantizationVersion: DefaultQuantizationVersion,
		ParallelVersion:     DefaultParallelVersion,
		ConversionVersion:   DefaultConversionVersion,
		AlgorithmVersion:    DefaultAlgorithmVersion,
	}
	p.CalibrationMarkers = []CalibrationMarker{{
		Path: "$.project.calibration.markers.session", Kind: "session", Value: "designer-approved",
	}}
	rng := newGenRand(opts.Seed)
	for di := 0; di < opts.DialogueCount; di++ {
		frames := opts.BaseFrames + (di%3)*2
		sampleCount := frames * frameSamples
		d := Dialogue{
			ID: fmt.Sprintf("dlg-%02d", di+1),
			PCM: PCM{
				Encoding: "pcm_s16le", SampleRate: opts.SampleRate, Channels: 1,
				Samples: make([]int16, sampleCount), DeclaredSampleCount: int64(sampleCount),
			},
		}
		segFrames := distribute(frames, opts.SegmentsPerDialogue)
		cursor := 0
		for si, sf := range segFrames {
			start := cursor
			end := cursor + sf*frameSamples
			cursor = end
			gain := int64(-1500 + di*180 + si*470)
			seg := Segment{
				ID: fmt.Sprintf("seg-%02d-%02d", di+1, si+1), DialogueID: d.ID,
				StartSample: int64(start), EndSample: int64(end), Gain: gain,
			}
			phFrameParts := distribute(sf, opts.PhonemesPerSegment)
			phCursor := start
			for pi, pf := range phFrameParts {
				phStart := phCursor
				phEnd := phCursor + pf*frameSamples
				phCursor = phEnd
				class, amp := classFor(pi + si)
				weight := 9000 + (di*120+si*70+pi*35)*10
				ph := Phoneme{
					ID:          fmt.Sprintf("ph-%02d-%02d-%02d", di+1, si+1, pi+1),
					StartSample: int64(phStart), EndSample: int64(phEnd),
					Class: class, Weight: weight,
				}
				seg.Phonemes = append(seg.Phonemes, ph)
				variation := int64(di*37 + si*53 + pi*29)
				for n := phStart; n < phEnd; n++ {
					a := amp + variation
					v := rng.intn(a*2+1) - a
					if n == phStart {
						v = a
					}
					if v > 32767 {
						v = 32767
					}
					if v < -32768 {
						v = -32768
					}
					d.PCM.Samples[n] = int16(v)
				}
			}
			seg.Markers = []CalibrationMarker{{
				Path:      fmt.Sprintf("$.sceneGroups[].dialogues.%s.segments.%s.calibrationMarkers", d.ID, seg.ID),
				SegmentID: seg.ID, Kind: "designer-note", Value: fmt.Sprintf("take-%d", si+1),
			}}
			d.Segments = append(d.Segments, seg)
		}
		p.Dialogues = append(p.Dialogues, d)
	}
	p.Sort()
	p.Organization = "scene-grouped"
	p.SceneGroups = []SceneGroup{{ID: "scene-01"}}
	for _, d := range p.Dialogues {
		p.SceneGroups[0].DialogueIDs = append(p.SceneGroups[0].DialogueIDs, d.ID)
	}

	intervals, err := BuildIntervals(p)
	if err != nil {
		return nil, err
	}
	featuresByInterval := make(map[string][]FrameFeature, len(intervals))
	var maxEnergy int64
	for _, iv := range intervals {
		fs, err := ComputeIntervalFeatures(p, iv)
		if err != nil {
			return nil, err
		}
		featuresByInterval[iv.ID] = fs
		for _, f := range fs {
			if f.Energy > maxEnergy {
				maxEnergy = f.Energy
			}
		}
	}
	p.FrameEnergyLimit = maxEnergy + maxEnergy/5 + 1

	// Choose one common, reachable target. The target is rounded to the
	// permille-compatible grid so a v1 round trip has zero scale residual.
	var maxMean int64 = LoudnessFloor
	for _, iv := range intervals {
		d := dialogueByID(p, iv.DialogueID)
		seg := segmentByID(d, iv.SegmentID)
		measured, _, err := MeasureFeatures(p, *seg, featuresByInterval[iv.ID])
		if err != nil {
			return nil, err
		}
		if measured > maxMean {
			maxMean = measured
		}
	}
	provisionalTarget := maxMean + 5000
	q, _ := divRound(provisionalTarget, 10, RoundHalfAway)
	p.TargetLoudness = q * 10
	if p.TargetLoudness < LoudnessFloor {
		p.TargetLoudness = LoudnessFloor
	}
	if p.TargetLoudness > LoudnessCeiling {
		p.TargetLoudness = LoudnessCeiling
	}

	truth := make([]FrameTruth, 0)
	for _, iv := range intervals {
		d := dialogueByID(p, iv.DialogueID)
		seg := segmentByID(d, iv.SegmentID)
		features := featuresByInterval[iv.ID]
		cal, _, requested, applied, err := CalibrateFeatures(p, *seg, features)
		if err != nil {
			return nil, err
		}
		for i, f := range features {
			base := FrameBaseLoudness(f, p.SessionGain, seg.Gain)
			truth = append(truth, FrameTruth{
				DialogueID: f.DialogueID, SegmentID: f.SegmentID, Frame: f.Frame,
				StartSample: f.StartSample, EndSample: f.EndSample,
				Energy: f.Energy, ZeroCrossings: f.ZeroCrossings,
				TransientPeak: f.TransientPeak, Weight: f.Weight,
				BaseLoudness: base, RequestedGain: requested, AppliedGain: applied,
				CalibratedLoudness: cal[i].CalibratedLoudness,
			})
		}
	}
	sort.Slice(truth, func(i, j int) bool {
		if truth[i].DialogueID != truth[j].DialogueID {
			return truth[i].DialogueID < truth[j].DialogueID
		}
		return truth[i].Frame < truth[j].Frame
	})
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	gp := &GeneratedProject{Project: p, Truth: truth, Docs: make(map[SchemaVersion][]byte)}
	for _, v := range []SchemaVersion{VersionV1, VersionV2, VersionV3} {
		exp, err := ExportProject(p, v)
		if err != nil {
			return nil, err
		}
		gp.Docs[v] = exp.Data
	}
	return gp, nil
}
