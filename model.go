package dialoguecal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
)

type SchemaVersion int

const (
	VersionV1 SchemaVersion = 1
	VersionV2 SchemaVersion = 2
	VersionV3 SchemaVersion = 3
)

const (
	DefaultQuantizationVersion = "quant.half-up.v1"
	DefaultParallelVersion     = "parallel.budget.v1"
	DefaultConversionVersion   = "fixed.scale.v1"
	DefaultAlgorithmVersion    = "dialogue.kernel.v1"
)

func (v SchemaVersion) String() string {
	return fmt.Sprintf("v%d", int(v))
}

func ParseSchemaVersion(v int) (SchemaVersion, bool) {
	switch SchemaVersion(v) {
	case VersionV1, VersionV2, VersionV3:
		return SchemaVersion(v), true
	default:
		return 0, false
	}
}

// PCM is always mono signed 16-bit little-endian in the internal model.
// DeclaredSampleCount is retained so a mismatch can be reported at the exact
// source path instead of being corrected silently.
type PCM struct {
	Encoding            string
	SampleRate          int
	Channels            int
	Samples             []int16
	DeclaredSampleCount int64
}

type Phoneme struct {
	ID          string
	StartSample int64
	EndSample   int64
	Class       string
	Weight      int
}

type Segment struct {
	ID          string
	DialogueID  string
	StartSample int64
	EndSample   int64
	// Gain is expressed in 1/10000 dB. The internal scale is never floating
	// point and never changes between schema versions.
	Gain     int64
	Phonemes []Phoneme
	Markers  []CalibrationMarker
}

type Dialogue struct {
	ID       string
	PCM      PCM
	Segments []Segment
}

// SceneGroup is presentation/source organization only. It is retained so a
// downgrade can report E_LOSSY precisely; it is intentionally excluded from
// the audible semantic fingerprint.
type SceneGroup struct {
	ID          string
	DialogueIDs []string
}

type CalibrationMarker struct {
	Path      string
	SegmentID string
	Kind      string
	Value     string
}

type ConversionTrace struct {
	FieldPath      string
	Stage          string
	FromUnit       string
	ToUnit         string
	Rounding       string
	OperandA       int64
	OperandB       int64
	Result         int64
	Remainder      int64
	Residual       int64
	SourceVersion  SchemaVersion
	TargetVersion  SchemaVersion
	TruncationSite string
}

type DefaultTrace struct {
	FieldPath    string
	OldValue     string
	DefaultValue string
	Source       string
}

type FieldMapping struct {
	SourcePath    string
	InternalPath  string
	Form          string
	SourceVersion SchemaVersion
}

type LossyIssue struct {
	Code        Code   `json:"code"`
	FieldPath   string `json:"fieldPath"`
	Original    string `json:"original"`
	Disposition string `json:"disposition"`
}

type Project struct {
	ID string

	SourceVersion SchemaVersion
	FrameRateHz   int
	SampleRate    int

	// Loudness, gains, tolerance and the gain ceiling all use 1/10000 dB.
	TargetLoudness int64
	SessionGain    int64
	GainUpperBound int64
	Tolerance      int64

	FrameEnergyLimit    int64
	MaxParallelism      int
	WorkerBufferSamples int
	QueueBudget         int

	QuantizationVersion string
	ParallelVersion     string
	ConversionVersion   string
	AlgorithmVersion    string

	Dialogues          []Dialogue
	Organization       string
	SceneGroups        []SceneGroup
	CalibrationMarkers []CalibrationMarker

	Conversions []ConversionTrace
	Defaults    []DefaultTrace
	Mappings    []FieldMapping
	Lossy       []LossyIssue
}

type FrameTruth struct {
	DialogueID  string `json:"dialogueId"`
	SegmentID   string `json:"segmentId"`
	Frame       int    `json:"frame"`
	StartSample int64  `json:"startSample"`
	EndSample   int64  `json:"endSample"`

	Energy             int64 `json:"energy"`
	ZeroCrossings      int   `json:"zeroCrossings"`
	TransientPeak      int   `json:"transientPeak"`
	Weight             int   `json:"weight"`
	BaseLoudness       int64 `json:"baseLoudness"`
	RequestedGain      int64 `json:"requestedGain"`
	AppliedGain        int64 `json:"appliedGain"`
	CalibratedLoudness int64 `json:"calibratedLoudness"`
}

type GeneratedProject struct {
	Project *Project
	Truth   []FrameTruth
	Docs    map[SchemaVersion][]byte
}

// Clone returns a fully independent copy. Persisted state and edits must never
// mutate an input project through an aliased slice.
func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Dialogues = make([]Dialogue, len(p.Dialogues))
	for i, d := range p.Dialogues {
		dc := d
		dc.PCM.Samples = append([]int16(nil), d.PCM.Samples...)
		dc.Segments = make([]Segment, len(d.Segments))
		for j, s := range d.Segments {
			sc := s
			sc.Phonemes = append([]Phoneme(nil), s.Phonemes...)
			sc.Markers = append([]CalibrationMarker(nil), s.Markers...)
			dc.Segments[j] = sc
		}
		cp.Dialogues[i] = dc
	}
	cp.CalibrationMarkers = append([]CalibrationMarker(nil), p.CalibrationMarkers...)
	cp.SceneGroups = make([]SceneGroup, len(p.SceneGroups))
	for i, g := range p.SceneGroups {
		cp.SceneGroups[i] = SceneGroup{ID: g.ID, DialogueIDs: append([]string(nil), g.DialogueIDs...)}
	}
	cp.Conversions = append([]ConversionTrace(nil), p.Conversions...)
	cp.Defaults = append([]DefaultTrace(nil), p.Defaults...)
	cp.Mappings = append([]FieldMapping(nil), p.Mappings...)
	cp.Lossy = append([]LossyIssue(nil), p.Lossy...)
	return &cp
}

// Sort canonicalizes only ordering, not IDs. All processing sorts explicitly
// so worker completion order can never affect observations.
func (p *Project) Sort() {
	sort.Slice(p.Dialogues, func(i, j int) bool { return p.Dialogues[i].ID < p.Dialogues[j].ID })
	for i := range p.Dialogues {
		d := &p.Dialogues[i]
		sort.Slice(d.Segments, func(a, b int) bool {
			return d.Segments[a].StartSample < d.Segments[b].StartSample
		})
		for j := range d.Segments {
			s := &d.Segments[j]
			sort.Slice(s.Phonemes, func(a, b int) bool {
				return s.Phonemes[a].StartSample < s.Phonemes[b].StartSample
			})
		}
	}
}

func PCMSHA256(p PCM) string {
	h := sha256.New()
	var b [2]byte
	for _, s := range p.Samples {
		binary.LittleEndian.PutUint16(b[:], uint16(s))
		_, _ = h.Write(b[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func HashStrings(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
