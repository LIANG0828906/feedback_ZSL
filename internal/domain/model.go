package domain

import "time"

type SchemaVersion string

const (
	SchemaV1 SchemaVersion = "lip.project/v1"
	SchemaV2 SchemaVersion = "lip.project/v2"
	SchemaV3 SchemaVersion = "lip.project/v3"

	AlgorithmVersion = "lip-algo-1.0.0"
)

type Category string

const (
	CategoryVowel     Category = "vowel"
	CategoryConsonant Category = "consonant"
	CategorySilence   Category = "silence"
)

type MouthClass string

const (
	MouthSilence   MouthClass = "X"
	MouthConsonant MouthClass = "C"
	MouthVowel     MouthClass = "V"
	MouthBridge    MouthClass = "T"
)

type Params struct {
	SampleRate               int    `json:"sample_rate"`
	FPS                      int    `json:"fps"`
	QuantizationRule         string `json:"quantization_rule"`
	RoundingRule             string `json:"rounding_rule"`
	TransitionTolerance      int    `json:"transition_tolerance"`
	MaxPhonemesPerPack       int    `json:"max_phonemes_per_pack"`
	AffinityMergeThreshold   int    `json:"affinity_merge_threshold_permyriad"`
	PreviewLatencyBudgetMS   int    `json:"preview_latency_budget_ms"`
	PreviewFrameStride       int    `json:"preview_frame_stride"`
	PreviewMaxTransitionScan int    `json:"preview_max_transition_scan"`
	ParamVersion             string `json:"param_version"`
}

func DefaultParams() Params {
	return Params{
		SampleRate:               48000,
		FPS:                      30,
		QuantizationRule:         "floor(sample*fps/sample_rate)",
		RoundingRule:             "floor_non_negative",
		TransitionTolerance:      2,
		MaxPhonemesPerPack:       3,
		AffinityMergeThreshold:   5000,
		PreviewLatencyBudgetMS:   250,
		PreviewFrameStride:       2,
		PreviewMaxTransitionScan: 64,
		ParamVersion:             "params-default-v1",
	}
}

type ChannelSpec struct {
	ID      string `json:"id"`
	Speaker string `json:"speaker"`
	Slots   int    `json:"slots"`
}

type PCMDescription struct {
	Encoding      string `json:"encoding"`
	Channels      int    `json:"channels"`
	SampleRate    int    `json:"sample_rate"`
	BitsPerSample int    `json:"bits_per_sample"`
	SampleCount   int64  `json:"sample_count"`
	ByteCount     int64  `json:"byte_count"`
	Deterministic bool   `json:"deterministic"`
	GeneratorSeed int64  `json:"generator_seed"`
}

type Phoneme struct {
	ID               string   `json:"id"`
	DialogueID       string   `json:"dialogue_id"`
	ShardID          string   `json:"shard_id"`
	Index            int      `json:"index"`
	StartSample      int64    `json:"start_sample"`
	EndSample        int64    `json:"end_sample"`
	Category         Category `json:"category"`
	CategoryCode     string   `json:"category_code"`
	EncodingGroup    string   `json:"encoding_group,omitempty"`
	WeightPermyriad  int      `json:"weight_permyriad"`
	AffinityToNext   int      `json:"affinity_to_next_permyriad"`
	AffinityExplicit bool     `json:"affinity_explicit"`
}

type Shard struct {
	ID            string         `json:"id"`
	DialogueID    string         `json:"dialogue_id"`
	Index         int            `json:"index"`
	ChannelID     string         `json:"channel_id"`
	TotalSamples  int64          `json:"total_samples"`
	PCM           PCMDescription `json:"pcm"`
	Phonemes      []Phoneme      `json:"phonemes"`
	InjectFailure bool           `json:"inject_failure,omitempty"`
}

type Dialogue struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Speaker       string         `json:"speaker"`
	ChannelID     string         `json:"channel_id"`
	SourceVersion SchemaVersion  `json:"source_version"`
	ContentHash   string         `json:"content_hash"`
	PCM           PCMDescription `json:"pcm"`
	Shards        []Shard        `json:"shards"`
}

type Project struct {
	ID            string        `json:"id"`
	Seed          int64         `json:"seed"`
	Params        Params        `json:"params"`
	Channels      []ChannelSpec `json:"channels"`
	Dialogues     []Dialogue    `json:"dialogues"`
	SourceVersion SchemaVersion `json:"source_version"`
	GeneratedAt   time.Time     `json:"generated_at"`
}

type FillRecord struct {
	Path         string        `json:"path"`
	OldValue     any           `json:"old_value"`
	DefaultValue any           `json:"default_value"`
	Reason       string        `json:"reason"`
	Version      SchemaVersion `json:"version"`
}

type ConversionRecord struct {
	Path        string `json:"path"`
	Field       string `json:"field"`
	FromUnit    string `json:"from_unit"`
	ToUnit      string `json:"to_unit"`
	Input       int64  `json:"input"`
	Output      int64  `json:"output"`
	Remainder   int64  `json:"remainder"`
	Rounding    string `json:"rounding"`
	TruncatedAt string `json:"truncated_at"`
}

type LoadReport struct {
	Version     SchemaVersion      `json:"version"`
	Fills       []FillRecord       `json:"fills"`
	Conversions []ConversionRecord `json:"conversions"`
	Losses      []*Error           `json:"losses,omitempty"`
}

type Pack struct {
	Index        int        `json:"index"`
	DialogueID   string     `json:"dialogue_id"`
	ShardID      string     `json:"shard_id"`
	PhonemeIDs   []string   `json:"phoneme_ids"`
	StartSample  int64      `json:"start_sample"`
	EndSample    int64      `json:"end_sample"`
	StartFrame   int64      `json:"start_frame"`
	EndFrame     int64      `json:"end_frame"`
	FrameCount   int64      `json:"frame_count"`
	Categories   []Category `json:"categories"`
	Digest       string     `json:"digest"`
	CacheKey     string     `json:"cache_key,omitempty"`
	UpstreamKeys []string   `json:"upstream_keys,omitempty"`
}

type FrameKind string

const (
	FrameStable     FrameKind = "stable"
	FrameTransition FrameKind = "transition"
)

type Frame struct {
	Index          int64      `json:"index"`
	DialogueID     string     `json:"dialogue_id"`
	ShardID        string     `json:"shard_id"`
	PackIndex      int        `json:"pack_index"`
	PhonemeID      string     `json:"phoneme_id"`
	Category       Category   `json:"category"`
	Mouth          MouthClass `json:"mouth"`
	Kind           FrameKind  `json:"kind"`
	TransitionFrom MouthClass `json:"transition_from,omitempty"`
	TransitionTo   MouthClass `json:"transition_to,omitempty"`
	FamilySpan     int        `json:"family_span,omitempty"`
	Sampled        bool       `json:"sampled,omitempty"`
}

type TransitionMark struct {
	Frame      int64      `json:"frame"`
	DialogueID string     `json:"dialogue_id"`
	ShardID    string     `json:"shard_id"`
	PackIndex  int        `json:"pack_index"`
	From       MouthClass `json:"from"`
	To         MouthClass `json:"to"`
	Bridge     MouthClass `json:"bridge"`
	FamilySpan int        `json:"family_span"`
	Inserted   bool       `json:"inserted"`
}

type ShardResult struct {
	DialogueID       string           `json:"dialogue_id"`
	ShardID          string           `json:"shard_id"`
	ShardIndex       int              `json:"shard_index"`
	ChannelID        string           `json:"channel_id"`
	TotalFrames      int64            `json:"total_frames"`
	Packs            []Pack           `json:"packs"`
	Frames           []Frame          `json:"frames"`
	Transitions      []TransitionMark `json:"transitions"`
	ContentHash      string           `json:"content_hash"`
	ParamVersion     string           `json:"param_version"`
	AlgorithmVersion string           `json:"algorithm_version"`
	SourceVersion    SchemaVersion    `json:"source_version"`
}

type DispatchRecord struct {
	DialogueID string     `json:"dialogue_id"`
	ShardID    string     `json:"shard_id"`
	FrameIndex int64      `json:"frame_index"`
	PackIndex  int        `json:"pack_index"`
	ChannelID  string     `json:"channel_id"`
	Mouth      MouthClass `json:"mouth"`
	Kind       FrameKind  `json:"kind"`
	CacheKey   string     `json:"cache_key"`
}

type PreviewStats struct {
	SourceFPS                int    `json:"source_fps"`
	PreviewFPS               int    `json:"preview_fps"`
	FrameStride              int    `json:"frame_stride"`
	SampleDensityPermyriad   int    `json:"sample_density_permyriad"`
	TotalFrames              int64  `json:"total_frames"`
	EvaluatedFrames          int64  `json:"evaluated_frames"`
	SkippedFrames            int64  `json:"skipped_frames"`
	MaxTransitionScan        int    `json:"max_transition_scan"`
	ActualTransitionScan     int    `json:"actual_transition_scan"`
	ElapsedMicros            int64  `json:"elapsed_micros"`
	BudgetMillis             int    `json:"budget_millis"`
	BudgetOccupancyPermyriad int64  `json:"budget_occupancy_permyriad"`
	WithinBudget             bool   `json:"within_budget"`
	Basis                    string `json:"basis"`
}

type PreviewResult struct {
	ShardID       string           `json:"shard_id"`
	Params        Params           `json:"params"`
	SampledFrames []Frame          `json:"sampled_frames"`
	Transitions   []TransitionMark `json:"transitions"`
	Stats         PreviewStats     `json:"stats"`
}

type Divergence struct {
	FirstFrame int64  `json:"first_frame"`
	PackIndex  int    `json:"pack_index"`
	Preview    any    `json:"preview"`
	Exact      any    `json:"exact"`
	Field      string `json:"field"`
}
