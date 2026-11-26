package dialogue

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const (
	// InternalTimeUnit is an integer sample point. No internal duration is a float.
	InternalTimeUnit = "sample"
	// InternalScale is the internal fixed-point scale for gains and loudness (1/10000).
	InternalScale int64 = 10000

	AlgorithmVersion      = "dialogue-envelope-v1"
	QuantizationVersion   = "fixed-q4-v1"
	CheckpointFormat      = "dialogue-checkpoint-v1"
	DefaultSceneID        = "scene-default"
	DefaultSceneName      = "Default scene"
	DefaultF0Source       = "schema.v1.defaultF0Breakpoints"
	DefaultSceneSource    = "schema.v1.defaultSceneGrouping"
	DefaultF0MilliHz      = int64(110000)
	DefaultFrameTolerance = int64(2)
)

// TruthTolerance is the declared comparison tolerance for a source schema.
// A per-mille truth value cannot represent the internal per-myriad unit more
// tightly than half a per-mille (5 internal units).
func TruthTolerance(sourceVersion string) int64 {
	if sourceVersion == Version1 {
		return 5
	}
	return DefaultFrameTolerance
}

type PhonemeKind string

const (
	PhonemeVowel     PhonemeKind = "vowel"
	PhonemeConsonant PhonemeKind = "consonant"
	PhonemeSilence   PhonemeKind = "silence"
)

func (k PhonemeKind) Valid() bool {
	return k == PhonemeVowel || k == PhonemeConsonant || k == PhonemeSilence
}

type F0Point struct {
	AtSample   int64 `json:"at_sample"`
	HertzMilli int64 `json:"hertz_milli"`
}

type Phoneme struct {
	ID            string      `json:"id"`
	StartSample   int64       `json:"start_sample"`
	EndSample     int64       `json:"end_sample"`
	Kind          PhonemeKind `json:"kind"`
	Weight        int64       `json:"weight"` // internal per-myriad
	BaseF0MilliHz int64       `json:"base_f0_milli_hz"`
	F0Points      []F0Point   `json:"f0_points,omitempty"`
}

type Segment struct {
	ID          string    `json:"id"`
	SceneID     string    `json:"scene_id"`
	StartSample int64     `json:"start_sample"`
	EndSample   int64     `json:"end_sample"`
	SessionGain int64     `json:"session_gain"` // internal per-myriad
	Phonemes    []Phoneme `json:"phonemes"`
}

func (s Segment) Length() int64 {
	return s.EndSample - s.StartSample
}

type Scene struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Segments []Segment `json:"segments"`
}

type FrameTruth struct {
	SegmentID          string `json:"segment_id"`
	FrameIndex         int    `json:"frame_index"`
	StartSample        int64  `json:"start_sample"`
	EndSample          int64  `json:"end_sample"`
	Energy             int64  `json:"energy"`
	PeakPosition       int    `json:"peak_position"`
	ExpectedEnvelope   int64  `json:"expected_envelope"`
	CalibratedEnvelope int64  `json:"calibrated_envelope"`
}

type SegmentTruth struct {
	SegmentID      string  `json:"segment_id"`
	GainAdjustment int64   `json:"gain_adjustment"`
	Calibrated     []int64 `json:"calibrated_envelopes"`
	Reached        bool    `json:"reached"`
}

type Truth struct {
	Frames   []FrameTruth   `json:"frames"`
	Segments []SegmentTruth `json:"segments"`
}

type SourceInfo struct {
	SchemaVersion       string `json:"schema_version"`
	NativeTimeUnit      string `json:"native_time_unit"`
	NativeGainScale     int64  `json:"native_gain_scale"`
	NativeLoudnessScale int64  `json:"native_loudness_scale"`
}

// Project is the lossless internal model. All durations are samples and all
// gains/loudness values are per-myriad fixed-point integers.
type Project struct {
	ID                   string     `json:"id"`
	SampleRate           int64      `json:"sample_rate"`
	DeclaredTotalSamples int64      `json:"declared_total_samples"`
	TargetLoudness       int64      `json:"target_loudness"`
	GainLimit            int64      `json:"gain_limit"`
	GainMin              int64      `json:"gain_minimum"`
	GainMax              int64      `json:"gain_maximum"`
	FrameLength          int64      `json:"frame_length"`
	Hop                  int64      `json:"hop"`
	PCM                  []int16    `json:"pcm"`
	Scenes               []Scene    `json:"scenes"`
	Truth                *Truth     `json:"truth,omitempty"`
	Source               SourceInfo `json:"source"`
}

func (p *Project) Segments() []Segment {
	if p == nil {
		return nil
	}
	var out []Segment
	for _, scene := range p.Scenes {
		out = append(out, scene.Segments...)
	}
	return out
}

func (p *Project) SegmentByID(id string) (Segment, bool) {
	for _, segment := range p.Segments() {
		if segment.ID == id {
			return segment, true
		}
	}
	return Segment{}, false
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	out := *p
	out.PCM = append([]int16(nil), p.PCM...)
	out.Scenes = make([]Scene, len(p.Scenes))
	for i, scene := range p.Scenes {
		out.Scenes[i] = scene
		out.Scenes[i].Segments = make([]Segment, len(scene.Segments))
		for j, segment := range scene.Segments {
			out.Scenes[i].Segments[j] = cloneSegment(segment)
		}
	}
	if p.Truth != nil {
		truth := *p.Truth
		truth.Frames = append([]FrameTruth(nil), p.Truth.Frames...)
		truth.Segments = make([]SegmentTruth, len(p.Truth.Segments))
		for i, segment := range p.Truth.Segments {
			truth.Segments[i] = segment
			truth.Segments[i].Calibrated = append([]int64(nil), segment.Calibrated...)
		}
		out.Truth = &truth
	}
	return &out
}

func cloneSegment(segment Segment) Segment {
	out := segment
	out.Phonemes = make([]Phoneme, len(segment.Phonemes))
	for i, phoneme := range segment.Phonemes {
		out.Phonemes[i] = phoneme
		out.Phonemes[i].F0Points = append([]F0Point(nil), phoneme.F0Points...)
	}
	return out
}

// SignalHash covers the parts of a segment that affect envelope measurement.
// Session gain is intentionally excluded; gain only affects calibration.
func (p *Project) SignalHash(segment Segment) string {
	h := sha256.New()
	writeString(h, "signal-v1")
	writeInt(h, p.SampleRate)
	writeInt(h, p.FrameLength)
	writeInt(h, p.Hop)
	writeInt(h, segment.StartSample)
	writeInt(h, segment.EndSample)
	for _, phoneme := range segment.Phonemes {
		writeString(h, phoneme.ID)
		writeInt(h, phoneme.StartSample)
		writeInt(h, phoneme.EndSample)
		writeString(h, string(phoneme.Kind))
		writeInt(h, phoneme.Weight)
	}
	start := max64(0, segment.StartSample)
	end := min64(int64(len(p.PCM)), segment.EndSample)
	for i := start; i < end; i++ {
		var buf [2]byte
		binary.LittleEndian.PutUint16(buf[:], uint16(p.PCM[i]))
		_, _ = h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (p *Project) ContentHash(segment Segment) string {
	h := sha256.New()
	writeString(h, p.SignalHash(segment))
	writeInt(h, segment.SessionGain)
	writeInt(h, p.TargetLoudness)
	writeInt(h, p.GainLimit)
	writeString(h, QuantizationVersion)
	writeString(h, AlgorithmVersion)
	return hex.EncodeToString(h.Sum(nil))
}

func writeString(h interface{ Write([]byte) (int, error) }, value string) {
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(value))
}

func writeInt(h interface{ Write([]byte) (int, error) }, value int64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(value))
	_, _ = h.Write(buf[:])
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func validateUniqueIDs(p *Project) error {
	seen := map[string]bool{}
	for _, segment := range p.Segments() {
		if segment.ID == "" {
			return newError(CodeSchema, p.ID, "segments[].id", "segment id is empty")
		}
		if seen[segment.ID] {
			return newError(CodeSchema, p.ID, "segments."+segment.ID, "duplicate segment id")
		}
		seen[segment.ID] = true
	}
	return nil
}

func (p *Project) String() string {
	return fmt.Sprintf("Project(%s, scenes=%d, segments=%d, samples=%d)", p.ID, len(p.Scenes), len(p.Segments()), len(p.PCM))
}
