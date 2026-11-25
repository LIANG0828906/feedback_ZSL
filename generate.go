package lipsync

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
)

type InjectKind string

const (
	InjectNone  InjectKind = ""
	InjectOrder InjectKind = "order"
	InjectLabel InjectKind = "label"
	InjectVis   InjectKind = "vis"
	InjectStage InjectKind = "stage"
)

type GenerateOptions struct {
	Seed           int64
	DialogueCount  int
	Inject         InjectKind
	TargetDialogue int
}

func DefaultGenerateOptions() GenerateOptions {
	return GenerateOptions{
		Seed:           0x51A7C0DE,
		DialogueCount:  4,
		Inject:         InjectNone,
		TargetDialogue: 2,
	}
}

func GenerateProject(opts GenerateOptions) *Project {
	if opts.Seed == 0 {
		opts.Seed = DefaultGenerateOptions().Seed
	}
	if opts.DialogueCount <= 0 {
		opts.DialogueCount = 4
	}
	if opts.TargetDialogue < 0 || opts.TargetDialogue >= opts.DialogueCount {
		opts.TargetDialogue = 0
	}

	project := &Project{
		ID:               "local-diagnostic-project",
		Seed:             opts.Seed,
		AlgorithmVersion: AlgorithmVersion,
		Recipe: Recipe{
			FPS:                    30,
			MaxPhonemesPerPack:     3,
			MinVisibleFrames:       2,
			SpeakerChannels:        4,
			FramePackBudget:        3,
			Quantization:           QuantizeDown,
			MergeAffinityThreshold: 0,
			MaxDeferFrames:         180,
			RecipeVersion:          "recipe-seed-v1",
		},
		Dialogues: make([]*Dialogue, 0, opts.DialogueCount),
	}

	for d := 0; d < opts.DialogueCount; d++ {
		seed := opts.Seed + int64(d)*1000003
		rng := rand.New(rand.NewSource(seed))
		dialogueID := fmt.Sprintf("dlg-%03d", d)
		phonemeCount := 9 + rng.Intn(4)
		if opts.Inject == InjectVis && d == opts.TargetDialogue {
			phonemeCount = 7
		}
		phonemes := make([]Phoneme, 0, phonemeCount)
		cursor := 0
		for i := 0; i < phonemeCount; i++ {
			class := ClassVowel
			switch {
			case i%4 == 3:
				class = ClassSilence
			case i%2 == 0:
				class = ClassVowel
			default:
				class = ClassConsonant
			}
			duration := 2600 + rng.Intn(2600)
			if class == ClassSilence {
				duration = 1200 + rng.Intn(1000)
			}
			if opts.Inject == InjectVis && d == opts.TargetDialogue {
				duration = 70 + rng.Intn(90)
			}
			start := cursor
			end := start + duration
			cursor = end

			weight := 120 + rng.Intn(560)
			unit := WeightPermille
			if i%5 == 4 {
				unit = WeightPermyriad
				weight = 900 + rng.Intn(3300)
			}
			ph := Phoneme{
				ID:          fmt.Sprintf("%s-ph-%03d", dialogueID, i),
				Index:       i,
				StartSample: start,
				EndSample:   end,
				Class:       class,
				Weight:      weight,
				WeightUnit:  unit,
			}
			if i < phonemeCount-1 {
				affinity := 2500 + rng.Intn(6500)
				ph.AffinityToNext = &affinity
			}
			phonemes = append(phonemes, ph)
		}

		for i := range phonemes {
			if phonemes[i].AffinityToNext == nil {
				continue
			}
			switch i {
			case 0, 4:
				tie := 8421
				phonemes[i].AffinityToNext = &tie
			case 1, 5:
				tie := 6733
				phonemes[i].AffinityToNext = &tie
			}
		}

		sampleCount := cursor + 2048
		samples := generatePCMSamples(seed, sampleCount)
		pcmHash := hashPCMSamples(samples)
		dialogue := &Dialogue{
			ID:         dialogueID,
			ShardIndex: d,
			PCM: PCMDescription{
				Encoding:    "pcm_s16le",
				Channels:    1,
				SampleRate:  16000,
				SampleCount: sampleCount,
				Seed:        seed,
				SHA256:      pcmHash,
				Samples:     samples,
			},
			Phonemes: phonemes,
		}
		project.Dialogues = append(project.Dialogues, dialogue)
	}

	applyInjection(project, opts)
	EnsureDialogueHashes(project)
	return project
}

func generatePCMSamples(seed int64, count int) []int16 {
	samples := make([]int16, count)
	state := uint32(seed)
	if state == 0 {
		state = 1
	}
	for i := 0; i < count; i++ {
		state = state*1664525 + 1013904223
		noise := int16(state >> 18)
		phase := (i * 137) % 145
		triangle := phase*2 - 145
		if triangle < 0 {
			triangle = -triangle
		}
		tone := int16((triangle - 72) * 18)
		samples[i] = tone/3 + noise/10
	}
	return samples
}

func hashPCMSamples(samples []int16) string {
	h := sha256.New()
	var buf [2]byte
	for _, sample := range samples {
		binary.LittleEndian.PutUint16(buf[:], uint16(sample))
		_, _ = h.Write(buf[:])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func applyInjection(p *Project, opts GenerateOptions) {
	if opts.Inject == InjectNone || opts.Inject == InjectStage {
		return
	}
	if opts.TargetDialogue >= len(p.Dialogues) {
		opts.TargetDialogue = 0
	}
	d := p.Dialogues[opts.TargetDialogue]
	switch opts.Inject {
	case InjectOrder:
		if len(d.Phonemes) > 4 {
			d.Phonemes[3].StartSample = d.Phonemes[3].EndSample + 512
		}
	case InjectLabel:
		if len(d.Phonemes) > 2 {
			d.Phonemes[2].Class = PhonemeClass("out_of_bounds_viseme")
		}
	case InjectVis:
		p.Recipe.MinVisibleFrames = 8
	}
}
