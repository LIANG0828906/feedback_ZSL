package dialogue

import (
	"fmt"
)

// GeneratorOptions controls the deterministic sample set.
type GeneratorOptions struct {
	Seed      int64
	ClipCount int
}

type TruthManifest struct {
	Seed           int64        `json:"seed"`
	DialogueID     string       `json:"dialogue_id"`
	SampleRate     int          `json:"sample_rate"`
	FrameRate      int          `json:"frame_rate"`
	TargetAmpMicro int64        `json:"target_amp_micro"`
	TargetLoudMDB  int64        `json:"target_loud_mdb"`
	Clips          []*ClipTruth `json:"clips"`
}

type GeneratedSamples struct {
	Project *Project
	Truth   *TruthManifest
	Docs    map[int][]byte
}

func envelopePermille(sampleInClip, frameLength int) int64 {
	frame := sampleInClip / frameLength
	j := sampleInClip % frameLength
	switch frame {
	case 0:
		if frameLength <= 1 {
			return 1000
		}
		return 200 + 800*int64(j)/int64(frameLength-1)
	case 1:
		return 1000
	case 2:
		return 0
	case 3:
		return 900
	default:
		if frameLength <= 1 {
			return 0
		}
		return 800 - 800*int64(j)/int64(frameLength-1)
	}
}

func triangleSample(index, period int64, amplitude int64) int64 {
	phase := index % period
	half := period / 2
	if phase < half {
		return -amplitude + (2*amplitude*phase)/half
	}
	ph := phase - half
	return amplitude - (2*amplitude*ph)/half
}

// GenerateProject builds the fixed-seed dialogue and its generator-owned truth.
func GenerateProject(opts GeneratorOptions) (*Project, *TruthManifest, error) {
	if opts.Seed == 0 {
		opts.Seed = FixedSeed
	}
	if opts.ClipCount <= 0 {
		opts.ClipCount = 4
	}
	sampleRate := DefaultSampleRate
	fps := DefaultFrameRate
	frameLength := sampleRate / fps
	clipFrames := 5
	clipLength := frameLength * clipFrames
	stride := frameLength * (clipFrames + 1)
	scenePaths := [][]string{
		{"prologue"},
		{"dialogue", "hero"},
		{"dialogue", "guard"},
		{"epilogue"},
	}
	gains := []int64{10_000, 10_200, 9_800, 10_100}
	freqs := []int64{250, 375, 500, 600}

	targetAmp := int64(72_000)
	targetLoud, _, err := LinearToDBMilli(targetAmp, "target_loud_mdb")
	if err != nil {
		return nil, nil, err
	}
	project := &Project{
		SchemaVersion:    3,
		SchemaID:         "dialogue-v3",
		SourceFile:       "dialogue-v3.json",
		DialogueID:       fmt.Sprintf("dlg_seed_%d", opts.Seed),
		Seed:             opts.Seed,
		SampleRate:       sampleRate,
		FrameRate:        fps,
		SessionGainBP:    10_000,
		TargetAmpMicro:   targetAmp,
		TargetLoudMDB:    targetLoud,
		MaxAdjustBP:      2_500,
		TotalBudgetBP:    12_000,
		AlgorithmVersion: AlgorithmVersion,
		QuantVersion:     QuantVersion,
		Clips:            make([]*Clip, 0, opts.ClipCount),
		Markers:          make([]Marker, 0, opts.ClipCount),
	}

	for i := 0; i < opts.ClipCount; i++ {
		period := int64(sampleRate) / freqs[i%len(freqs)]
		start := int64(i * stride)
		samples := make([]int16, clipLength)
		transientOffset := frameLength + frameLength/2
		for j := 0; j < clipLength; j++ {
			envPM := envelopePermille(j, frameLength)
			v := triangleSample(int64(j), period, 5_200)
			v = v * envPM / PermilleScale
			samples[j] = int16(v)
		}
		samples[transientOffset] = 26_000
		id := fmt.Sprintf("clip-%02d", i+1)
		scenePath := append([]string(nil), scenePaths[i%len(scenePaths)]...)
		gain := gains[i%len(gains)]
		hash := AudioHash(sampleRate, start, samples)
		clip := &Clip{
			ID:                  id,
			ScenePath:           scenePath,
			StartSample:         start,
			Samples:             samples,
			DeclaredSampleCount: len(samples),
			GainBP:              gain,
			ToneHz:              int(freqs[i%len(freqs)]),
			TransientSample:     start + int64(transientOffset),
			AudioHash:           hash,
		}
		project.Clips = append(project.Clips, clip)
		project.Markers = append(project.Markers, Marker{
			ID:      fmt.Sprintf("marker-%s-transient", id),
			ClipID:  id,
			Kind:    "transient_peak",
			Sample:  clip.TransientSample,
			FrameNo: FrameOfSample(clip.TransientSample, sampleRate, fps),
			Value:   26_000,
		})
	}
	project.SceneTree = BuildSceneTree(project.Clips)

	truth, truthErr := BuildReferenceTruth(project)
	if truthErr != nil {
		return nil, nil, truthErr
	}
	return project, truth, nil
}

func buildSampleChecks(project *Project, clip *Clip, frameLength int) []SampleTruth {
	period := int64(project.SampleRate / clip.ToneHz)
	offsets := []int{
		0,
		frameLength + frameLength/2, // injected transient
		2*frameLength + 10,          // silence
		frameLength + int(period/2), // positive triangle peak
		int(period / 4),             // first analytic zero crossing
	}
	checks := make([]SampleTruth, 0, len(offsets))
	for _, offset := range offsets {
		if offset < 0 || offset >= len(clip.Samples) {
			continue
		}
		pcm := clip.Samples[offset]
		amp, _ := SampleToAmpMicro(pcm, "sample_truth.amp_micro")
		envPM := envelopePermille(offset, frameLength)
		globalSample := clip.StartSample + int64(offset)
		checks = append(checks, SampleTruth{
			SampleNo:   globalSample,
			ClipSample: offset,
			PCM:        int(pcm),
			AmpMicro:   amp,
			EnvelopeBP: envPM * 10,
			FrameNo:    FrameOfSample(globalSample, project.SampleRate, project.FrameRate),
		})
	}
	return checks
}

// BuildSceneTree derives a nested scene tree from clip scene paths.
func BuildSceneTree(clips []*Clip) []SceneGroup {
	rootPtrs := make([]*SceneGroup, 0)
	index := make(map[string]*SceneGroup)
	for _, clip := range clips {
		path := clip.ScenePath
		if len(path) == 0 {
			path = []string{"default"}
		}
		currentPath := ""
		var parentGroup *SceneGroup
		for depth, name := range path {
			if depth == 0 {
				currentPath = name
			} else {
				currentPath += "/" + name
			}
			g, ok := index[currentPath]
			if !ok {
				newGroup := &SceneGroup{Name: name}
				index[currentPath] = newGroup
				if depth == 0 {
					rootPtrs = append(rootPtrs, newGroup)
				} else {
					parentGroup.Children = append(parentGroup.Children, newGroup)
				}
				g = newGroup
			}
			parentGroup = g
		}
		if parentGroup != nil {
			parentGroup.ClipIDs = append(parentGroup.ClipIDs, clip.ID)
		}
	}
	roots := make([]SceneGroup, 0, len(rootPtrs))
	for _, g := range rootPtrs {
		roots = append(roots, *g)
	}
	return roots
}
