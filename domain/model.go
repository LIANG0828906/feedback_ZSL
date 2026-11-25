package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

type Rounding string

const (
	RoundFloor   Rounding = "floor"
	RoundCeil    Rounding = "ceil"
	RoundNearest Rounding = "nearest"
)

func (r Rounding) Valid() bool {
	return r == RoundFloor || r == RoundCeil || r == RoundNearest
}

type Category string

const (
	CategoryVowel     Category = "vowel"
	CategoryConsonant Category = "consonant"
	CategorySilence   Category = "silence"
)

func (c Category) Valid() bool {
	return c == CategoryVowel || c == CategoryConsonant || c == CategorySilence
}

type PCMInfo struct {
	File       string `json:"file"`
	Encoding   string `json:"encoding"`
	SampleRate int64  `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Samples    int64  `json:"samples"`
	SHA256     string `json:"sha256"`
}

type RenderConfig struct {
	TargetFPS             int64    `json:"target_fps"`
	Rounding              Rounding `json:"rounding"`
	FrameBudget           int      `json:"frame_budget"`
	ChannelSlots          int      `json:"channel_slots"`
	MinVisibleFrames      int64    `json:"min_visible_frames"`
	MaxPhonemesPerPackage int      `json:"max_phonemes_per_package"`
	AffinityThreshold     float64  `json:"affinity_threshold"`
	MaxDelayFrames        int64    `json:"max_delay_frames"`
	SamplesPerFrame       int64    `json:"samples_per_frame"`
}

type Phoneme struct {
	ID              string   `json:"id"`
	StartFrame      int64    `json:"start_frame"`
	EndFrame        int64    `json:"end_frame"`
	Category        Category `json:"category"`
	CategoryCode    string   `json:"category_code,omitempty"`
	WeightPermyriad int64    `json:"weight_permyriad"`
	Affinity        float64  `json:"affinity"`
	AffinityPresent bool     `json:"affinity_present"`

	SourceBasis       string `json:"source_basis,omitempty"`
	SourceStart       int64  `json:"source_start,omitempty"`
	SourceEnd         int64  `json:"source_end,omitempty"`
	SourceWeight      int64  `json:"source_weight,omitempty"`
	SourceWeightScale int64  `json:"source_weight_scale,omitempty"`
	SampleStart       int64  `json:"sample_start,omitempty"`
	SampleEnd         int64  `json:"sample_end,omitempty"`
}

type Dialogue struct {
	ID         string    `json:"id"`
	ShardIndex int       `json:"shard_index"`
	Speaker    string    `json:"speaker"`
	Channel    int       `json:"channel"`
	PCM        PCMInfo   `json:"pcm"`
	Phonemes   []Phoneme `json:"phonemes"`
}

// Trace documents a field-level conversion performed while loading a schema.
type Trace struct {
	Path          string `json:"path"`
	SourceVersion string `json:"source_version"`
	From          string `json:"from"`
	To            string `json:"to"`
	Rule          string `json:"rule"`
	DefaultSource string `json:"default_source,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	Remainder     int64  `json:"remainder,omitempty"`
	Divisor       int64  `json:"divisor,omitempty"`
}

// DefaultFill is emitted specifically when an older schema omits a field and
// the loader fills it from a declared schema default.
type DefaultFill struct {
	Path          string `json:"path"`
	OldValue      string `json:"old_value"`
	DefaultValue  string `json:"default_value"`
	DefaultSource string `json:"default_source"`
}

type CategoryGroup struct {
	Name  string   `json:"name"`
	Codes []string `json:"codes"`
}

type Project struct {
	ID               string          `json:"project_id"`
	SourceVersion    string          `json:"source_version"`
	TimeBasis        string          `json:"time_basis"`
	WeightScale      int64           `json:"weight_scale"`
	CategoryEncoding string          `json:"category_encoding"`
	CategoryGroups   []CategoryGroup `json:"category_groups,omitempty"`
	Render           RenderConfig    `json:"render"`
	Dialogues        []Dialogue      `json:"dialogues"`
	Traces           []Trace         `json:"traces"`
	Defaults         []DefaultFill   `json:"defaults"`
	TruthFile        string          `json:"truth_file,omitempty"`
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	var cp Project
	_ = json.Unmarshal(b, &cp)
	return &cp
}

func CanonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func Hash(v any) (string, error) {
	b, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type shardDigest struct {
	ProjectID  string          `json:"project_id"`
	ID         string          `json:"dialogue_id"`
	ShardIndex int             `json:"shard_index"`
	Speaker    string          `json:"speaker"`
	Channel    int             `json:"channel"`
	PCM        PCMInfo         `json:"pcm"`
	Phonemes   []phonemeDigest `json:"phonemes"`
}

type phonemeDigest struct {
	ID              string   `json:"id"`
	StartFrame      int64    `json:"start_frame"`
	EndFrame        int64    `json:"end_frame"`
	Category        Category `json:"category"`
	CategoryCode    string   `json:"category_code"`
	WeightPermyriad int64    `json:"weight_permyriad"`
	Affinity        float64  `json:"affinity"`
	SampleStart     int64    `json:"sample_start"`
	SampleEnd       int64    `json:"sample_end"`
}

// DialogueContentHash intentionally excludes schema version and source field
// names. It covers normalized audio metadata and every phoneme property that
// can affect lip-sync output.
func DialogueContentHash(projectID string, d Dialogue) (string, error) {
	phones := append([]Phoneme(nil), d.Phonemes...)
	sort.Slice(phones, func(i, j int) bool {
		if phones[i].ID == phones[j].ID {
			return phones[i].StartFrame < phones[j].StartFrame
		}
		return phones[i].ID < phones[j].ID
	})
	normalized := make([]phonemeDigest, len(phones))
	for i, ph := range phones {
		normalized[i] = phonemeDigest{
			ID: ph.ID, StartFrame: ph.StartFrame, EndFrame: ph.EndFrame,
			Category: ph.Category, CategoryCode: ph.CategoryCode,
			WeightPermyriad: ph.WeightPermyriad, Affinity: ph.Affinity,
			SampleStart: ph.SampleStart, SampleEnd: ph.SampleEnd,
		}
	}
	return Hash(shardDigest{
		ProjectID: projectID, ID: d.ID, ShardIndex: d.ShardIndex,
		Speaker: d.Speaker, Channel: d.Channel, PCM: d.PCM, Phonemes: normalized,
	})
}

// QuantizationVersion names the full deterministic rendering parameter set,
// including scheduling limits. A recovery run therefore cannot reuse a shard
// produced with a different budget, slot count, visibility window, affinity
// threshold, retry horizon, or samples-per-frame declaration.
func QuantizationVersion(rate int64, cfg RenderConfig) string {
	return fmt.Sprintf(
		"q1:rate=%d:fps=%d:round=%s:spf=%d:budget=%d:slots=%d:minvis=%d:maxphones=%d:aff=%s:delay=%d",
		rate, cfg.TargetFPS, cfg.Rounding, cfg.SamplesPerFrame, cfg.FrameBudget,
		cfg.ChannelSlots, cfg.MinVisibleFrames, cfg.MaxPhonemesPerPackage,
		strconv.FormatFloat(cfg.AffinityThreshold, 'g', -1, 64), cfg.MaxDelayFrames,
	)
}

const AlgorithmVersion = "merge-v1/schedule-v1"

type Package struct {
	ID         string   `json:"id"`
	DialogueID string   `json:"dialogue_id"`
	ShardIndex int      `json:"shard_index"`
	Channel    int      `json:"channel"`
	StartFrame int64    `json:"start_frame"`
	EndFrame   int64    `json:"end_frame"`
	PhonemeIDs []string `json:"phoneme_ids"`
	Summary    string   `json:"summary"`
}

type ScheduledPackage struct {
	Package
	Status             string `json:"status"`
	DispatchStartFrame int64  `json:"dispatch_start_frame"`
	DispatchEndFrame   int64  `json:"dispatch_end_frame"`
}

type FrameRecord struct {
	Frame      int64    `json:"frame"`
	PackageIDs []string `json:"package_ids"`
}

type DispatchEvent struct {
	Frame      int64  `json:"frame"`
	PackageID  string `json:"package_id"`
	DialogueID string `json:"dialogue_id"`
	ShardIndex int    `json:"shard_index"`
	Channel    int    `json:"channel"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	RetryFrame int64  `json:"retry_frame,omitempty"`
}

type ScheduleResult struct {
	Packages []ScheduledPackage `json:"packages"`
	Events   []DispatchEvent    `json:"events"`
	Frames   []FrameRecord      `json:"frames"`
}

type ShardArtifact struct {
	ShardIndex       int                `json:"shard_index"`
	DialogueID       string             `json:"dialogue_id"`
	State            string             `json:"state"`
	SourceVersion    string             `json:"source_version"`
	ContentHash      string             `json:"content_hash"`
	QuantVersion     string             `json:"quantization_version"`
	AlgorithmVersion string             `json:"algorithm_version"`
	TruthFingerprint string             `json:"truth_fingerprint"`
	PackageSummary   string             `json:"package_summary"`
	Packages         []ScheduledPackage `json:"packages"`
	Frames           []FrameRecord      `json:"frames"`
	Events           []DispatchEvent    `json:"events"`
	CommitSeq        int64              `json:"commit_seq"`
}

type EditKind string

const (
	EditBoundary EditKind = "boundary"
	EditWeight   EditKind = "weight"
	EditFPS      EditKind = "fps"
)

type Edit struct {
	Kind          EditKind `json:"kind"`
	DialogueID    string   `json:"dialogue_id,omitempty"`
	ShardIndex    int      `json:"shard_index,omitempty"`
	PhonemeID     string   `json:"phoneme_id,omitempty"`
	NewStartFrame int64    `json:"new_start_frame,omitempty"`
	NewEndFrame   int64    `json:"new_end_frame,omitempty"`
	NewWeight     int64    `json:"new_weight_permyriad,omitempty"`
	NewFPS        int64    `json:"new_fps,omitempty"`
}

type Invalidation struct {
	Reason         string   `json:"reason"`
	Edit           Edit     `json:"edit"`
	PhonemeNodes   []string `json:"phoneme_nodes"`
	PackageNodes   []string `json:"package_nodes"`
	FrameNodes     []string `json:"frame_nodes"`
	PackageIDs     []string `json:"package_ids"`
	ReusedPackages []string `json:"reused_packages"`
}
