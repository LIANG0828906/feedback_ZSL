package dialoguecal

import (
	"fmt"
	"sort"
)

const (
	MinTargetLevelPermille = 1
	MaxTargetLevelPermille = 1000
	MinTargetDBX100        = -6000
	MaxTargetDBX100        = 0
	MaxFrameRateFPS        = 1000
	MaxGainPermille        = 1_000_000
)

type Issue struct {
	Code        Code           `json:"code"`
	Path        string         `json:"path"`
	SegmentID   string         `json:"segment_id,omitempty"`
	PhonemeID   string         `json:"phoneme_id,omitempty"`
	StartSample int            `json:"start_sample,omitempty"`
	EndSample   int            `json:"end_sample,omitempty"`
	Detail      string         `json:"detail"`
	Context     map[string]any `json:"context,omitempty"`
}

type ValidationReport struct {
	OK     bool    `json:"ok"`
	Issues []Issue `json:"issues"`
}

func (r ValidationReport) Err() error {
	if len(r.Issues) == 0 {
		return nil
	}
	issue := r.Issues[0]
	return &BusinessError{Code: issue.Code, Path: issue.Path, SegmentID: issue.SegmentID, PhonemeID: issue.PhonemeID, Detail: issue.Detail, Context: issue.Context}
}

// NormalizeProject validates and derives all fixed-point fields. It does not
// fill semantic source fields with silent defaults; policy constants are only
// derived after explicit values have been loaded.
func NormalizeProject(p *Project) error {
	if p == nil {
		return newError(CodeSchemaUnknown, "project", "nil project")
	}
	if p.SchemaVersion == "" {
		p.SchemaVersion = CanonicalSchema
	}
	if p.SampleRateHz <= 0 {
		return newError(CodeFrameRateRange, "sample_rate_hz", "sample rate must be positive")
	}
	if p.FramesPerSecond == 0 {
		return newError(CodeFrameRateZero, "frames_per_second", "frame rate is zero")
	}
	if p.FramesPerSecond < 0 || p.FramesPerSecond > MaxFrameRateFPS {
		return newError(CodeFrameRateRange, "frames_per_second", "frame rate %d outside [1,%d]", p.FramesPerSecond, MaxFrameRateFPS)
	}
	if p.FrameShiftSamples <= 0 {
		return newError(CodeFrameRateRange, "frame_shift_samples", "normalized frame shift must be positive")
	}
	if p.FrameWindowSamples <= 0 {
		return newError(CodeFrameRateRange, "frame_window_samples", "normalized frame window must be positive")
	}
	if p.FrameWindowSamples > len(p.PCM) {
		return newError(CodeFrameBounds, "frame_window_samples", "window %d exceeds PCM length %d", p.FrameWindowSamples, len(p.PCM))
	}
	if p.EnergyCapPermille <= 0 || p.EnergyCapPermille > 100000 {
		return newError(CodeTargetLoudnessRange, "energy_cap_permille", "energy cap permille must be in [1,100000]")
	}
	p.EnergyCapQ15 = PermilleToQ15(p.EnergyCapPermille)
	if p.CalibrationTolDBQ8 == 0 {
		p.CalibrationTolDBQ8 = Q8(1 * Q8Scale)
	}
	if p.LevelJumpThresholdQ8 == 0 {
		p.LevelJumpThresholdQ8 = Q8(3 * Q8Scale)
	}
	if p.SettleFrames <= 0 {
		p.SettleFrames = 2
	}

	sort.SliceStable(p.Segments, func(i, j int) bool { return p.Segments[i].StartSample < p.Segments[j].StartSample })
	for i := range p.Segments {
		seg := &p.Segments[i]
		seg.Index = i
		path := fmt.Sprintf("segments[%d]", i)
		if seg.ID == "" {
			seg.ID = fmt.Sprintf("seg-%d", i)
		}
		if seg.StartSample < 0 || seg.EndSample > len(p.PCM) || seg.StartSample >= seg.EndSample {
			return newError(CodeFrameBounds, path, "invalid segment sample range [%d,%d), pcm_len=%d", seg.StartSample, seg.EndSample, len(p.PCM))
		}
		if seg.GainPermille < 0 {
			return &BusinessError{Code: CodeGainNegative, Path: path + ".gain_permille", SegmentID: seg.ID, Detail: fmt.Sprintf("gain_permille=%d", seg.GainPermille)}
		}
		if seg.GainPermille > MaxGainPermille {
			return newError(CodeFixedOverflow, path+".gain_permille", "gain permille %d exceeds declared safe bound %d", seg.GainPermille, MaxGainPermille)
		}
		seg.GainQ15 = PermilleToQ15(seg.GainPermille)
		if seg.TargetLevelPermille < MinTargetLevelPermille || seg.TargetLevelPermille > MaxTargetLevelPermille {
			return &BusinessError{Code: CodeTargetLoudnessRange, Path: path + ".target_level_permille", SegmentID: seg.ID, Detail: fmt.Sprintf("target level %d outside [%d,%d] permille", seg.TargetLevelPermille, MinTargetLevelPermille, MaxTargetLevelPermille)}
		}
		seg.TargetEnergyQ15 = PermilleToQ15(seg.TargetLevelPermille)
		seg.TargetDBQ8 = LinearToDBQ8(seg.TargetEnergyQ15)
		if seg.WatermarkDBX100 == 0 {
			seg.WatermarkDBX100 = int(int64(seg.TargetDBQ8) * 100 / int64(Q8Scale))
		}
	}
	if err := validateSegmentRanges(p); err != nil {
		return err
	}
	for i := range p.Phonemes {
		if idx := segmentAt(p, p.Phonemes[i].StartSample); idx >= 0 {
			p.Phonemes[i].SegmentID = p.Segments[idx].ID
		}
	}
	return ValidateAnnotations(p).Err()
}

func validateSegmentRanges(p *Project) error {
	cursor := 0
	for i, seg := range p.Segments {
		if seg.StartSample != cursor {
			code := CodePhonemeCoverageGap
			if seg.StartSample < cursor {
				code = CodePhonemeOverlap
			}
			return &BusinessError{Code: code, Path: fmt.Sprintf("segments[%d]", i), SegmentID: seg.ID, Detail: fmt.Sprintf("segment range starts at %d, expected cursor %d", seg.StartSample, cursor)}
		}
		cursor = seg.EndSample
	}
	if cursor != len(p.PCM) {
		return &BusinessError{Code: CodePhonemeCoverageGap, Path: "segments", Detail: fmt.Sprintf("segments end at %d but PCM length is %d", cursor, len(p.PCM))}
	}
	return nil
}

// ValidateAnnotations checks order and complete, non-overlapping coverage on
// the normalized global sample axis. The returned report contains all issues
// found up to the first coverage break after sorting.
func ValidateAnnotations(p *Project) ValidationReport {
	report := ValidationReport{OK: true}
	if p == nil {
		return ValidationReport{OK: false, Issues: []Issue{{Code: CodeSchemaUnknown, Path: "project", Detail: "nil project"}}}
	}
	for i := range p.Phonemes {
		ph := &p.Phonemes[i]
		path := fmt.Sprintf("phonemes[%s]", ph.ID)
		if ph.StartSample >= ph.EndSample {
			report.OK = false
			report.Issues = append(report.Issues, Issue{Code: CodePhonemeOrder, Path: path, PhonemeID: ph.ID, StartSample: ph.StartSample, EndSample: ph.EndSample, Detail: "start must be less than end"})
		}
		if ph.StartSample < 0 || ph.EndSample > len(p.PCM) {
			report.OK = false
			report.Issues = append(report.Issues, Issue{Code: CodeFrameBounds, Path: path, PhonemeID: ph.ID, StartSample: ph.StartSample, EndSample: ph.EndSample, Detail: fmt.Sprintf("phoneme range outside pcm length %d", len(p.PCM))})
		}
		if ph.WeightPermille < 0 || ph.WeightPermille > 4000 {
			report.OK = false
			report.Issues = append(report.Issues, Issue{Code: CodeTargetLoudnessRange, Path: path + ".weight_permille", PhonemeID: ph.ID, Detail: "weight must be in [0,4000]"})
		}
		ph.WeightQ15 = PermilleToQ15(ph.WeightPermille)
	}
	if len(report.Issues) > 0 {
		return report
	}

	ordered := append([]Phoneme(nil), p.Phonemes...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].StartSample == ordered[j].StartSample {
			return ordered[i].EndSample < ordered[j].EndSample
		}
		return ordered[i].StartSample < ordered[j].StartSample
	})
	cursor := 0
	for _, ph := range ordered {
		if ph.StartSample > cursor {
			report.OK = false
			report.Issues = append(report.Issues, Issue{Code: CodePhonemeCoverageGap, Path: "phonemes", PhonemeID: ph.ID, StartSample: cursor, EndSample: ph.StartSample, Detail: "coverage gap"})
			cursor = ph.StartSample
		} else if ph.StartSample < cursor {
			report.OK = false
			report.Issues = append(report.Issues, Issue{Code: CodePhonemeOverlap, Path: "phonemes", PhonemeID: ph.ID, StartSample: ph.StartSample, EndSample: ph.EndSample, Detail: "coverage overlap", Context: map[string]any{"cursor_sample": cursor}})
		}
		cursor = ph.EndSample
	}
	if cursor != len(p.PCM) && len(report.Issues) == 0 {
		report.OK = false
		report.Issues = append(report.Issues, Issue{Code: CodePhonemeCoverageGap, Path: "phonemes", StartSample: cursor, EndSample: len(p.PCM), Detail: "trailing coverage gap"})
	}
	return report
}

// BuildFrames maps declared FPS/shift/window to sample intervals and assigns
// each frame to the segment containing its first sample.
func BuildFrames(p *Project) ([]Frame, error) {
	if err := NormalizeProject(p); err != nil {
		return nil, err
	}
	if p.FrameShiftSamples <= 0 || p.FrameWindowSamples <= 0 {
		return nil, newError(CodeFrameBounds, "frame_shift_samples", "frame plan is not normalized")
	}
	var frames []Frame
	indexInSegment := map[string]int{}
	for start, global := 0, 0; start+p.FrameWindowSamples <= len(p.PCM); start += p.FrameShiftSamples {
		end := start + p.FrameWindowSamples
		segIndex := segmentAt(p, start)
		if segIndex < 0 {
			return nil, &BusinessError{Code: CodeFrameBounds, FrameIndex: global, Path: fmt.Sprintf("frames[%d]", global), Detail: fmt.Sprintf("frame start %d is outside declared segments", start)}
		}
		seg := p.Segments[segIndex]
		ids := []string{}
		for _, ph := range p.Phonemes {
			if ph.StartSample < end && ph.EndSample > start {
				ids = append(ids, ph.ID)
			}
		}
		sort.Strings(ids)
		local := indexInSegment[seg.ID]
		indexInSegment[seg.ID] = local + 1
		frames = append(frames, Frame{
			GlobalIndex:    global,
			SegmentID:      seg.ID,
			IndexInSegment: local,
			StartSample:    start,
			EndSample:      end,
			PhonemeIDs:     ids,
		})
		global++
	}
	return frames, nil
}

func segmentAt(p *Project, sample int) int {
	for i := range p.Segments {
		if sample >= p.Segments[i].StartSample && sample < p.Segments[i].EndSample {
			return i
		}
	}
	return -1
}

func segmentByID(p *Project, id string) *Segment {
	for i := range p.Segments {
		if p.Segments[i].ID == id {
			return &p.Segments[i]
		}
	}
	return nil
}

func phonemeByID(p *Project, id string) *Phoneme {
	for i := range p.Phonemes {
		if p.Phonemes[i].ID == id {
			return &p.Phonemes[i]
		}
	}
	return nil
}

// SafeFrameSamples returns a copy-like subslice only after bounds checks.
func SafeFrameSamples(p *Project, f Frame) ([]int16, error) {
	if f.StartSample < 0 || f.EndSample < f.StartSample || f.EndSample > len(p.PCM) {
		return nil, &BusinessError{Code: CodeFrameBounds, FrameIndex: f.GlobalIndex, SegmentID: f.SegmentID, Path: fmt.Sprintf("frames[%d]", f.GlobalIndex), Detail: fmt.Sprintf("samples [%d,%d) out of pcm length %d", f.StartSample, f.EndSample, len(p.PCM))}
	}
	return p.PCM[f.StartSample:f.EndSample], nil
}
