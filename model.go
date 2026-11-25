package lipsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"
)

const (
	SupportedMinFPS        = 1
	SupportedMaxFPS        = 240
	SupportedMinSampleRate = 1000
	SupportedMaxSampleRate = 384000
	DefaultAlgorithm       = "lipcal-go/1"
	DefaultQuantVersion    = "qint/v1"
)

// LipCategory is a renderer-facing viseme class. It is deliberately a small
// game-domain vocabulary rather than a phoneme identifier.
type LipCategory string

const (
	LipSilent LipCategory = "silent"
	LipClosed LipCategory = "closed"
	LipPress  LipCategory = "press"
	LipRound  LipCategory = "round"
	LipWide   LipCategory = "wide"
	LipSmile  LipCategory = "smile"
	LipOpen   LipCategory = "open"
)

var validLips = map[LipCategory]bool{
	LipSilent: true,
	LipClosed: true,
	LipPress:  true,
	LipRound:  true,
	LipWide:   true,
	LipSmile:  true,
	LipOpen:   true,
}

// IsValid reports whether the category belongs to the supported viseme set.
func (l LipCategory) IsValid() bool { return validLips[l] }

// lipFamily partitions visemes into the two continuity families used by the
// transition constraint. Crossing between these families is discontinuous and
// requires a bridge frame.
func lipFamily(l LipCategory) int {
	switch l {
	case LipSilent, LipClosed, LipPress:
		return 0
	case LipRound, LipWide, LipSmile, LipOpen:
		return 1
	default:
		return -1
	}
}

type Quantization struct {
	Direction string `json:"direction"`
	Version   string `json:"version"`
}

type Subtitle struct {
	Text    string `json:"text"`
	Weights []int  `json:"weights"`
}

type Phoneme struct {
	ID          string      `json:"id"`
	StartSample int64       `json:"start_sample"`
	EndSample   int64       `json:"end_sample"`
	Lip         LipCategory `json:"lip"`
}

type Segment struct {
	ID          string    `json:"id"`
	VoiceRef    string    `json:"voice_ref"`
	StartSample int64     `json:"start_sample"`
	EndSample   int64     `json:"end_sample"`
	Phonemes    []Phoneme `json:"phonemes"`
}

// DialogueProject is the complete, immutable input to calibration.
type DialogueProject struct {
	DialogueID             string       `json:"dialogue_id"`
	Segments               []Segment    `json:"segments"`
	Subtitle               Subtitle     `json:"subtitle"`
	FPS                    int          `json:"fps"`
	SampleRate             int          `json:"sample_rate"`
	Quantization           Quantization `json:"quantization"`
	MinReadableFrames      int          `json:"min_readable_frames"`
	LipTransitionTolerance int          `json:"lip_transition_tolerance"`
	AlgorithmVersion       string       `json:"algorithm_version"`
}

func (p DialogueProject) RuneCount() int { return utf8.RuneCountInString(p.Subtitle.Text) }

func (p DialogueProject) TotalSamples() int64 {
	if len(p.Segments) == 0 {
		return 0
	}
	var max int64
	for _, s := range p.Segments {
		if s.EndSample > max {
			max = s.EndSample
		}
	}
	return max
}

// ContentHash is the whole-project identity used by window checkpoints.
func ContentHash(p DialogueProject) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// FrameResult is both the truth record and the runtime output for one frame.
// Reused and CacheKey are diagnostics and are intentionally absent from truth
// JSON comparisons.
type FrameResult struct {
	Frame      int         `json:"frame"`
	CharStart  int         `json:"char_start"`
	CharEnd    int         `json:"char_end"`
	BaseLip    LipCategory `json:"base_lip"`
	Lip        LipCategory `json:"lip"`
	Transition bool        `json:"transition"`
	PhonemeID  string      `json:"phoneme_id"`
	SegmentID  string      `json:"segment_id"`
	Reused     bool        `json:"-"`
	CacheKey   string      `json:"-"`
}

type LipSwitch struct {
	Frame int         `json:"frame"`
	From  LipCategory `json:"from"`
	To    LipCategory `json:"to"`
}

type LipTransition struct {
	Frame          int           `json:"frame"`
	From           LipCategory   `json:"from"`
	To             LipCategory   `json:"to"`
	Inserted       []LipCategory `json:"inserted"`
	InsertedFrames []int         `json:"inserted_frames"`
	Distance       int           `json:"distance"`
}

// Truth is emitted by the deterministic generator's analytic constructor.
type Truth struct {
	DialogueID          string          `json:"dialogue_id"`
	FPS                 int             `json:"fps"`
	FrameCount          int             `json:"frame_count"`
	Frames              []FrameResult   `json:"frames"`
	LipSwitches         []LipSwitch     `json:"lip_switches"`
	Transitions         []LipTransition `json:"transitions"`
	ContentHash         string          `json:"content_hash"`
	QuantizationVersion string          `json:"quantization_version"`
	AlgorithmVersion    string          `json:"algorithm_version"`
}

type FrameTruth struct {
	Frame      int         `json:"frame"`
	CharStart  int         `json:"char_start"`
	CharEnd    int         `json:"char_end"`
	BaseLip    LipCategory `json:"base_lip"`
	Lip        LipCategory `json:"lip"`
	Transition bool        `json:"transition"`
	PhonemeID  string      `json:"phoneme_id"`
	SegmentID  string      `json:"segment_id"`
}

func frameTruth(f FrameResult) FrameTruth {
	return FrameTruth{
		Frame:      f.Frame,
		CharStart:  f.CharStart,
		CharEnd:    f.CharEnd,
		BaseLip:    f.BaseLip,
		Lip:        f.Lip,
		Transition: f.Transition,
		PhonemeID:  f.PhonemeID,
		SegmentID:  f.SegmentID,
	}
}
