package lipsync

import (
	"fmt"
	"unicode/utf8"
)

// stableRNG is a fixed splitmix64 generator. Unlike math/rand, its sequence is
// owned by this project and cannot change with a Go toolchain update.
type stableRNG struct {
	state uint64
}

func newStableRNG(seed int64) *stableRNG {
	return &stableRNG{state: uint64(seed)}
}

func (r *stableRNG) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *stableRNG) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

type SampleSet struct {
	Seed     int64
	Projects []DialogueProject
	Truths   []Truth
}

var sampleTexts = []string{
	"你好，欢迎来到训练场。",
	"目标已确认，\n准备出发。",
	"注意左侧通道，\n保持安静。",
	"训练完成，\n请领取补给。",
	"系统检测到异常，\n请立即撤离。",
	"你的表现很好，\n继续前进。",
}

var sampleFPS = []int{24, 30, 60, 24, 30, 60}
var sampleDirections = []string{"ceil", "floor", "round", "ceil", "round", "floor"}
var sampleFrameCounts = []int{71, 71, 73, 77, 84, 80}

// GenerateSampleSet creates deterministic projects and their analytic truth.
// No random source outside this function affects the result.
func GenerateSampleSet(seed int64, count int) (SampleSet, error) {
	if count <= 0 {
		return SampleSet{}, &DomainError{Code: CodeSchema, Field: "count", Message: "sample count must be positive"}
	}
	rng := newStableRNG(seed)
	set := SampleSet{Seed: seed, Projects: make([]DialogueProject, 0, count), Truths: make([]Truth, 0, count)}
	for i := 0; i < count; i++ {
		fps := sampleFPS[i%len(sampleFPS)]
		if i >= len(sampleFPS) {
			fps = []int{24, 30, 60}[rng.intn(3)]
		}
		direction := sampleDirections[i%len(sampleDirections)]
		frameCount := sampleFrameCounts[i%len(sampleFrameCounts)]
		if i >= len(sampleFrameCounts) {
			frameCount = 60 + rng.intn(31)
		}
		text := sampleTexts[i%len(sampleTexts)]
		runeCount := utf8.RuneCountInString(text)
		weights := make([]int, runeCount)
		for j := range weights {
			weights[j] = 1 + rng.intn(9)
		}
		segmentCount := 2
		if i%2 == 0 {
			segmentCount = 3
		}
		segmentWeights := make([]int, segmentCount)
		for j := range segmentWeights {
			segmentWeights[j] = 1 + rng.intn(3)
		}
		segmentFrames, _, err := allocateWithMinimum(frameCount, segmentWeights, 16)
		if err != nil {
			return SampleSet{}, fmt.Errorf("allocate sample segments: %w", err)
		}
		const sampleRate = 48000
		frameSamples := sampleRate / fps
		totalSamples := int64(frameCount * frameSamples)
		segments := make([]Segment, 0, segmentCount)
		sampleCursor := int64(0)
		for si := 0; si < segmentCount; si++ {
			segmentStart := sampleCursor
			segmentEnd := segmentStart + int64(segmentFrames[si]*frameSamples)
			phonemeCount := 7 + (si+i)%2
			phWeights := make([]int, phonemeCount)
			for j := range phWeights {
				phWeights[j] = 2 + rng.intn(7)
			}
			phFrames, _, err := allocateWithMinimum(segmentFrames[si], phWeights, 2)
			if err != nil {
				return SampleSet{}, fmt.Errorf("allocate sample phonemes: %w", err)
			}
			phonemes := make([]Phoneme, 0, phonemeCount)
			phSampleStart := segmentStart
			for pi := 0; pi < phonemeCount; pi++ {
				phSampleEnd := phSampleStart + int64(phFrames[pi]*frameSamples)
				if pi < phonemeCount-1 {
					jitter := 0
					if frameSamples > 600 {
						jitter = rng.intn(frameSamples/2) - frameSamples/4
					}
					phSampleEnd += int64(jitter)
					minEnd := phSampleStart + int64(frameSamples)
					if phSampleEnd < minEnd {
						phSampleEnd = minEnd
					}
					maxEnd := segmentEnd - int64((phonemeCount-pi-1)*frameSamples)
					if phSampleEnd > maxEnd {
						phSampleEnd = maxEnd
					}
				} else {
					phSampleEnd = segmentEnd
				}
				lip := LipSilent
				if pi > 0 {
					speech := []LipCategory{LipWide, LipOpen, LipRound, LipWide, LipSmile, LipOpen, LipPress}
					lip = speech[(i+si*3+pi)%len(speech)]
				}
				phonemes = append(phonemes, Phoneme{
					ID:          fmt.Sprintf("ph-%d-%d", si+1, pi+1),
					StartSample: phSampleStart,
					EndSample:   phSampleEnd,
					Lip:         lip,
				})
				phSampleStart = phSampleEnd
			}
			segments = append(segments, Segment{
				ID:          fmt.Sprintf("seg-%d", si+1),
				VoiceRef:    fmt.Sprintf("voice://dlg-%03d/seg-%d.wav", i+1, si+1),
				StartSample: segmentStart,
				EndSample:   segmentEnd,
				Phonemes:    phonemes,
			})
			sampleCursor = segmentEnd
		}
		if sampleCursor != totalSamples {
			return SampleSet{}, fmt.Errorf("generator produced %d samples, expected %d", sampleCursor, totalSamples)
		}
		project := DialogueProject{
			DialogueID:             fmt.Sprintf("dlg-%03d", i+1),
			Segments:               segments,
			Subtitle:               Subtitle{Text: text, Weights: weights},
			FPS:                    fps,
			SampleRate:             sampleRate,
			Quantization:           Quantization{Direction: direction, Version: DefaultQuantVersion},
			MinReadableFrames:      2,
			LipTransitionTolerance: 2,
			AlgorithmVersion:       DefaultAlgorithm,
		}
		if err := ValidateProject(project); err != nil {
			return SampleSet{}, err
		}
		truth, err := ConstructTruth(project)
		if err != nil {
			return SampleSet{}, err
		}
		set.Projects = append(set.Projects, project)
		set.Truths = append(set.Truths, truth)
	}
	return set, nil
}
