package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type SchemaName string

const (
	SchemaV1 SchemaName = "prosody.v1"
	SchemaV2 SchemaName = "prosody.v2"
	SchemaV3 SchemaName = "prosody.v3"
)

type PhonemeKind string

const (
	Vowel     PhonemeKind = "vowel"
	Consonant PhonemeKind = "consonant"
	Silence   PhonemeKind = "silence"
)

type F0Point struct {
	OffsetSample int64 `json:"offset_sample"`
	MilliHz      int64 `json:"f0_milli_hz"`
}

type Phoneme struct {
	Index             int         `json:"index"`
	ID                string      `json:"id"`
	StartSample       int64       `json:"start_sample"`
	EndSample         int64       `json:"end_sample"`
	Kind              PhonemeKind `json:"kind"`
	Stretchable       bool        `json:"stretchable"`
	WeightPer10k      int64       `json:"weight_per_10k"`
	F0                []F0Point   `json:"f0"`
	MaxStretchSamples *int64      `json:"max_stretch_samples,omitempty"`
	Edited            bool        `json:"edited,omitempty"`
}

func (p Phoneme) DurationSamples() int64 { return p.EndSample - p.StartSample }

type EnvelopeParams struct {
	FrameLengthSamples int   `json:"frame_length_samples"`
	HopSamples         int   `json:"hop_samples"`
	SmoothWindow       int   `json:"smooth_window"`
	FixedPointScale    int64 `json:"fixed_point_scale"`
}

func DefaultEnvelopeParams(sampleRate int) EnvelopeParams {
	frame := sampleRate / 100
	if frame < 16 {
		frame = 32
	}
	hop := frame / 2
	if hop < 1 {
		hop = 1
	}
	return EnvelopeParams{FrameLengthSamples: frame, HopSamples: hop, SmoothWindow: 3, FixedPointScale: 1_000_000}
}

type FitParameters struct {
	TargetDurationSamples int64 `json:"target_duration_samples"`
	DefaultMaxStretch     int64 `json:"default_max_stretch_samples"`
	MinSilenceHoldSamples int64 `json:"min_silence_hold_samples"`
	AccentTolerance       int64 `json:"accent_tolerance_samples"`
}

type Project struct {
	SchemaVersion          SchemaName     `json:"schema_version"`
	SourceVersion          string         `json:"source_version"`
	ID                     string         `json:"id"`
	Seed                   int64          `json:"seed"`
	SampleRateHz           int            `json:"sample_rate_hz"`
	SampleCount            int            `json:"sample_count"`
	PCM                    []int16        `json:"pcm,omitempty"`
	Tempo                  float64        `json:"-"`
	DeclaredTargetDuration int64          `json:"declared_target_duration_samples"`
	DeclaredMaxStretch     int64          `json:"declared_max_stretch_samples"`
	MinSilenceHoldSamples  int64          `json:"min_silence_hold_samples"`
	AccentToleranceSamples int64          `json:"accent_tolerance_samples"`
	EnvelopeParams         EnvelopeParams `json:"envelope_params"`
	Phonemes               []Phoneme      `json:"phonemes"`
	ContentHash            string         `json:"content_hash"`
	LoadedFrom             string         `json:"loaded_from,omitempty"`
}

func (p *Project) FitParameters() FitParameters {
	return FitParameters{
		TargetDurationSamples: p.DeclaredTargetDuration,
		DefaultMaxStretch:     p.DeclaredMaxStretch,
		MinSilenceHoldSamples: p.MinSilenceHoldSamples,
		AccentTolerance:       p.AccentToleranceSamples,
	}
}

type Range struct {
	StartSample int64 `json:"start_sample"`
	EndSample   int64 `json:"end_sample"`
}

type FrameResult struct {
	FrameIndex       int   `json:"frame_index"`
	StartSample      int64 `json:"start_sample"`
	CenterSample     int64 `json:"center_sample"`
	RawLoudness      int64 `json:"raw_loudness"`
	SmoothedLoudness int64 `json:"smoothed_loudness"`
	EnvelopeFixed    int64 `json:"envelope_fixed"`
	PhonemeIndex     int   `json:"phoneme_index"`
}

type AccentResult struct {
	Sample           int64 `json:"sample"`
	PhonemeIndex     int   `json:"phoneme_index"`
	LoudnessPeak     int64 `json:"loudness_peak_sample"`
	PitchPeak        int64 `json:"pitch_peak_sample"`
	DistanceSamples  int64 `json:"distance_samples"`
	ToleranceSamples int64 `json:"tolerance_samples"`
	FinalFrameIndex  int   `json:"final_frame_index"`
}

type SegmentFit struct {
	PhonemeIndex        int    `json:"phoneme_index"`
	OriginalStart       int64  `json:"original_start"`
	OriginalEnd         int64  `json:"original_end"`
	OriginalDuration    int64  `json:"original_duration"`
	AddedSamples        int64  `json:"added_samples"`
	NewStart            int64  `json:"new_start"`
	NewEnd              int64  `json:"new_end"`
	NewDuration         int64  `json:"new_duration"`
	CoefficientPermille int64  `json:"coefficient_permille"`
	CapSamples          int64  `json:"cap_samples"`
	CapTriggered        bool   `json:"cap_triggered"`
	WeightPer10k        int64  `json:"weight_per_10k"`
	Reason              string `json:"reason"`
}

type FitResult struct {
	TargetDuration int64         `json:"target_duration"`
	FinalDuration  int64         `json:"final_duration"`
	Gap            int64         `json:"gap"`
	Allocated      int64         `json:"allocated"`
	Shortfall      int64         `json:"shortfall"`
	Segments       []SegmentFit  `json:"segments"`
	Accent         *AccentResult `json:"accent,omitempty"`
	WarpedAccent   int64         `json:"warped_accent_sample"`
	Feasible       bool          `json:"feasible"`
}

type DialogResult struct {
	DialogID         string         `json:"dialog_id"`
	FragmentID       string         `json:"fragment_id"`
	ContentHash      string         `json:"content_hash"`
	ParameterHash    string         `json:"parameter_hash"`
	AlgorithmVersion string         `json:"algorithm_version"`
	Frames           []FrameResult  `json:"frames"`
	Accent           AccentResult   `json:"accent"`
	Fit              FitResult      `json:"fit"`
	EnvelopeDigest   string         `json:"envelope_digest"`
	Scheduler        map[string]any `json:"scheduler,omitempty"`
}

func CanonicalContentHash(p *Project) string {
	cp := *p
	cp.ContentHash = ""
	cp.SchemaVersion = ""
	cp.SourceVersion = ""
	cp.LoadedFrom = ""
	cp.PCM = append([]int16(nil), p.PCM...)
	data, _ := json.Marshal(cp)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashCanonical(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("hash-error:%v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}
