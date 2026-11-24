package sample

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"audioeval/internal/codec"
	"audioeval/internal/model"
)

type GenerateOptions struct {
	Dir         string
	SampleID    string
	Seed        int64
	SampleRate  int
	Channels    int
	FrameLength int
	FrameHop    int
	FrameCount  int
	Quality     model.QualityLevel
}

type Generated struct {
	Dir      string
	Manifest model.Manifest
	Truth    model.Truth
}

func Generate(opts GenerateOptions) (Generated, error) {
	if err := validateGenerateOptions(&opts); err != nil {
		return Generated{}, err
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return Generated{}, fmt.Errorf("create sample directory: %w", err)
	}
	spec, _ := model.LookupQuality(opts.Quality)
	totalSamples := (opts.FrameCount-1)*opts.FrameHop + opts.FrameLength
	interleaved := totalSamples * opts.Channels

	original := make([]int16, interleaved)
	for frame := 0; frame < opts.FrameCount; frame++ {
		frameStart := frame * opts.FrameHop
		for ch := 0; ch < opts.Channels; ch++ {
			for local := 0; local < opts.FrameLength; local++ {
				index := frameStart + local
				original[index*opts.Channels+ch] = int16(clamp16(baseSample(opts.Seed, opts.SampleRate, index, ch)))
			}
		}
	}

	pulsePositions := make([][]int, opts.FrameCount)
	for frame := 0; frame < opts.FrameCount; frame++ {
		positions := framePulses(opts.FrameLength)
		pulsePositions[frame] = positions
		frameStart := frame * opts.FrameHop
		for ch := 0; ch < opts.Channels; ch++ {
			for pulseIndex, position := range positions {
				amplitude := int64(9000 + (pulseIndex%2)*2500)
				if (frame+pulseIndex+ch)%2 == 1 {
					amplitude = -amplitude
				}
				shape := []int64{-1, 2, 4, 2, -1}
				for d, weight := range shape {
					at := frameStart + position + d - 2
					if at < 0 || at >= totalSamples {
						continue
					}
					idx := at*opts.Channels + ch
					value := int64(original[idx]) + amplitude*weight/4
					original[idx] = int16(clamp16(value))
				}
			}
		}
	}

	acc := make([]int64, interleaved)
	counts := make([]int, totalSamples)
	for frame := 0; frame < opts.FrameCount; frame++ {
		frameStart := frame * opts.FrameHop
		for ch := 0; ch < opts.Channels; ch++ {
			segment := make([]int16, opts.FrameLength)
			for local := 0; local < opts.FrameLength; local++ {
				segment[local] = original[(frameStart+local)*opts.Channels+ch]
			}
			encoding, err := codec.EncodeFrame(segment, spec.Step)
			if err != nil {
				return Generated{}, err
			}
			for local := 0; local < opts.FrameLength; local++ {
				at := frameStart + local
				acc[at*opts.Channels+ch] += int64(encoding.Reconstruction[local])
			}
		}
		for local := 0; local < opts.FrameLength; local++ {
			counts[frameStart+local]++
		}
	}

	reconstruction := make([]int16, interleaved)
	for index := 0; index < totalSamples; index++ {
		if counts[index] == 0 {
			return Generated{}, fmt.Errorf("internal encoder gap at sample %d", index)
		}
		for ch := 0; ch < opts.Channels; ch++ {
			pos := index*opts.Channels + ch
			reconstruction[pos] = int16(clamp16(roundDiv(acc[pos], int64(counts[index]))))
		}
	}

	truth := model.Truth{
		SchemaVersion: model.SchemaVersion,
		SampleID:      opts.SampleID,
		Frames:        make([]model.FrameTruth, opts.FrameCount),
	}
	quantVariance := float64(spec.Step*spec.Step) * float64(2*opts.FrameLength-1) /
		float64(12*opts.FrameLength*opts.FrameLength)
	quantStd := math.Sqrt(quantVariance) * 65536
	expectedSNR := int64(math.Floor(10 * math.Log10(float64(32767*32767)/quantVariance) * 1_000_000))
	for frame := 0; frame < opts.FrameCount; frame++ {
		// Expected PSNR uses the same int16 peak convention as the evaluator.
		// It is derived only from declared quant step and frame geometry, never
		// from an observed reconstruction metric.
		truth.Frames[frame] = model.FrameTruth{
			Frame:            frame,
			PulsePositions:   pulsePositions[frame],
			ExpectedSNRUDb:   expectedSNR,
			NoiseFloorStdQ16: int64(math.Round(quantStd)),
		}
	}
	fingerprint, err := model.TruthFingerprint(&truth)
	if err != nil {
		return Generated{}, err
	}
	truth.TruthFingerprint = fingerprint

	originalBytes := pcmBytes(original)
	reconstructionBytes := pcmBytes(reconstruction)
	originalHash := hashBytes(originalBytes)
	reconstructionHash := hashBytes(reconstructionBytes)
	originalFrameHashes := make([]string, opts.FrameCount)
	reconstructionFrameHashes := make([]string, opts.FrameCount)
	for frame := 0; frame < opts.FrameCount; frame++ {
		start := frame * opts.FrameHop
		originalFrameHashes[frame] = hashBytes(pcmBytes(original[start*opts.Channels : (start+opts.FrameLength)*opts.Channels]))
		reconstructionFrameHashes[frame] = hashBytes(pcmBytes(reconstruction[start*opts.Channels : (start+opts.FrameLength)*opts.Channels]))
	}

	manifest := model.Manifest{
		SchemaVersion:             model.SchemaVersion,
		SampleID:                  opts.SampleID,
		Seed:                      opts.Seed,
		SampleRate:                opts.SampleRate,
		Channels:                  opts.Channels,
		FrameLength:               opts.FrameLength,
		FrameHop:                  opts.FrameHop,
		FrameCount:                opts.FrameCount,
		TotalSamplesPerChannel:    totalSamples,
		Quality:                   opts.Quality,
		QuantStep:                 spec.Step,
		OriginalFile:              "original.i16",
		ReconstructionFile:        "reconstruction.i16",
		TruthFile:                 "truth.json",
		OriginalHash:              originalHash,
		ReconstructionHash:        reconstructionHash,
		TruthFingerprint:          fingerprint,
		OriginalFrameHashes:       originalFrameHashes,
		ReconstructionFrameHashes: reconstructionFrameHashes,
	}
	inputHash, err := model.ComputeInputHash(&manifest, fingerprint)
	if err != nil {
		return Generated{}, err
	}
	manifest.InputHash = inputHash

	if err := os.WriteFile(filepath.Join(opts.Dir, manifest.OriginalFile), originalBytes, 0o644); err != nil {
		return Generated{}, fmt.Errorf("write original sequence: %w", err)
	}
	if err := os.WriteFile(filepath.Join(opts.Dir, manifest.ReconstructionFile), reconstructionBytes, 0o644); err != nil {
		return Generated{}, fmt.Errorf("write reconstruction sequence: %w", err)
	}
	if err := writeJSON(filepath.Join(opts.Dir, manifest.TruthFile), truth); err != nil {
		return Generated{}, err
	}
	if err := writeJSON(filepath.Join(opts.Dir, "manifest.json"), manifest); err != nil {
		return Generated{}, err
	}

	return Generated{Dir: opts.Dir, Manifest: manifest, Truth: truth}, nil
}

func validateGenerateOptions(opts *GenerateOptions) error {
	if opts.Dir == "" {
		return model.NewError(model.CodePara, "dir", -1, "output directory is required")
	}
	if opts.SampleID == "" {
		opts.SampleID = "sample"
	}
	if opts.SampleRate <= 0 {
		return model.NewError(model.CodePara, "sample_rate", -1, "sample rate must be a positive integer")
	}
	if opts.Channels <= 0 {
		return model.NewError(model.CodePara, "channels", -1, "channel count must be a positive integer")
	}
	if opts.FrameLength <= 1 {
		return model.NewError(model.CodePara, "frame_length", -1, "frame length must be greater than one")
	}
	if opts.FrameHop <= 0 || opts.FrameHop > opts.FrameLength {
		return model.NewError(model.CodePara, "frame_hop", -1, "frame hop must be a positive integer not greater than frame length")
	}
	if opts.FrameCount <= 0 {
		return model.NewError(model.CodePara, "frame_count", -1, "frame count must be a positive integer")
	}
	if _, ok := model.LookupQuality(opts.Quality); !ok {
		return model.NewError(model.CodePara, "quality", -1, "unsupported quality level")
	}
	return nil
}

func baseSample(seed int64, sampleRate, sampleIndex, channel int) int64 {
	lowHz := 80
	period := sampleRate / lowHz
	if period < 4 {
		period = 4
	}
	phase := (sampleIndex + channel*31 + int(seed%int64(period))) % period
	triangle := 2*phase - period
	if triangle < 0 {
		triangle = -triangle
	}
	low := int64(7000 - triangle*14000/period)

	state := uint64(seed) + uint64(sampleIndex+1)*0x9e3779b97f4a7c15 + uint64(channel+1)*0xbf58476d1ce4e5b9
	noise := int64(splitMix64(state) % 4801)
	noise -= 2400
	texture := int64(0)
	if (sampleIndex+channel)%3 == 0 {
		texture = noise * 3 / 2
	} else {
		texture = noise
	}
	return low + texture
}

func framePulses(frameLength int) []int {
	candidates := []int{
		frameLength / 8,
		frameLength / 3,
		2 * frameLength / 3,
		frameLength - 1 - frameLength/8,
	}
	seen := map[int]bool{}
	positions := make([]int, 0, len(candidates))
	for _, position := range candidates {
		if position < 0 {
			position = 0
		}
		if position >= frameLength {
			position = frameLength - 1
		}
		if !seen[position] {
			seen[position] = true
			positions = append(positions, position)
		}
	}
	return positions
}

func splitMix64(value uint64) uint64 {
	value += 0x9e3779b97f4a7c15
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func pcmBytes(samples []int16) []byte {
	data := make([]byte, len(samples)*2)
	for i, value := range samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(value))
	}
	return data
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func clamp16(value int64) int16 {
	if value < math.MinInt16 {
		return math.MinInt16
	}
	if value > math.MaxInt16 {
		return math.MaxInt16
	}
	return int16(value)
}

func roundDiv(value, divisor int64) int64 {
	if divisor == 0 {
		return 0
	}
	if divisor < 0 {
		value = -value
		divisor = -divisor
	}
	if value >= 0 {
		return (value + divisor/2) / divisor
	}
	return -((-value + divisor/2) / divisor)
}
