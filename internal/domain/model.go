package domain

const (
	SchemaV1 = "1.0.0"
	SchemaV2 = "2.0.0"
	SchemaV3 = "3.0.0"

	AlgorithmVersion = "present-algo-v1"
	Seed             = int64(0x5EED42)
)

type Category struct {
	Code   string `json:"code"`
	Family string `json:"family"`
	Label  string `json:"label,omitempty"`
}

type Project struct {
	SchemaVersion             string           `json:"schema_version"`
	ProjectID                 string           `json:"project_id"`
	Seed                      int64            `json:"seed"`
	SampleRateHz              int              `json:"sample_rate_hz"`
	TargetFPS                 int              `json:"target_fps"`
	SourceFPS                 int              `json:"source_fps"`
	MinReadableFrames         int              `json:"min_readable_frames"`
	TransitionToleranceFrames int              `json:"transition_tolerance_frames"`
	RoundingMode              string           `json:"rounding_mode"`
	WeightScale               int              `json:"weight_scale"`
	VolumeScale               int              `json:"volume_scale"`
	DefaultVolume             int              `json:"default_volume"`
	CategoryTable             []Category       `json:"category_table"`
	Dialogues                 []Dialogue       `json:"dialogues"`
	LoadTrace                 *ConversionTrace `json:"load_trace,omitempty"`
}

type Dialogue struct {
	ID       string    `json:"id"`
	Speaker  string    `json:"speaker"`
	Segments []Segment `json:"segments"`
}

type Segment struct {
	Index               int                  `json:"index"`
	ID                  string               `json:"id"`
	Text                string               `json:"text"`
	DurationSamples     int64                `json:"duration_samples"`
	DurationFrames      int                  `json:"duration_frames"`
	CharWeights         []int                `json:"char_weights"`
	Phonemes            []Phoneme            `json:"phonemes"`
	TransitionOverrides []TransitionOverride `json:"transition_overrides,omitempty"`
}

type Phoneme struct {
	Index        int    `json:"index"`
	Symbol       string `json:"symbol"`
	CategoryCode string `json:"category_code"`
	StartSample  int64  `json:"start_sample"`
	EndSample    int64  `json:"end_sample"`
	StartFrame   int    `json:"start_frame"`
	EndFrame     int    `json:"end_frame"`
}

type TransitionOverride struct {
	AtFrame        int    `json:"at_frame"`
	FromCategory   string `json:"from_category"`
	ToCategory     string `json:"to_category"`
	BridgeCategory string `json:"bridge_category"`
	OffsetFrames   int    `json:"offset_frames"`
	Reason         string `json:"reason"`
}

type FieldTrace struct {
	Path        string `json:"path"`
	SourceField string `json:"source_field"`
	TargetField string `json:"target_field"`
	Converter   string `json:"converter"`
	OldValue    any    `json:"old_value,omitempty"`
	NewValue    any    `json:"new_value,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	Remainder   int64  `json:"remainder,omitempty"`
}

type DefaultFill struct {
	Path         string `json:"path"`
	OldValue     any    `json:"old_value"`
	DefaultValue any    `json:"default_value"`
	Source       string `json:"source"`
}

type LossyIssue struct {
	Code        string `json:"code"`
	Path        string `json:"path"`
	Field       string `json:"field"`
	Value       any    `json:"value,omitempty"`
	Reason      string `json:"reason"`
	Disposition string `json:"disposition"`
}

type ConversionTrace struct {
	SourceVersion string        `json:"source_version"`
	TargetVersion string        `json:"target_version,omitempty"`
	Direction     string        `json:"direction"`
	Fields        []FieldTrace  `json:"fields,omitempty"`
	Fills         []DefaultFill `json:"fills,omitempty"`
	Lossy         []LossyIssue  `json:"lossy,omitempty"`
	Remainders    []any         `json:"remainders,omitempty"`
}

type CharInterval struct {
	CharIndex       int    `json:"char_index"`
	RuneStart       int    `json:"rune_start"`
	RuneEnd         int    `json:"rune_end"`
	ByteStart       int    `json:"byte_start"`
	ByteEnd         int    `json:"byte_end"`
	Char            string `json:"char"`
	WeightPermyriad int    `json:"weight_permyriad"`
	StartFrame      int    `json:"start_frame"`
	EndFrame        int    `json:"end_frame"`
}

type FrameCharRange struct {
	Frame              int    `json:"frame"`
	CharIndex          int    `json:"char_index"`
	RuneStart          int    `json:"rune_start"`
	RuneEnd            int    `json:"rune_end"`
	ByteStart          int    `json:"byte_start"`
	ByteEnd            int    `json:"byte_end"`
	Char               string `json:"char"`
	IntervalStartFrame int    `json:"interval_start_frame"`
	IntervalEndFrame   int    `json:"interval_end_frame"`
}

type LipFrame struct {
	BaseFrame     int    `json:"base_frame"`
	SequenceFrame int    `json:"sequence_frame"`
	PhonemeIndex  int    `json:"phoneme_index"`
	Category      string `json:"category"`
	Family        string `json:"family"`
	Inserted      bool   `json:"inserted"`
	TransitionID  string `json:"transition_id,omitempty"`
}

type Transition struct {
	ID               string `json:"id"`
	DialogueID       string `json:"dialogue_id"`
	PartitionID      string `json:"partition_id"`
	BoundaryFrame    int    `json:"boundary_frame"`
	InsertAtSequence int    `json:"insert_at_sequence"`
	OffsetFrames     int    `json:"offset_frames"`
	FromCategory     string `json:"from_category"`
	ToCategory       string `json:"to_category"`
	BridgeCategory   string `json:"bridge_category"`
	Rule             string `json:"rule"`
	Reason           string `json:"reason"`
}

type CompressionRecord struct {
	Frame        int    `json:"frame"`
	CharIndex    int    `json:"char_index"`
	Char         string `json:"char"`
	Priority     int    `json:"priority"`
	BeforeLength int    `json:"before_length"`
	AfterLength  int    `json:"after_length"`
	Rule         string `json:"rule"`
}

type FrameDigest struct {
	BaseFrame     int    `json:"base_frame"`
	SequenceFrame int    `json:"sequence_frame"`
	CharIndex     int    `json:"char_index"`
	Category      string `json:"category"`
	Inserted      bool   `json:"inserted"`
	Digest        string `json:"digest"`
}

type PartitionResult struct {
	DialogueID        string              `json:"dialogue_id"`
	DialogueIndex     int                 `json:"dialogue_index"`
	PartitionID       string              `json:"partition_id"`
	PartitionIndex    int                 `json:"partition_index"`
	FrameCount        int                 `json:"frame_count"`
	LipSequenceLength int                 `json:"lip_sequence_length"`
	SourceSchema      string              `json:"source_schema"`
	ContentHash       string              `json:"content_hash"`
	UpstreamKey       string              `json:"upstream_key"`
	CacheKey          string              `json:"cache_key"`
	QuantParamVersion string              `json:"quant_param_version"`
	AlgorithmVersion  string              `json:"algorithm_version"`
	CharIntervals     []CharInterval      `json:"char_intervals"`
	CharFrames        []FrameCharRange    `json:"char_frames"`
	LipFrames         []LipFrame          `json:"lip_frames"`
	Transitions       []Transition        `json:"transitions"`
	Compressions      []CompressionRecord `json:"compressions,omitempty"`
	FrameDigests      []FrameDigest       `json:"frame_digests"`
	Digest            string              `json:"digest"`
	CommitSequence    int                 `json:"commit_sequence,omitempty"`
}

type PartitionRef struct {
	DialogueID     string `json:"dialogue_id"`
	PartitionID    string `json:"partition_id"`
	PartitionIndex int    `json:"partition_index"`
}

func MakePartitionID(dialogueID string, partitionIndex int) string {
	return dialogueID + "-P" + itoa(partitionIndex)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
