package lipsync

import "time"

const AlgorithmVersion = "lipsync-algo-1.0.0"

type PhonemeClass string

const (
	ClassVowel     PhonemeClass = "vowel"
	ClassConsonant PhonemeClass = "consonant"
	ClassSilence   PhonemeClass = "silence"
)

type QuantizeDirection string

const (
	QuantizeDown    QuantizeDirection = "down"
	QuantizeUp      QuantizeDirection = "up"
	QuantizeNearest QuantizeDirection = "nearest"
)

type WeightUnit string

const (
	WeightPermille  WeightUnit = "permille"
	WeightPermyriad WeightUnit = "permyriad"
)

type PCMDescription struct {
	Encoding    string  `json:"encoding"`
	Channels    int     `json:"channels"`
	SampleRate  int     `json:"sampleRate"`
	SampleCount int     `json:"sampleCount"`
	Seed        int64   `json:"seed"`
	SHA256      string  `json:"sha256"`
	Samples     []int16 `json:"-"`
}

type Phoneme struct {
	ID             string       `json:"id"`
	Index          int          `json:"index"`
	StartSample    int          `json:"startSample"`
	EndSample      int          `json:"endSample"`
	Class          PhonemeClass `json:"class"`
	Weight         int          `json:"weight"`
	WeightUnit     WeightUnit   `json:"weightUnit"`
	AffinityToNext *int         `json:"affinityToNext,omitempty"`
}

type Dialogue struct {
	ID          string         `json:"id"`
	ShardIndex  int            `json:"shardIndex"`
	PCM         PCMDescription `json:"pcm"`
	Phonemes    []Phoneme      `json:"phonemes"`
	ContentHash string         `json:"contentHash"`
}

type Recipe struct {
	FPS                    int               `json:"fps"`
	MaxPhonemesPerPack     int               `json:"maxPhonemesPerPack"`
	MinVisibleFrames       int               `json:"minVisibleFrames"`
	SpeakerChannels        int               `json:"speakerChannels"`
	FramePackBudget        int               `json:"framePackBudget"`
	Quantization           QuantizeDirection `json:"quantization"`
	MergeAffinityThreshold int               `json:"mergeAffinityThreshold"`
	MaxDeferFrames         int               `json:"maxDeferFrames"`
	RecipeVersion          string            `json:"recipeVersion"`
}

type Project struct {
	ID               string      `json:"id"`
	Seed             int64       `json:"seed"`
	AlgorithmVersion string      `json:"algorithmVersion"`
	Recipe           Recipe      `json:"recipe"`
	Dialogues        []*Dialogue `json:"dialogues"`
}

type Truncation struct {
	DialogueID      string            `json:"dialogueId"`
	ShardIndex      int               `json:"shardIndex"`
	PhonemeID       string            `json:"phonemeId"`
	Boundary        string            `json:"boundary"`
	Sample          int               `json:"sample"`
	Direction       QuantizeDirection `json:"direction"`
	ScaledNumerator int64             `json:"scaledNumerator"`
	Denominator     int64             `json:"denominator"`
	Remainder       int64             `json:"remainder"`
	Frame           int               `json:"frame"`
}

type QuantizedPhoneme struct {
	Phoneme
	StartFrame      int `json:"startFrame"`
	EndFrame        int `json:"endFrame"`
	VisibleFrames   int `json:"visibleFrames"`
	WeightPermyriad int `json:"weightPermyriad"`
	WeightPermille  int `json:"weightPermille"`
	WeightRemainder int `json:"weightRemainder"`
}

type BoundaryDecision struct {
	BoundaryIndex    int    `json:"boundaryIndex"`
	AfterPhonemeID   string `json:"afterPhonemeId"`
	Affinity         int    `json:"affinity"`
	StartFrame       int    `json:"startFrame"`
	Eligible         bool   `json:"eligible"`
	Selected         bool   `json:"selected"`
	Reason           string `json:"reason"`
	Rule             string `json:"rule,omitempty"`
	TieBreakApplied  bool   `json:"tieBreakApplied"`
	TieBreakRule     string `json:"tieBreakRule,omitempty"`
	GroupStartFrames []int  `json:"groupStartFrames,omitempty"`
}

type RepairDecision struct {
	PackIndex       int    `json:"packIndex"`
	WithPackIndex   int    `json:"withPackIndex"`
	BoundaryIndex   int    `json:"boundaryIndex"`
	Affinity        int    `json:"affinity"`
	StartFrame      int    `json:"startFrame"`
	VisibleFrames   int    `json:"visibleFrames"`
	Reason          string `json:"reason"`
	TieBreakApplied bool   `json:"tieBreakApplied"`
}

type PackDecision struct {
	Rule         string             `json:"rule"`
	Boundaries   []BoundaryDecision `json:"boundaries"`
	Repairs      []RepairDecision   `json:"repairs,omitempty"`
	TieBreakRule string             `json:"tieBreakRule"`
	Rationale    string             `json:"rationale"`
}

type PackPhonemeDetail struct {
	PhonemeID       string `json:"phonemeId"`
	StartFrame      int    `json:"startFrame"`
	EndFrame        int    `json:"endFrame"`
	WeightPermyriad int    `json:"weightPermyriad"`
	WeightPermille  int    `json:"weightPermille"`
	Dominant        bool   `json:"dominant"`
}

type Pack struct {
	DialogueID     string              `json:"dialogueId"`
	ShardIndex     int                 `json:"shardIndex"`
	Index          int                 `json:"index"`
	PackID         string              `json:"packId"`
	PhonemeIDs     []string            `json:"phonemeIds"`
	StartFrame     int                 `json:"startFrame"`
	EndFrame       int                 `json:"endFrame"`
	LastFrame      int                 `json:"lastFrame"`
	VisibleFrames  int                 `json:"visibleFrames"`
	SummaryHash    string              `json:"summaryHash"`
	PhonemeDetails []PackPhonemeDetail `json:"phonemeDetails"`
	Decision       PackDecision        `json:"decision"`
}

type FrameAttribution struct {
	DialogueID    string       `json:"dialogueId"`
	ShardIndex    int          `json:"shardIndex"`
	Frame         int          `json:"frame"`
	PackIndex     int          `json:"packIndex"`
	PackID        string       `json:"packId"`
	DominantClass PhonemeClass `json:"dominantClass"`
}

type Assignment struct {
	Frame         int    `json:"frame"`
	Channel       int    `json:"channel"`
	DialogueID    string `json:"dialogueId"`
	ShardIndex    int    `json:"shardIndex"`
	PackIndex     int    `json:"packIndex"`
	PackID        string `json:"packId"`
	OriginalStart int    `json:"originalStart"`
	OriginalEnd   int    `json:"originalEnd"`
	ActiveFrom    int    `json:"activeFrom"`
	ActiveTo      int    `json:"activeTo"`
	DelayFrames   int    `json:"delayFrames"`
}

type DeferralRecord struct {
	DialogueID        string `json:"dialogueId"`
	ShardIndex        int    `json:"shardIndex"`
	PackIndex         int    `json:"packIndex"`
	PackID            string `json:"packId"`
	Frame             int    `json:"frame"`
	NextEligibleFrame int    `json:"nextEligibleFrame"`
	Reason            string `json:"reason"`
}

type RejectionRecord struct {
	DialogueID string `json:"dialogueId"`
	ShardIndex int    `json:"shardIndex"`
	PackIndex  int    `json:"packIndex"`
	PackID     string `json:"packId"`
	LastFrame  int    `json:"lastFrame"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail"`
}

type ChannelState struct {
	Channel          int      `json:"channel"`
	PackIDs          []string `json:"packIds"`
	DeliveredPackIDs []string `json:"deliveredPackIds"`
}

type FrameDispatch struct {
	Frame             int               `json:"frame"`
	BudgetLimit       int               `json:"budgetLimit"`
	BudgetUsed        int               `json:"budgetUsed"`
	BudgetOccupied    int               `json:"budgetOccupied"`
	ActivePackCount   int               `json:"activePackCount"`
	Channels          []ChannelState    `json:"channels"`
	Dispatched        []Assignment      `json:"dispatched"`
	DeferredThisFrame []DeferralRecord  `json:"deferredThisFrame,omitempty"`
	RejectedThisFrame []RejectionRecord `json:"rejectedThisFrame,omitempty"`
}

type DispatchResult struct {
	FrameCount   int               `json:"frameCount"`
	ChannelCount int               `json:"channelCount"`
	Frames       []FrameDispatch   `json:"frames"`
	Assignments  []Assignment      `json:"assignments"`
	Deferrals    []DeferralRecord  `json:"deferrals"`
	Rejections   []RejectionRecord `json:"rejections"`
}

type DialogueResult struct {
	Dialogue    *Dialogue          `json:"dialogue"`
	Quantized   []QuantizedPhoneme `json:"quantized"`
	Packs       []Pack             `json:"packs"`
	Attribution []FrameAttribution `json:"attribution"`
	Truncations []Truncation       `json:"truncations"`
}

type FullResult struct {
	ProjectID   string            `json:"projectId"`
	Recipe      Recipe            `json:"recipe"`
	Dialogues   []*DialogueResult `json:"dialogues"`
	Dispatch    *DispatchResult   `json:"dispatch"`
	Truncations []Truncation      `json:"truncations"`
	GeneratedAt time.Time         `json:"generatedAt,omitempty"`
}

type ShardCommitRecord struct {
	CommitSeq        int64  `json:"commitSeq"`
	RunID            string `json:"runId"`
	ProjectID        string `json:"projectId"`
	ShardIndex       int    `json:"shardIndex"`
	DialogueID       string `json:"dialogueId"`
	Revision         int    `json:"revision"`
	ContentHash      string `json:"contentHash"`
	RecipeVersion    string `json:"recipeVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
	PackCount        int    `json:"packCount"`
	FrameCount       int    `json:"frameCount"`
	CommittedAt      string `json:"committedAt"`
}

type QueueEvent struct {
	Time         time.Time     `json:"time"`
	ShardIndex   int           `json:"shardIndex"`
	DialogueID   string        `json:"dialogueId"`
	Reason       string        `json:"reason"`
	Waited       time.Duration `json:"waited"`
	WaitedMillis int64         `json:"waitedMillis"`
	Parallelism  int           `json:"parallelism"`
	BufferUsed   int           `json:"bufferUsed"`
	QueueBudget  int           `json:"queueBudget"`
}
