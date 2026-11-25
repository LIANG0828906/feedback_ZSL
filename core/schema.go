package dialoguecal

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
)

const (
	SchemaV1 = "dialogue.flat.v1"
	SchemaV2 = "dialogue.tree.v2"
	SchemaV3 = "dialogue.sample.v3"
)

func encodePCM(samples []int16) string {
	buf := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(s))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func decodePCM(encoded string) ([]int16, error) {
	buf, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, newError(CodeSchemaUnknown, "pcm_base64", "decode pcm: %v", err)
	}
	if len(buf)%2 != 0 {
		return nil, newError(CodeSchemaUnknown, "pcm_base64", "odd PCM byte length %d", len(buf))
	}
	samples := make([]int16, len(buf)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(buf[2*i:]))
	}
	return samples, nil
}

type dtoV1Phoneme struct {
	ID             string `json:"phoneme_id"`
	SyllableID     string `json:"syllable_id"`
	StartMs        int    `json:"start_ms"`
	EndMs          int    `json:"end_ms"`
	Class          string `json:"class"`
	F0StartHzX100  int    `json:"f0_start_hz_x100"`
	F0EndHzX100    int    `json:"f0_end_hz_x100"`
	WeightPermille int    `json:"weight_permille"`
}

type dtoV1Segment struct {
	ID                         string `json:"segment_id"`
	StartMs                    int    `json:"start_ms"`
	EndMs                      int    `json:"end_ms"`
	GainPermille               int    `json:"gain_permille"`
	TargetLoudnessDBX100       int    `json:"target_loudness_db_x100"`
	CalibrationWatermarkDBX100 *int   `json:"calibration_watermark_db_x100,omitempty"`
}

type dtoV1 struct {
	Schema            string         `json:"schema"`
	DialogueID        string         `json:"dialogue_id"`
	Seed              int64          `json:"seed"`
	SampleRateHz      int            `json:"sample_rate_hz"`
	FramesPerSecond   int            `json:"frames_per_second"`
	FrameShiftMs      int            `json:"frame_shift_ms"`
	FrameWindowMs     int            `json:"frame_window_ms"`
	EnergyCapPermille int            `json:"energy_cap_permille"`
	PCMBase64         string         `json:"pcm_base64"`
	Segments          []dtoV1Segment `json:"segments"`
	PhonemesFlat      []dtoV1Phoneme `json:"phonemes_flat"`
}

type dtoV2Phoneme struct {
	ID             string `json:"phonemeId"`
	OffsetTicks    int    `json:"offsetTicks"`
	DurationTicks  int    `json:"durationTicks"`
	Class          string `json:"class"`
	F0StartHzX100  int    `json:"f0StartHzX100"`
	F0EndHzX100    int    `json:"f0EndHzX100"`
	WeightPermille int    `json:"weightPermille"`
}

type dtoV2Syllable struct {
	ID          string         `json:"syllableId"`
	OffsetTicks int            `json:"offsetTicks"`
	Phonemes    []dtoV2Phoneme `json:"phonemes"`
}

type dtoV2Segment struct {
	ID                    string          `json:"segmentId"`
	StartTick             int             `json:"startTick"`
	DurationTicks         int             `json:"durationTicks"`
	GainPerTenThousand    int             `json:"gainPerTenThousand"`
	TargetAmplitudePer10k int             `json:"targetAmplitudePerTenThousand"`
	Syllables             []dtoV2Syllable `json:"syllablesTree"`
}

type dtoV2 struct {
	Schema           string         `json:"schema"`
	DialogueID       string         `json:"dialogueId"`
	Seed             int64          `json:"seed"`
	SampleRateHz     int            `json:"sampleRateHz"`
	TicksPerSecond   int            `json:"ticksPerSecond"`
	FramesPerSecond  int            `json:"framesPerSecond"`
	FrameShiftTicks  int            `json:"frameShiftTicks"`
	FrameWindowTicks int            `json:"frameWindowTicks"`
	EnergyCapPer10k  int            `json:"energyCapPerTenThousand"`
	PCMBase64        string         `json:"pcmBase64"`
	Segments         []dtoV2Segment `json:"segments"`
}

type dtoV3Segment struct {
	ID                         string `json:"id"`
	StartSample                int    `json:"startSample"`
	EndSample                  int    `json:"endSample"`
	GainDBTimes1e5             int64  `json:"gainDbTimes1e5"`
	TargetLoudnessDBX100       int    `json:"targetLoudnessDbX100"`
	CalibrationWatermarkDBX100 *int   `json:"calibrationWatermarkDbX100,omitempty"`
}

type dtoV3Phoneme struct {
	ID             string `json:"id"`
	SyllableID     string `json:"syllableId"`
	StartSample    int    `json:"startSample"`
	EndSample      int    `json:"endSample"`
	Class          string `json:"class"`
	F0StartHzQ8    Q8     `json:"f0StartHzQ8"`
	F0EndHzQ8      Q8     `json:"f0EndHzQ8"`
	WeightPermille int    `json:"weightPermille"`
}

type dtoV3 struct {
	Schema             string         `json:"schema"`
	DialogueID         string         `json:"dialogueId"`
	Seed               int64          `json:"seed"`
	SampleRateHz       int            `json:"sampleRateHz"`
	FramesPerSecond    int            `json:"framesPerSecond"`
	FrameShiftSamples  int            `json:"frameShiftSamples"`
	FrameWindowSamples int            `json:"frameWindowSamples"`
	EnergyCapPermille  int            `json:"energyCapPermille"`
	PCMBase64          string         `json:"pcmBase64"`
	Segments           []dtoV3Segment `json:"segments"`
	Phonemes           []dtoV3Phoneme `json:"phonemes"`
}

// LoadProjectJSON detects one of the three declared schema versions.
func LoadProjectJSON(data []byte) (*LoadResult, error) {
	var header struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, newError(CodeSchemaUnknown, "schema", "invalid JSON: %v", err)
	}
	switch header.Schema {
	case SchemaV1:
		var doc dtoV1
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, newError(CodeSchemaUnknown, "schema", "invalid v1 document: %v", err)
		}
		return loadV1(&doc)
	case SchemaV2:
		var doc dtoV2
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, newError(CodeSchemaUnknown, "schema", "invalid v2 document: %v", err)
		}
		return loadV2(&doc)
	case SchemaV3:
		var doc dtoV3
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, newError(CodeSchemaUnknown, "schema", "invalid v3 document: %v", err)
		}
		return loadV3(&doc)
	default:
		return nil, newError(CodeSchemaUnknown, "schema", "unrecognized schema %q", header.Schema)
	}
}

func msToSamples(ms, sampleRate int) int {
	return int(math.Round(float64(ms) * float64(sampleRate) / 1000.0))
}

func tickToSamples(tick, ticksPerSecond, sampleRate int) int {
	return int(math.Round(float64(tick) * float64(sampleRate) / float64(ticksPerSecond)))
}

func dbX100ToPermille(dbX100 int) int {
	return DBMicroToPermille(int64(dbX100) * 1000)
}

func targetPermilleFromDBX100(dbX100 int) (int, error) {
	if dbX100 < MinTargetDBX100 || dbX100 > MaxTargetDBX100 {
		return 0, &BusinessError{Code: CodeTargetLoudnessRange, Path: "target_loudness_db_x100", Detail: fmt.Sprintf("target dB*100=%d outside [%d,%d]", dbX100, MinTargetDBX100, MaxTargetDBX100)}
	}
	return dbX100ToPermille(dbX100), nil
}

func baseProject(id string, seed int64, sampleRate, fps int, pcm []int16, shift, window, capPermille int) Project {
	return Project{
		SchemaVersion:      CanonicalSchema,
		ProjectID:          id,
		Seed:               seed,
		SampleRateHz:       sampleRate,
		PCM:                pcm,
		FramesPerSecond:    fps,
		FrameShiftSamples:  shift,
		FrameWindowSamples: window,
		EnergyCapPermille:  capPermille,
	}
}

func loadV1(doc *dtoV1) (*LoadResult, error) {
	pcm, err := decodePCM(doc.PCMBase64)
	if err != nil {
		return nil, err
	}
	p := baseProject(doc.DialogueID, doc.Seed, doc.SampleRateHz, doc.FramesPerSecond, pcm,
		msToSamples(doc.FrameShiftMs, doc.SampleRateHz),
		msToSamples(doc.FrameWindowMs, doc.SampleRateHz),
		doc.EnergyCapPermille)
	resolutions := []string{}
	for i, s := range doc.Segments {
		target, targetErr := targetPermilleFromDBX100(s.TargetLoudnessDBX100)
		if targetErr != nil {
			be := targetErr.(*BusinessError)
			be.Path = fmt.Sprintf("segments[%d].target_loudness_db_x100", i)
			be.SegmentID = s.ID
			return nil, targetErr
		}
		seg := Segment{ID: s.ID, StartSample: msToSamples(s.StartMs, doc.SampleRateHz), EndSample: msToSamples(s.EndMs, doc.SampleRateHz), GainPermille: s.GainPermille, TargetLevelPermille: target}
		if s.CalibrationWatermarkDBX100 != nil {
			seg.WatermarkDBX100 = *s.CalibrationWatermarkDBX100
			resolutions = append(resolutions, fmt.Sprintf("segment:%s watermark explicit_db_x100:%d", s.ID, seg.WatermarkDBX100))
		}
		p.Segments = append(p.Segments, seg)
	}
	for _, ph := range doc.PhonemesFlat {
		p.Phonemes = append(p.Phonemes, Phoneme{
			ID: ph.ID, SyllableID: ph.SyllableID,
			StartSample: msToSamples(ph.StartMs, doc.SampleRateHz), EndSample: msToSamples(ph.EndMs, doc.SampleRateHz),
			Class: PhonemeClass(ph.Class), F0StartHzQ8: Q8(ph.F0StartHzX100 * 256 / 100), F0EndHzQ8: Q8(ph.F0EndHzX100 * 256 / 100), WeightPermille: ph.WeightPermille,
		})
	}
	if err := NormalizeProject(&p); err != nil {
		return nil, err
	}
	return &LoadResult{Project: &p, SourceSchema: SchemaV1, Resolutions: resolutions}, nil
}

func loadV2(doc *dtoV2) (*LoadResult, error) {
	pcm, err := decodePCM(doc.PCMBase64)
	if err != nil {
		return nil, err
	}
	tps := doc.TicksPerSecond
	if tps <= 0 {
		return nil, newError(CodeSchemaConflict, "ticksPerSecond", "v2 ticksPerSecond must be explicit and positive")
	}
	p := baseProject(doc.DialogueID, doc.Seed, doc.SampleRateHz, doc.FramesPerSecond, pcm,
		tickToSamples(doc.FrameShiftTicks, tps, doc.SampleRateHz),
		tickToSamples(doc.FrameWindowTicks, tps, doc.SampleRateHz),
		doc.EnergyCapPer10k/10)
	for _, s := range doc.Segments {
		start := tickToSamples(s.StartTick, tps, doc.SampleRateHz)
		duration := tickToSamples(s.DurationTicks, tps, doc.SampleRateHz)
		p.Segments = append(p.Segments, Segment{
			ID: s.ID, StartSample: start, EndSample: start + duration,
			GainPermille:        s.GainPerTenThousand / 10,
			TargetLevelPermille: s.TargetAmplitudePer10k / 10,
		})
		for _, syl := range s.Syllables {
			sylStart := start + tickToSamples(syl.OffsetTicks, tps, doc.SampleRateHz)
			for _, ph := range syl.Phonemes {
				phStart := sylStart + tickToSamples(ph.OffsetTicks, tps, doc.SampleRateHz)
				phEnd := phStart + tickToSamples(ph.DurationTicks, tps, doc.SampleRateHz)
				p.Phonemes = append(p.Phonemes, Phoneme{
					ID: ph.ID, SegmentID: s.ID, SyllableID: syl.ID,
					StartSample: phStart, EndSample: phEnd,
					Class:          PhonemeClass(ph.Class),
					F0StartHzQ8:    Q8(ph.F0StartHzX100 * 256 / 100),
					F0EndHzQ8:      Q8(ph.F0EndHzX100 * 256 / 100),
					WeightPermille: ph.WeightPermille,
				})
			}
		}
	}
	if err := NormalizeProject(&p); err != nil {
		return nil, err
	}
	return &LoadResult{Project: &p, SourceSchema: SchemaV2}, nil
}

func loadV3(doc *dtoV3) (*LoadResult, error) {
	pcm, err := decodePCM(doc.PCMBase64)
	if err != nil {
		return nil, err
	}
	p := baseProject(doc.DialogueID, doc.Seed, doc.SampleRateHz, doc.FramesPerSecond, pcm,
		doc.FrameShiftSamples, doc.FrameWindowSamples, doc.EnergyCapPermille)
	resolutions := []string{}
	for i, s := range doc.Segments {
		target, targetErr := targetPermilleFromDBX100(s.TargetLoudnessDBX100)
		if targetErr != nil {
			be := targetErr.(*BusinessError)
			be.Path = fmt.Sprintf("segments[%d].targetLoudnessDbX100", i)
			be.SegmentID = s.ID
			return nil, targetErr
		}
		if s.GainDBTimes1e5 < -12_000_000 || s.GainDBTimes1e5 > 6_000_000 {
			return nil, &BusinessError{Code: CodeFixedOverflow, Path: fmt.Sprintf("segments[%d].gainDbTimes1e5", i), SegmentID: s.ID, Detail: fmt.Sprintf("gain dB micro value %d outside safe conversion range", s.GainDBTimes1e5)}
		}
		gain := DBMicroToPermille(s.GainDBTimes1e5)
		seg := Segment{ID: s.ID, StartSample: s.StartSample, EndSample: s.EndSample, GainPermille: gain, TargetLevelPermille: target}
		if s.CalibrationWatermarkDBX100 != nil {
			seg.WatermarkDBX100 = *s.CalibrationWatermarkDBX100
			resolutions = append(resolutions, fmt.Sprintf("segment:%s watermark explicit_db_x100:%d", s.ID, seg.WatermarkDBX100))
		}
		p.Segments = append(p.Segments, seg)
	}
	for _, ph := range doc.Phonemes {
		p.Phonemes = append(p.Phonemes, Phoneme{ID: ph.ID, SyllableID: ph.SyllableID, StartSample: ph.StartSample, EndSample: ph.EndSample, Class: PhonemeClass(ph.Class), F0StartHzQ8: ph.F0StartHzQ8, F0EndHzQ8: ph.F0EndHzQ8, WeightPermille: ph.WeightPermille})
	}
	if err := NormalizeProject(&p); err != nil {
		return nil, err
	}
	return &LoadResult{Project: &p, SourceSchema: SchemaV3, Resolutions: resolutions}, nil
}

// MixedFragment is the explicit conflict-injection and disambiguation path.
type MixedFragment struct {
	Schema                     string `json:"schema"`
	SegmentID                  string `json:"segment_id"`
	GainPermille               *int   `json:"gain_permille,omitempty"`
	GainPer10k                 *int   `json:"gain_per_10k,omitempty"`
	TargetDBX100               *int   `json:"target_loudness_db_x100,omitempty"`
	TargetAmplitudePermille    *int   `json:"target_amplitude_permille,omitempty"`
	CalibrationWatermarkDBX100 *int   `json:"calibration_watermark_db_x100,omitempty"`
}

func LoadMixedFragmentJSON(data []byte) (*Segment, string, error) {
	var f MixedFragment
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, "", newError(CodeSchemaUnknown, "fragment", "invalid JSON: %v", err)
	}
	path := fmt.Sprintf("segments[%s]", f.SegmentID)
	var targetPermille int
	source := ""
	switch {
	case f.TargetDBX100 != nil && f.TargetAmplitudePermille != nil:
		fromDB, targetErr := targetPermilleFromDBX100(*f.TargetDBX100)
		if targetErr != nil {
			return nil, "", targetErr
		}
		if absInt64(int64(fromDB)-int64(*f.TargetAmplitudePermille)) > 1 {
			if f.CalibrationWatermarkDBX100 == nil {
				return nil, "", &BusinessError{Code: CodeSchemaConflict, Path: path, SegmentID: f.SegmentID, Detail: fmt.Sprintf("target db maps to %d permille but linear target is %d; no watermark", fromDB, *f.TargetAmplitudePermille), Context: map[string]any{"db_permille": fromDB, "linear_permille": *f.TargetAmplitudePermille}}
			}
			watermarkPermille, wmErr := targetPermilleFromDBX100(*f.CalibrationWatermarkDBX100)
			if wmErr != nil {
				return nil, "", wmErr
			}
			switch {
			case absInt64(int64(watermarkPermille)-int64(fromDB)) <= 1:
				targetPermille = fromDB
				source = "watermark:db_expression"
			case absInt64(int64(watermarkPermille)-int64(*f.TargetAmplitudePermille)) <= 1:
				targetPermille = *f.TargetAmplitudePermille
				source = "watermark:linear_expression"
			default:
				return nil, "", &BusinessError{Code: CodeSchemaConflict, Path: path + ".calibration_watermark_db_x100", SegmentID: f.SegmentID, Detail: "watermark matches neither conflicting expression"}
			}
		} else {
			targetPermille = *f.TargetAmplitudePermille
			source = "consistent_expressions"
		}
	case f.TargetDBX100 != nil:
		dbTarget, targetErr := targetPermilleFromDBX100(*f.TargetDBX100)
		if targetErr != nil {
			return nil, "", targetErr
		}
		targetPermille = dbTarget
		source = "db_expression"
	case f.TargetAmplitudePermille != nil:
		targetPermille = *f.TargetAmplitudePermille
		source = "linear_expression"
	default:
		return nil, "", newError(CodeSchemaConflict, path, "fragment has no target loudness expression")
	}

	gain := 1000
	if f.GainPermille != nil && f.GainPer10k != nil {
		if *f.GainPer10k/10 != *f.GainPermille {
			return nil, "", &BusinessError{Code: CodeSchemaConflict, Path: path + ".gain", SegmentID: f.SegmentID, Detail: fmt.Sprintf("gain_permille=%d conflicts with gain_per_10k=%d", *f.GainPermille, *f.GainPer10k)}
		}
		gain = *f.GainPermille
	} else if f.GainPermille != nil {
		gain = *f.GainPermille
	} else if f.GainPer10k != nil {
		gain = *f.GainPer10k / 10
	}
	seg := &Segment{ID: f.SegmentID, GainPermille: gain, TargetLevelPermille: targetPermille}
	if f.CalibrationWatermarkDBX100 != nil {
		seg.WatermarkDBX100 = *f.CalibrationWatermarkDBX100
	}
	return seg, source, nil
}

// MustJSON is a test/demo helper for deterministic compact output.
func MustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return b
}
