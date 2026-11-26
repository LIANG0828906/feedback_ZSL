package loudness

import (
	"fmt"
	"sort"
)

const (
	// InternalGainScale is linear gain Q12: 4096 == 1.0.
	InternalGainScale int64 = 1 << 12
	// InternalLoudnessScale is loudness Q8 dB: 256 == 1 dB.
	InternalLoudnessScale int64 = 1 << 8
	// InternalWeightScale is loudness weight Q8: 256 == 1.0.
	InternalWeightScale int64 = 1 << 8
	// InternalLinearScale is linear amplitude Q16: 65536 == 1.0.
	InternalLinearScale int64 = 1 << 16
)

const (
	MaxFrameLength int64 = 1 << 16
	MaxSampleRate  int64 = 384000
)

// Version identifies a supported on-disk game-audio schema.
type Version string

const (
	SchemaV1 Version = "game-audio/v1"
	SchemaV2 Version = "game-audio/v2"
	SchemaV3 Version = "game-audio/v3"
)

func (v Version) Valid() bool {
	return v == SchemaV1 || v == SchemaV2 || v == SchemaV3
}

func SupportedVersions() []Version {
	return []Version{SchemaV1, SchemaV2, SchemaV3}
}

// Category controls the category contribution to measured loudness.
type Category string

const (
	CategoryDialog  Category = "dialog"
	CategorySFX     Category = "sfx"
	CategoryMusic   Category = "music"
	CategoryAmbient Category = "ambient"
)

func (c Category) Valid() bool {
	switch c {
	case CategoryDialog, CategorySFX, CategoryMusic, CategoryAmbient:
		return true
	default:
		return false
	}
}

// CategoryFactorQ12 returns the deterministic category multiplier.
func CategoryFactorQ12(c Category) int64 {
	switch c {
	case CategoryDialog:
		return InternalGainScale
	case CategorySFX:
		return 3584 // 0.875
	case CategoryMusic:
		return 3072 // 0.75
	case CategoryAmbient:
		return 2048 // 0.5
	default:
		return 0
	}
}

// Rounding declares how division remainders are handled. RoundDown truncates
// toward zero; RoundHalfUp rounds magnitudes to the nearest integer.
type Rounding string

const (
	RoundDown   Rounding = "toward-zero"
	RoundHalfUp Rounding = "half-away-from-zero"
)

func (r Rounding) Valid() bool {
	return r == RoundDown || r == RoundHalfUp
}

// Truncation records one integer division or approximation point. Remainder is
// the signed remainder after Quotient; Residual is the scaled value not
// represented by the returned integer.
type Truncation struct {
	Path        string   `json:"path"`
	Operation   string   `json:"operation"`
	Numerator   int64    `json:"numerator"`
	Denominator int64    `json:"denominator"`
	Quotient    int64    `json:"quotient"`
	Remainder   int64    `json:"remainder"`
	Residual    int64    `json:"residual"`
	Rounding    Rounding `json:"rounding"`
}

// TraceKind identifies why a source field appeared in the internal model.
type TraceKind string

const (
	TraceMap       TraceKind = "map"
	TraceConvert   TraceKind = "convert"
	TraceDefault   TraceKind = "default"
	TraceStructure TraceKind = "structure"
)

// FieldTrace preserves source provenance for every normalized field.
type FieldTrace struct {
	SourceVersion Version     `json:"sourceVersion"`
	SourcePath    string      `json:"sourcePath"`
	InternalPath  string      `json:"internalPath"`
	Kind          TraceKind   `json:"kind"`
	OldValue      string      `json:"oldValue,omitempty"`
	NewValue      string      `json:"newValue,omitempty"`
	DefaultSource string      `json:"defaultSource,omitempty"`
	Truncation    *Truncation `json:"truncation,omitempty"`
}

// Provenance survives normalization and version conversion.
type Provenance struct {
	OriginalVersion Version      `json:"originalVersion"`
	CurrentVersion  Version      `json:"currentVersion"`
	Traces          []FieldTrace `json:"traces,omitempty"`
}

// Scene preserves scene grouping in versions that can express it.
type Scene struct {
	Name    string `json:"name"`
	ClipIDs []int  `json:"clipIds"`
}

// Clip is the unified internal representation. All time values are samples,
// gain is Q12, loudness is Q8 dB, and weight is Q8.
type Clip struct {
	ID                int      `json:"id"`
	Scene             string   `json:"scene,omitempty"`
	StartSample       int64    `json:"startSample"`
	EndSample         int64    `json:"endSample"`
	Category          Category `json:"category"`
	F0TrackHz         []int64  `json:"f0TrackHz"`
	AmplitudeQ12      int64    `json:"amplitudeQ12"`
	LoudnessWeightQ8  int64    `json:"loudnessWeightQ8"`
	GainQ12           int64    `json:"gainQ12"`
	CalibrationMarker *string  `json:"calibrationMarker,omitempty"`
}

func (c Clip) DurationSamples() int64 { return c.EndSample - c.StartSample }

// Project is the normalized, version-independent model.
type Project struct {
	FormatVersion       Version      `json:"formatVersion"`
	ID                  string       `json:"id"`
	Seed                int64        `json:"seed"`
	SampleRate          int64        `json:"sampleRate"`
	FrameLength         int64        `json:"frameLength"`
	FrameShift          int64        `json:"frameShift"`
	SessionGainQ12      int64        `json:"sessionGainQ12"`
	TargetLoudnessDBQ8  int64        `json:"targetLoudnessDbQ8"`
	MaxGainQ12          int64        `json:"maxGainQ12"`
	LoudnessToleranceQ8 int64        `json:"loudnessToleranceQ8"`
	Scenes              []Scene      `json:"scenes,omitempty"`
	Clips               []Clip       `json:"clips"`
	Provenance          Provenance   `json:"provenance"`
	Quantization        Quantization `json:"quantization"`
}

// Quantization versions the fixed-point interpretation used by cache keys.
type Quantization struct {
	Version       string `json:"version"`
	GainScale     int64  `json:"gainScale"`
	LoudnessScale int64  `json:"loudnessScale"`
	WeightScale   int64  `json:"weightScale"`
	LinearScale   int64  `json:"linearScale"`
}

func DefaultQuantization() Quantization {
	return Quantization{
		Version:       "fixed-v1",
		GainScale:     InternalGainScale,
		LoudnessScale: InternalLoudnessScale,
		WeightScale:   InternalWeightScale,
		LinearScale:   InternalLinearScale,
	}
}

func (p *Project) ClipByID(id int) (*Clip, bool) {
	for i := range p.Clips {
		if p.Clips[i].ID == id {
			return &p.Clips[i], true
		}
	}
	return nil, false
}

func (p *Project) ClipIDs() []int {
	ids := make([]int, 0, len(p.Clips))
	for _, clip := range p.Clips {
		ids = append(ids, clip.ID)
	}
	sort.Ints(ids)
	return ids
}

func (p *Project) TotalSamples() int64 {
	var total int64
	for _, clip := range p.Clips {
		total += clip.DurationSamples()
	}
	return total
}

// CloneProject performs a deep copy so edits never mutate caller-owned models.
func CloneProject(p *Project) *Project {
	if p == nil {
		return nil
	}
	clone := *p
	clone.Clips = make([]Clip, len(p.Clips))
	for i, clip := range p.Clips {
		clone.Clips[i] = clip
		clone.Clips[i].F0TrackHz = append([]int64(nil), clip.F0TrackHz...)
		if clip.CalibrationMarker != nil {
			marker := *clip.CalibrationMarker
			clone.Clips[i].CalibrationMarker = &marker
		}
	}
	clone.Scenes = make([]Scene, len(p.Scenes))
	for i, scene := range p.Scenes {
		clone.Scenes[i] = scene
		clone.Scenes[i].ClipIDs = append([]int(nil), scene.ClipIDs...)
	}
	clone.Provenance.Traces = append([]FieldTrace(nil), p.Provenance.Traces...)
	return &clone
}

func markerValue(marker *string) string {
	if marker == nil {
		return "<absent>"
	}
	return *marker
}

func validateProjectBasics(p *Project) error {
	if p == nil {
		return &DomainError{Code: ESchema, Path: "$", Detail: "project is nil"}
	}
	if !p.FormatVersion.Valid() {
		return &DomainError{Code: ESchema, Path: "$.formatVersion", SourceVersion: p.FormatVersion, Detail: "unknown schema version"}
	}
	if p.ID == "" {
		return &DomainError{Code: ESchema, Path: "$.id", SourceVersion: p.FormatVersion, Detail: "required field is empty"}
	}
	if p.SampleRate <= 0 || p.SampleRate > MaxSampleRate {
		return &DomainError{Code: ESchema, Path: "$.sampleRate", SourceVersion: p.FormatVersion, Detail: fmt.Sprintf("must be in [1,%d]", MaxSampleRate)}
	}
	if err := ValidateFrameParameters(p.FrameLength, p.FrameShift); err != nil {
		return err
	}
	if p.SessionGainQ12 <= 0 || p.MaxGainQ12 <= 0 {
		return &DomainError{Code: ESchema, Path: "$", SourceVersion: p.FormatVersion, Detail: "session gain and max gain must be positive"}
	}
	if p.LoudnessToleranceQ8 < 0 {
		return &DomainError{Code: ESchema, Path: "$.loudnessToleranceQ8", SourceVersion: p.FormatVersion, Detail: "must be non-negative"}
	}
	seen := make(map[int]struct{}, len(p.Clips))
	for i, clip := range p.Clips {
		path := fmt.Sprintf("$.clips[%d]", i)
		if _, exists := seen[clip.ID]; exists {
			return &DomainError{Code: ESchema, Path: path + ".id", SourceVersion: p.FormatVersion, Detail: "duplicate clip id"}
		}
		seen[clip.ID] = struct{}{}
		if clip.StartSample < 0 || clip.EndSample <= clip.StartSample {
			return &DomainError{Code: ESchema, Path: path + ".startSample", SourceVersion: p.FormatVersion, Detail: "clip interval must satisfy 0 <= start < end"}
		}
		if !clip.Category.Valid() {
			return &DomainError{Code: ESchema, Path: path + ".category", SourceVersion: p.FormatVersion, Detail: "unknown category"}
		}
		if len(clip.F0TrackHz) == 0 {
			return &DomainError{Code: ESchema, Path: path + ".f0TrackHz", SourceVersion: p.FormatVersion, Detail: "at least one F0 point is required"}
		}
		for j, frequency := range clip.F0TrackHz {
			if frequency <= 0 {
				return &DomainError{Code: ESchema, Path: fmt.Sprintf("%s.f0TrackHz[%d]", path, j), SourceVersion: p.FormatVersion, Detail: "frequency must be positive"}
			}
		}
		if clip.AmplitudeQ12 <= 0 || clip.LoudnessWeightQ8 <= 0 || clip.GainQ12 <= 0 {
			return &DomainError{Code: ESchema, Path: path, SourceVersion: p.FormatVersion, Detail: "amplitude, weight, and gain must be positive"}
		}
	}
	if len(p.Clips) == 0 {
		return &DomainError{Code: ESchema, Path: "$.clips", SourceVersion: p.FormatVersion, Detail: "at least one clip is required"}
	}
	return nil
}

// ValidateFrameParameters validates the declared legal frame interval.
func ValidateFrameParameters(frameLength, frameShift int64) error {
	if frameLength < 1 || frameLength > MaxFrameLength {
		return &DomainError{
			Code: EPara, Field: "frameLength", Path: "$.frameLength",
			LegalMin: 1, LegalMax: MaxFrameLength,
			Detail: fmt.Sprintf("frame length must be in [1,%d]", MaxFrameLength),
		}
	}
	if frameShift < 1 || frameShift > frameLength {
		return &DomainError{
			Code: EPara, Field: "frameShift", Path: "$.frameShift",
			LegalMin: 1, LegalMax: frameLength,
			Detail: fmt.Sprintf("frame shift must be in [1,%d]", frameLength),
		}
	}
	return nil
}
