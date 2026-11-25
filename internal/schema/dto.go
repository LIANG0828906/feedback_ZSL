package schema

import "prosodyalign/internal/model"

const (
	V1Name = string(model.SchemaV1)
	V2Name = string(model.SchemaV2)
	V3Name = string(model.SchemaV3)
)

type v1F0Point struct {
	OffsetSample int64  `json:"offset_sample"`
	CentiHz      *int64 `json:"f0_centi_hz"`
	XMilliHz     *int64 `json:"x_f0_milli_hz,omitempty"`
}

type v1Phoneme struct {
	PhonemeID              string      `json:"phoneme_id"`
	StartSample            *int64      `json:"start_sample"`
	EndSample              *int64      `json:"end_sample"`
	Class                  *string     `json:"class"`
	Stretchable            bool        `json:"stretchable"`
	LoudnessWeightPermille *int64      `json:"loudness_weight_permille"`
	F0                     []v1F0Point `json:"f0_centi_hz_trajectory"`
	XMaxStretchSamples     *int64      `json:"x_max_stretch_samples,omitempty"`
}

type v1Envelope struct {
	FrameLengthSamples int   `json:"frame_length_samples,omitempty"`
	HopSamples         int   `json:"hop_samples,omitempty"`
	SmoothWindow       int   `json:"smooth_window,omitempty"`
	FixedPointScale    int64 `json:"fixed_point_scale,omitempty"`
}

type v1File struct {
	Schema                    string      `json:"schema"`
	DialogID                  string      `json:"dialog_id"`
	Seed                      int64       `json:"seed"`
	SampleRateHz              int         `json:"sample_rate_hz"`
	SampleCount               int         `json:"sample_count,omitempty"`
	PCM                       []int16     `json:"pcm"`
	TargetDurationSamples     *int64      `json:"target_duration_samples"`
	DefaultMaxStretchSamples  *int64      `json:"default_max_stretch_samples,omitempty"`
	XDefaultMaxStretchSamples *int64      `json:"x_default_max_stretch_samples,omitempty"`
	MinSilenceHoldSamples     *int64      `json:"min_silence_hold_samples"`
	AccentToleranceSamples    *int64      `json:"accent_tolerance_samples"`
	Envelope                  *v1Envelope `json:"envelope,omitempty"`
	Phonemes                  []v1Phoneme `json:"phonemes"`
}

type v2F0Point struct {
	OffsetMs int64  `json:"offsetMs"`
	MilliHz  int64  `json:"f0MilliHz"`
	XMilliHz *int64 `json:"xExactMilliHz,omitempty"`
}

type v2Phoneme struct {
	PhonemeID    string      `json:"phonemeId"`
	StartMs      *int64      `json:"startMs"`
	EndMs        *int64      `json:"endMs"`
	Kind         *string     `json:"kind"`
	Stretchable  bool        `json:"stretchable"`
	WeightPer10k *int64      `json:"weightPer10k"`
	F0           []v2F0Point `json:"f0Trajectory"`
	MaxStretchMs *int64      `json:"maxStretchMs,omitempty"`
}

type v2Envelope struct {
	FrameLengthSamples int   `json:"frameLengthSamples,omitempty"`
	HopSamples         int   `json:"hopSamples,omitempty"`
	SmoothWindow       int   `json:"smoothWindow,omitempty"`
	FixedPointScale    int64 `json:"fixedPointScale,omitempty"`
}

type v2File struct {
	Schema              string      `json:"schema"`
	DialogID            string      `json:"dialogId"`
	Seed                int64       `json:"seed"`
	SampleRateHz        int         `json:"sampleRateHz"`
	SampleCount         int         `json:"sampleCount,omitempty"`
	PCM                 []int16     `json:"pcm"`
	TargetDurationMs    *int64      `json:"targetDurationMs"`
	DefaultMaxStretchMs *int64      `json:"maxStretchMs"`
	MinSilenceHoldMs    *int64      `json:"minSilenceHoldMs"`
	AccentToleranceMs   *int64      `json:"accentToleranceMs"`
	Envelope            *v2Envelope `json:"envelope,omitempty"`
	Phonemes            []v2Phoneme `json:"phonemes"`
}

type v3Timing struct {
	Basis                    string `json:"basis"`
	TargetDurationSamples    int64  `json:"target_duration_samples"`
	DefaultMaxStretchSamples int64  `json:"default_max_stretch_samples"`
	MinSilenceHoldSamples    int64  `json:"min_silence_hold_samples"`
	AccentToleranceSamples   int64  `json:"accent_tolerance_samples"`
}

type v3Phoneme struct {
	Index             int               `json:"index"`
	ID                string            `json:"id"`
	Range             model.Range       `json:"range"`
	Kind              model.PhonemeKind `json:"kind"`
	Stretchable       bool              `json:"stretchable"`
	WeightPer10k      int64             `json:"weight_per_10k"`
	F0                []model.F0Point   `json:"f0_trajectory"`
	MaxStretchSamples *int64            `json:"max_stretch_samples,omitempty"`
}

type v3File struct {
	Schema       string               `json:"schema"`
	ID           string               `json:"id"`
	Seed         int64                `json:"seed"`
	SampleRateHz int                  `json:"sample_rate_hz"`
	SampleCount  int                  `json:"sample_count,omitempty"`
	PCM          []int16              `json:"pcm"`
	Timing       v3Timing             `json:"timing"`
	Envelope     model.EnvelopeParams `json:"envelope"`
	Phonemes     []v3Phoneme          `json:"phonemes"`
}
