package loudness

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"math/bits"
	"sort"
)

// FrameEnvelope is one logical-frame measurement. Energy is Q32 mean-square
// energy; Rms is Q16 linear amplitude. Measured values include clip gain,
// category, and loudness weight, but not session gain or calibration.
type FrameEnvelope struct {
	ClipID              int          `json:"clipId"`
	FrameIndex          int          `json:"frameIndex"`
	RangeID             string       `json:"rangeId"`
	StartSample         int64        `json:"startSample"`
	EndSample           int64        `json:"endSample"`
	SampleCount         int64        `json:"sampleCount"`
	EnergyQ32           int64        `json:"energyQ32"`
	RmsQ16              int64        `json:"rmsQ16"`
	MeasuredDBQ8        int64        `json:"measuredDbQ8"`
	PeakSample          int64        `json:"peakSample"`
	PeakOffset          int64        `json:"peakOffset"`
	CalibratedEnergyQ32 int64        `json:"calibratedEnergyQ32"`
	CalibratedRmsQ16    int64        `json:"calibratedRmsQ16"`
	CalibratedDBQ8      int64        `json:"calibratedDbQ8"`
	Truncations         []Truncation `json:"truncations"`
	CacheKey            string       `json:"cacheKey,omitempty"`
}

// ClipMeasurement is the segment-level envelope extracted from frames.
type ClipMeasurement struct {
	ClipID           int             `json:"clipId"`
	ContentHash      string          `json:"contentHash"`
	PCMHash          string          `json:"pcmHash"`
	FrameLength      int64           `json:"frameLength"`
	FrameShift       int64           `json:"frameShift"`
	Frames           []FrameEnvelope `json:"frames"`
	SegmentEnergyQ32 int64           `json:"segmentEnergyQ32"`
	SegmentRmsQ16    int64           `json:"segmentRmsQ16"`
	SegmentDBQ8      int64           `json:"segmentDbQ8"`
	Truncations      []Truncation    `json:"truncations"`
}

// ClipResult is a calibrated clip and the frozen unit used by commits.
type ClipResult struct {
	ClipID                     int             `json:"clipId"`
	ContentHash                string          `json:"contentHash"`
	PCMHash                    string          `json:"pcmHash"`
	FrameLength                int64           `json:"frameLength"`
	FrameShift                 int64           `json:"frameShift"`
	Frames                     []FrameEnvelope `json:"frames"`
	GainAdjustmentQ12          int64           `json:"gainAdjustmentQ12"`
	DesiredGainQ12             int64           `json:"desiredGainQ12"`
	GainLimited                bool            `json:"gainLimited"`
	MeasuredSegmentEnergyQ32   int64           `json:"measuredSegmentEnergyQ32"`
	MeasuredSegmentRmsQ16      int64           `json:"measuredSegmentRmsQ16"`
	MeasuredSegmentDBQ8        int64           `json:"measuredSegmentDbQ8"`
	CalibratedSegmentDBQ8      int64           `json:"calibratedSegmentDbQ8"`
	CalibratedSegmentRmsQ16    int64           `json:"calibratedSegmentRmsQ16"`
	CalibratedSegmentEnergyQ32 int64           `json:"calibratedSegmentEnergyQ32"`
	MeasurementTruncations     []Truncation    `json:"measurementTruncations,omitempty"`
	Truncations                []Truncation    `json:"truncations"`
	EnvelopeDigest             string          `json:"envelopeDigest"`
	MeasurementCacheKey        string          `json:"measurementCacheKey,omitempty"`
	CalibrationCacheKey        string          `json:"calibrationCacheKey,omitempty"`
}

// GeneratePCM builds a deterministic mono triangle-wave clip from its
// descriptor. No audio file or floating-point oscillator is used.
func GeneratePCM(project *Project, clip Clip) ([]int16, error) {
	if project == nil {
		return nil, &DomainError{Code: ESchema, Path: "$", Detail: "project is nil"}
	}
	if project.SampleRate <= 0 {
		return nil, &DomainError{Code: EPara, Field: "sampleRate", LegalMin: 1, LegalMax: MaxSampleRate}
	}
	if len(clip.F0TrackHz) == 0 {
		return nil, &DomainError{Code: ESchema, Path: fmt.Sprintf("$.clips[%d].f0TrackHz", clip.ID), Detail: "frequency track is empty"}
	}
	length := clip.DurationSamples()
	if length <= 0 {
		return nil, &DomainError{Code: ESchema, Path: fmt.Sprintf("$.clips[%d]", clip.ID), Detail: "clip has no samples"}
	}
	if length > int64(int(^uint(0)>>1)) {
		return nil, &DomainError{Code: EOvF, Path: fmt.Sprintf("$.clips[%d]", clip.ID), Field: "duration", Operands: []int64{length, int64(int(^uint(0) >> 1))}}
	}
	samples := make([]int16, int(length))
	var phase uint64
	fadeLength := int64(64)
	if length < fadeLength*2 {
		fadeLength = length / 2
	}
	trackLast := int64(len(clip.F0TrackHz) - 1)
	for i := int64(0); i < length; i++ {
		frequency := clip.F0TrackHz[0]
		if trackLast > 0 {
			scaled := i * trackLast * 65536 / maxInt64(1, length-1)
			index := scaled >> 16
			if index >= trackLast {
				frequency = clip.F0TrackHz[trackLast]
			} else {
				frac := scaled & 0xFFFF
				low := clip.F0TrackHz[index]
				high := clip.F0TrackHz[index+1]
				frequency = low + (high-low)*frac/65536
			}
		}
		phaseInc := uint64(frequency) * (uint64(1) << 32) / uint64(project.SampleRate)
		phase += phaseInc
		saw := int64((phase >> 22) & 1023)
		var triangle int64
		if saw < 512 {
			triangle = -2048 + saw*8
		} else {
			triangle = 2048 - (saw-512)*8
		}
		fadeQ12 := InternalGainScale
		if fadeLength > 0 {
			if i < fadeLength {
				fadeQ12 = i * InternalGainScale / fadeLength
			}
			remaining := length - 1 - i
			if remaining < fadeLength {
				edge := remaining * InternalGainScale / fadeLength
				if edge < fadeQ12 {
					fadeQ12 = edge
				}
			}
		}
		value := (clip.AmplitudeQ12 * triangle) >> 11
		value = (value * fadeQ12) >> 12
		if value > 32767 {
			value = 32767
		}
		if value < -32768 {
			value = -32768
		}
		samples[i] = int16(value)
	}
	return samples, nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// MeasureClip synthesizes PCM and extracts a per-frame envelope.
func MeasureClip(project *Project, clip Clip) (ClipMeasurement, error) {
	pcm, err := GeneratePCM(project, clip)
	if err != nil {
		return ClipMeasurement{}, err
	}
	return MeasureClipPCM(project, clip, pcm)
}

// MeasureClipPCM measures an already synthesized deterministic PCM buffer.
func MeasureClipPCM(project *Project, clip Clip, pcm []int16) (ClipMeasurement, error) {
	if err := ValidateFrameParameters(project.FrameLength, project.FrameShift); err != nil {
		return ClipMeasurement{}, err
	}
	if int64(len(pcm)) != clip.DurationSamples() {
		return ClipMeasurement{}, &DomainError{
			Code: EPara, Path: fmt.Sprintf("$.clips[%d].pcm", clip.ID), Field: "pcm",
			Operands: []int64{int64(len(pcm)), clip.DurationSamples()}, Detail: "PCM length must equal clip duration",
		}
	}
	categoryFactor := CategoryFactorQ12(clip.Category)
	if categoryFactor <= 0 {
		return ClipMeasurement{}, &DomainError{Code: ESchema, Path: fmt.Sprintf("$.clips[%d].category", clip.ID), Field: "category", Detail: "unknown category"}
	}
	measurement := ClipMeasurement{
		ClipID: clip.ID, FrameLength: project.FrameLength, FrameShift: project.FrameShift,
	}
	measurement.PCMHash = hashPCM(pcm)
	measurement.ContentHash = hashClip(project, clip, measurement.PCMHash)
	var weightedEnergySum int64
	var weightedCount int64
	for localStart := int64(0); localStart < int64(len(pcm)); localStart += project.FrameShift {
		localEnd := localStart + project.FrameLength
		if localEnd > int64(len(pcm)) {
			localEnd = int64(len(pcm))
		}
		sampleCount := localEnd - localStart
		var sumSquares int64
		var peakOffset int64
		var peakAbs int64 = -1
		for i := localStart; i < localEnd; i++ {
			sample := int64(pcm[i])
			abs := sample
			if abs < 0 {
				abs = -abs
			}
			if abs > peakAbs {
				peakAbs = abs
				peakOffset = i - localStart
			}
			sumSquares += sample * sample
		}
		framePath := fmt.Sprintf("$.clips[%d].frames[%d]", clip.ID, len(measurement.Frames))
		meanSquareQ30, meanTrace, err := MulDivAt(framePath+".meanSquareQ30", sumSquares, 1, sampleCount, RoundDown)
		if err != nil {
			return ClipMeasurement{}, err
		}
		root, sqrtResidual := integerSqrt(meanSquareQ30)
		rmsQ16 := root * 2
		truncations := []Truncation{
			{
				Path: framePath + ".window", Operation: "frame-window-clamp",
				Numerator: localEnd - localStart, Denominator: project.FrameLength,
				Quotient: localEnd - localStart, Remainder: localEnd - localStart, Residual: 0, Rounding: RoundDown,
			},
			meanTrace,
			{
				Path: framePath + ".sqrt", Operation: "integer-sqrt-q30->q15",
				Numerator: meanSquareQ30, Denominator: 1, Quotient: root,
				Remainder: sqrtResidual, Residual: sqrtResidual, Rounding: RoundDown,
			},
		}
		weightedRms, factorTraces, err := applyLoudnessFactors(rmsQ16, categoryFactor, clip.LoudnessWeightQ8, clip.GainQ12, framePath)
		if err != nil {
			return ClipMeasurement{}, err
		}
		truncations = append(truncations, factorTraces...)
		energyQ32, err := CheckedMul(weightedRms, weightedRms, framePath+".energyQ32")
		if err != nil {
			return ClipMeasurement{}, err
		}
		dbQ8, dbTrace, err := LinearToDBQ16(weightedRms, framePath+".db")
		if err != nil {
			return ClipMeasurement{}, err
		}
		truncations = append(truncations, dbTrace)
		frame := FrameEnvelope{
			ClipID: clip.ID, FrameIndex: len(measurement.Frames),
			RangeID:     fmt.Sprintf("clip:%d/frame:%d/%d-%d", clip.ID, len(measurement.Frames), clip.StartSample+localStart, clip.StartSample+localEnd),
			StartSample: clip.StartSample + localStart, EndSample: clip.StartSample + localEnd,
			SampleCount: sampleCount, EnergyQ32: energyQ32, RmsQ16: weightedRms,
			MeasuredDBQ8: dbQ8, PeakSample: clip.StartSample + localStart + peakOffset,
			PeakOffset: peakOffset, Truncations: truncations,
		}
		measurement.Frames = append(measurement.Frames, frame)
		weightedEnergy, err := CheckedMul(energyQ32, sampleCount, framePath+".weightedEnergy")
		if err != nil {
			return ClipMeasurement{}, err
		}
		weightedEnergySum, err = CheckedAdd(weightedEnergySum, weightedEnergy, framePath+".weightedEnergySum")
		if err != nil {
			return ClipMeasurement{}, err
		}
		weightedCount, err = CheckedAdd(weightedCount, sampleCount, framePath+".weightedCount")
		if err != nil {
			return ClipMeasurement{}, err
		}
	}
	if weightedCount == 0 {
		return ClipMeasurement{}, &DomainError{Code: EPara, Path: fmt.Sprintf("$.clips[%d]", clip.ID), Field: "samples", Detail: "clip has no measurable samples"}
	}
	segmentEnergy, segmentTrace, err := MulDivAt(fmt.Sprintf("$.clips[%d].segment.energyQ32", clip.ID), weightedEnergySum, 1, weightedCount, RoundHalfUp)
	if err != nil {
		return ClipMeasurement{}, err
	}
	root, sqrtResidual := integerSqrt(segmentEnergy)
	segmentRms := root
	segmentDB, dbTrace, err := LinearToDBQ16(segmentRms, fmt.Sprintf("$.clips[%d].segment.db", clip.ID))
	if err != nil {
		return ClipMeasurement{}, err
	}
	measurement.SegmentEnergyQ32 = segmentEnergy
	measurement.SegmentRmsQ16 = segmentRms
	measurement.SegmentDBQ8 = segmentDB
	measurement.Truncations = []Truncation{
		segmentTrace,
		{
			Path: fmt.Sprintf("$.clips[%d].segment.sqrt", clip.ID), Operation: "integer-sqrt-q32->q16",
			Numerator: segmentEnergy, Denominator: 1, Quotient: root,
			Remainder: sqrtResidual, Residual: sqrtResidual, Rounding: RoundDown,
		},
		dbTrace,
	}
	return measurement, nil
}

func applyLoudnessFactors(rmsQ16, categoryFactor, weightQ8, gainQ12 int64, path string) (int64, []Truncation, error) {
	value := rmsQ16
	traces := make([]Truncation, 0, 3)
	for _, step := range []struct {
		name   string
		factor int64
		shift  uint
	}{
		{"category", categoryFactor, 12},
		{"weight", weightQ8, 8},
		{"clipGain", gainQ12, 12},
	} {
		next, trace, err := MulDivAt(path+"."+step.name, value, step.factor, int64(1)<<step.shift, RoundHalfUp)
		if err != nil {
			return 0, nil, err
		}
		traces = append(traces, trace)
		value = next
	}
	return value, traces, nil
}

func integerSqrt(value int64) (int64, int64) {
	if value <= 0 {
		return 0, 0
	}
	uvalue := uint64(value)
	root := uint64(1) << ((bits.Len64(uvalue) + 1) / 2)
	for {
		next := (root + uvalue/root) / 2
		if next >= root {
			break
		}
		root = next
	}
	for (root+1)*(root+1) <= uvalue {
		root++
	}
	for root*root > uvalue {
		root--
	}
	return int64(root), value - int64(root*root)
}

func hashPCM(pcm []int16) string {
	hasher := sha256.New()
	var buffer [2]byte
	for _, sample := range pcm {
		binary.LittleEndian.PutUint16(buffer[:], uint16(sample))
		hasher.Write(buffer[:])
	}
	return fmt.Sprintf("%x", hasher.Sum(nil))
}

func hashClip(project *Project, clip Clip, pcmHash string) string {
	hasher := sha256.New()
	writeField(hasher, "clip.id", fmt.Sprintf("%d", clip.ID))
	writeField(hasher, "clip.scene", clip.Scene)
	writeField(hasher, "clip.start", fmt.Sprintf("%d", clip.StartSample))
	writeField(hasher, "clip.end", fmt.Sprintf("%d", clip.EndSample))
	writeField(hasher, "clip.category", string(clip.Category))
	writeField(hasher, "clip.weight", fmt.Sprintf("%d", clip.LoudnessWeightQ8))
	writeField(hasher, "clip.gain", fmt.Sprintf("%d", clip.GainQ12))
	writeField(hasher, "clip.amplitude", fmt.Sprintf("%d", clip.AmplitudeQ12))
	writeField(hasher, "clip.sampleRate", fmt.Sprintf("%d", project.SampleRate))
	writeField(hasher, "clip.pcm", pcmHash)
	for _, frequency := range clip.F0TrackHz {
		writeField(hasher, "clip.f0", fmt.Sprintf("%d", frequency))
	}
	return fmt.Sprintf("%x", hasher.Sum(nil))
}

func writeField(hasher hash.Hash, name, value string) {
	hasher.Write([]byte(name))
	hasher.Write([]byte{0})
	hasher.Write([]byte(value))
	hasher.Write([]byte{0})
}

// MeasureProject measures all clips in clip-id order.
func MeasureProject(project *Project) (map[int]ClipMeasurement, error) {
	if err := validateProjectBasics(project); err != nil {
		return nil, err
	}
	measurements := make(map[int]ClipMeasurement, len(project.Clips))
	for _, clip := range project.Clips {
		measurement, err := MeasureClip(project, clip)
		if err != nil {
			return nil, err
		}
		measurements[clip.ID] = measurement
	}
	return measurements, nil
}

// SortedResults returns calibrated results in deterministic clip-id order.
func SortedResults(results map[int]ClipResult) []ClipResult {
	ids := make([]int, 0, len(results))
	for id := range results {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]ClipResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, results[id])
	}
	return out
}
