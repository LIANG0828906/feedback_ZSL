package model

const AlgorithmVersion = "dialoguefit/v1"

type Category string

const (
	CategoryVowel     Category = "vowel"
	CategoryConsonant Category = "consonant"
	CategorySilence   Category = "silence"
)

func (c Category) Valid() bool {
	switch c {
	case CategoryVowel, CategoryConsonant, CategorySilence:
		return true
	default:
		return false
	}
}

type EnvelopeConfig struct {
	FrameLength  int `json:"frame_length"`
	FrameShift   int `json:"frame_shift"`
	SmoothWindow int `json:"smooth_window"`
}

type FitParams struct {
	TargetSamples          int     `json:"target_samples"`
	MaxStretch             float64 `json:"max_stretch"`
	MinSilenceSamples      int     `json:"min_silence_samples"`
	StressToleranceSamples int     `json:"stress_tolerance_samples"`
}

type Edit struct {
	DialogueID             string   `json:"DialogueID"`
	TargetSamples          *int     `json:"TargetSamples,omitempty"`
	MaxStretch             *float64 `json:"MaxStretch,omitempty"`
	MinSilenceSamples      *int     `json:"MinSilenceSamples,omitempty"`
	StressToleranceSamples *int     `json:"StressToleranceSamples,omitempty"`
}

type PCMDescriptor struct {
	Encoding    string  `json:"encoding"`
	SampleRate  int     `json:"sample_rate"`
	SampleCount int     `json:"sample_count"`
	Data        string  `json:"data,omitempty"`
	Samples     []int16 `json:"-"`
}

type Phoneme struct {
	ID             string    `json:"id"`
	Index          int       `json:"index"`
	Category       Category  `json:"category"`
	StartSample    int       `json:"start_sample"`
	EndSample      int       `json:"end_sample"`
	F0StepSamples  int       `json:"f0_step_samples"`
	F0             []float64 `json:"f0"`
	LoudnessWeight float64   `json:"loudness_weight"`
	Stretchable    bool      `json:"stretchable"`
}

func (p Phoneme) Duration() int { return p.EndSample - p.StartSample }

type Dialogue struct {
	ID       string        `json:"id"`
	PCM      PCMDescriptor `json:"pcm"`
	Phonemes []Phoneme     `json:"phonemes"`
	Params   FitParams     `json:"params"`
}

type Project struct {
	SchemaVersion    string         `json:"schema_version"`
	Seed             int64          `json:"seed"`
	AlgorithmVersion string         `json:"algorithm_version"`
	Envelope         EnvelopeConfig `json:"envelope"`
	Dialogues        []Dialogue     `json:"dialogues"`
}

type Frame struct {
	Index        int     `json:"index"`
	StartSample  int     `json:"start_sample"`
	CenterSample int     `json:"center_sample"`
	EndSample    int     `json:"end_sample"`
	Value        float64 `json:"value"`
}

type StressMark struct {
	PhonemeID       string `json:"phoneme_id"`
	Index           int    `json:"index"`
	Sample          int    `json:"sample"`
	AlignedSample   int    `json:"aligned_sample"`
	Distance        int    `json:"distance,omitempty"`
	WithinTolerance bool   `json:"within_tolerance"`
}

type StretchSegment struct {
	PhonemeID     string  `json:"phoneme_id"`
	Index         int     `json:"index"`
	SourceStart   int     `json:"source_start"`
	SourceEnd     int     `json:"source_end"`
	StartSample   int     `json:"start_sample"`
	EndSample     int     `json:"end_sample"`
	StretchFactor float64 `json:"stretch_factor"`
	AddedSamples  int     `json:"added_samples"`
	HitCap        bool    `json:"hit_cap"`
	Basis         string  `json:"basis"`
}

type TruthStress struct {
	PhonemeID     string `json:"phoneme_id"`
	Index         int    `json:"index"`
	Sample        int    `json:"sample"`
	AlignedSample int    `json:"aligned_sample"`
}

type TruthDialogue struct {
	DialogueID       string           `json:"dialogue_id"`
	SampleRate       int              `json:"sample_rate"`
	SampleCount      int              `json:"sample_count"`
	ContentHash      string           `json:"content_hash"`
	AlgorithmVersion string           `json:"algorithm_version"`
	EnvelopeParams   EnvelopeConfig   `json:"envelope_params"`
	FitParams        FitParams        `json:"fit_params"`
	FrameValues      []float64        `json:"frame_values"`
	Stress           []TruthStress    `json:"stress"`
	Stretched        []StretchSegment `json:"stretched"`
	Fingerprint      string           `json:"fingerprint"`
}

type TruthFile struct {
	SchemaVersion    string          `json:"schema_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
	Dialogues        []TruthDialogue `json:"dialogues"`
}

type PhonemeResult struct {
	DialogueID string            `json:"dialogue_id"`
	Index      int               `json:"phoneme_index"`
	PhonemeID  string            `json:"phoneme_id"`
	Category   Category          `json:"category,omitempty"`
	Frames     []Frame           `json:"frames"`
	Stress     *StressMark       `json:"stress,omitempty"`
	Segment    *StretchSegment   `json:"segment,omitempty"`
	CacheKey   string            `json:"cache_key"`
	Upstream   []string          `json:"upstream_keys,omitempty"`
	Nodes      map[string]string `json:"nodes,omitempty"`
}

type DialogueResult struct {
	DialogueID       string           `json:"dialogue_id"`
	ContentHash      string           `json:"content_hash"`
	ParamVersion     string           `json:"param_version"`
	AlgorithmVersion string           `json:"algorithm_version"`
	TruthFingerprint string           `json:"truth_fingerprint,omitempty"`
	Params           FitParams        `json:"params"`
	Frames           []Frame          `json:"frames"`
	Stress           []StressMark     `json:"stress"`
	Segments         []StretchSegment `json:"segments"`
	PhonemeResults   []PhonemeResult  `json:"phoneme_results,omitempty"`
	CommitSequence   []string         `json:"commit_sequence"`
	State            string           `json:"state"`
}

type InvalidationEntry struct {
	DialogueID   string `json:"dialogue_id"`
	PhonemeIndex int    `json:"phoneme_index"`
	PhonemeID    string `json:"phoneme_id,omitempty"`
	Reason       string `json:"reason"`
	Node         string `json:"node"`
}

type InvalidationReport struct {
	Entries            []InvalidationEntry `json:"entries"`
	RecomputedPhonemes int                 `json:"recomputed_phonemes"`
	ReusedPhonemes     int                 `json:"reused_phonemes"`
	ReusedDialogues    int                 `json:"reused_dialogues,omitempty"`
	ComputedDialogues  int                 `json:"computed_dialogues,omitempty"`
}

type QueueEvent struct {
	TaskID   string `json:"task_id"`
	Reason   string `json:"reason"`
	WaitMS   int64  `json:"wait_ms"`
	WaitUS   int64  `json:"wait_us"`
	AtUnixMS int64  `json:"at_unix_ms"`
}
