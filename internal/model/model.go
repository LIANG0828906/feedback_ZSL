package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"qualitybench/internal/core"
)

type StepType string

const (
	StepNoise      StepType = "noise"
	StepBlur       StepType = "blur"
	StepBlockiness StepType = "blockiness"
	StepChroma     StepType = "chroma_shift"
	StepOverexpose StepType = "overexpose"
)

func (s StepType) Valid() bool {
	switch s {
	case StepNoise, StepBlur, StepBlockiness, StepChroma, StepOverexpose:
		return true
	default:
		return false
	}
}

func (s StepType) Category() string {
	return string(s)
}

type Step struct {
	Type     StepType   `json:"type"`
	Strength float64    `json:"strength"`
	Region   *core.Rect `json:"region,omitempty"`
}

func (s Step) RegionRect(width, height int) core.Rect {
	if s.Region == nil {
		return core.Rect{W: width, H: height}
	}
	return *s.Region
}

func (s Step) CanonicalParams() string {
	region := "full"
	if s.Region != nil {
		region = strconv.Itoa(s.Region.X) + "," + strconv.Itoa(s.Region.Y) + "," +
			strconv.Itoa(s.Region.W) + "," + strconv.Itoa(s.Region.H)
	}
	return string(s.Type) + "|" + strconv.FormatFloat(s.Strength, 'g', -1, 64) + "|" + region
}

type Chain struct {
	ID      string `json:"id"`
	ImageID string `json:"image_id"`
	Steps   []Step `json:"steps"`
}

func (c Chain) Clone() Chain {
	out := Chain{ID: c.ID, ImageID: c.ImageID, Steps: make([]Step, len(c.Steps))}
	for i, s := range c.Steps {
		out.Steps[i] = s
		if s.Region != nil {
			r := *s.Region
			out.Steps[i].Region = &r
		}
	}
	return out
}

type Shape struct {
	ID    string    `json:"id"`
	Kind  string    `json:"kind"`
	BBox  core.Rect `json:"bbox"`
	Color [3]uint8  `json:"color"`
}

type Detail struct {
	BBox   core.Rect `json:"bbox"`
	Period int       `json:"period"`
	Delta  int       `json:"delta"`
}

type ImageSpec struct {
	ID        string    `json:"id"`
	Frame     int       `json:"frame"`
	Seed      int64     `json:"seed"`
	Shapes    []Shape   `json:"shapes"`
	Watermark core.Rect `json:"watermark"`
	Details   []Detail  `json:"details"`
}

type Edit struct {
	Op       string     `json:"op"`
	ImageID  string     `json:"image_id,omitempty"`
	Step     int        `json:"step,omitempty"`
	Strength *float64   `json:"strength,omitempty"`
	Region   *core.Rect `json:"region,omitempty"`
	From     int        `json:"from,omitempty"`
	To       int        `json:"to,omitempty"`
	Version  string     `json:"version,omitempty"`
}

type Project struct {
	SchemaVersion    string      `json:"schema_version"`
	Seed             int64       `json:"seed"`
	Width            int         `json:"width"`
	Height           int         `json:"height"`
	Channels         int         `json:"channels"`
	BlockSize        int         `json:"block_size"`
	AttributionShare float64     `json:"attribution_share"`
	MetricTolerance  float64     `json:"metric_tolerance"`
	AlgorithmVersion string      `json:"algorithm_version"`
	Images           []ImageSpec `json:"images"`
	Chains           []Chain     `json:"chains"`
	Edits            []Edit      `json:"edits"`
}

func (p *Project) Image(id string) (ImageSpec, bool) {
	for _, image := range p.Images {
		if image.ID == id {
			return image, true
		}
	}
	return ImageSpec{}, false
}

func (p *Project) Chain(imageID string) (Chain, bool) {
	for _, c := range p.Chains {
		if c.ImageID == imageID {
			return c, true
		}
	}
	return Chain{}, false
}

type SampleTruth struct {
	X         float64  `json:"x"`
	Y         float64  `json:"y"`
	Expected  [3]uint8 `json:"expected"`
	Tolerance int      `json:"tolerance"`
	Label     string   `json:"label,omitempty"`
}

type MetricInterval struct {
	NoiseVariance [2]float64 `json:"noise_variance"`
	EdgeSharpness [2]float64 `json:"edge_sharpness"`
	ChromaShift   [2]float64 `json:"chroma_shift"`
}

type AttributionTruth struct {
	Step      int     `json:"step"`
	Category  string  `json:"category"`
	Share     float64 `json:"share"`
	Tolerance float64 `json:"tolerance"`
}

type BlockTruth struct {
	X           int              `json:"x"`
	Y           int              `json:"y"`
	W           int              `json:"w"`
	H           int              `json:"h"`
	Metrics     MetricInterval   `json:"metrics"`
	Attribution AttributionTruth `json:"attribution"`
}

type ImageTruth struct {
	ID             string        `json:"id"`
	Frame          int           `json:"frame"`
	StepCount      int           `json:"step_count"`
	Steps          []Step        `json:"steps"`
	BaselineSHA256 string        `json:"baseline_sha256"`
	Shapes         []Shape       `json:"shapes"`
	Watermark      core.Rect     `json:"watermark"`
	Samples        []SampleTruth `json:"samples"`
	Blocks         []BlockTruth  `json:"blocks"`
}

type Truth struct {
	SchemaVersion        string       `json:"schema_version"`
	ProjectFingerprint   string       `json:"project_fingerprint"`
	AlgorithmVersion     string       `json:"algorithm_version"`
	Seed                 int64        `json:"seed"`
	Width                int          `json:"width"`
	Height               int          `json:"height"`
	Channels             int          `json:"channels"`
	BlockSize            int          `json:"block_size"`
	AttributionShare     float64      `json:"attribution_share"`
	AttributionTolerance float64      `json:"attribution_tolerance"`
	Images               []ImageTruth `json:"images"`
}

func (t *Truth) Image(id string) (ImageTruth, bool) {
	for _, image := range t.Images {
		if image.ID == id {
			return image, true
		}
	}
	return ImageTruth{}, false
}

func CanonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func Fingerprint(v any) (string, error) {
	data, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func ChainFingerprint(c Chain, algorithmVersion string) string {
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(append([]byte(algorithmVersion+"\x00"), data...))
	return hex.EncodeToString(sum[:])
}

func SupportRadius(step Step) int {
	if step.Type != StepBlur || step.Strength <= 0 {
		return 0
	}
	r := 1 + int(math.Floor(step.Strength*2))
	if r > 3 {
		r = 3
	}
	return r
}

// InputSupportRadius declares how far outside an output block an operator may
// read. Blockiness needs the complete aligned 8x8 cell, whose centre can be up
// to seven pixels away from the block edge.
func InputSupportRadius(step Step) int {
	r := SupportRadius(step)
	if step.Type == StepBlockiness && step.Strength > 0 && r < 7 {
		r = 7
	}
	return r
}

func IsFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func Interval(actual, tolerance float64) [2]float64 {
	if tolerance < 0 {
		tolerance = 0
	}
	width := math.Abs(actual) * tolerance
	if width < 1e-9 {
		width = 1e-9
	}
	return [2]float64{math.Max(0, actual-width), actual + width}
}

func IntervalContains(iv [2]float64, v float64) bool {
	return v >= iv[0]-1e-9 && v <= iv[1]+1e-9
}

func StepLabel(index int, step Step) string {
	return "step[" + strconv.Itoa(index+1) + "]=" + string(step.Type) + "(" +
		strconv.FormatFloat(step.Strength, 'f', 3, 64) + ")"
}

func NormalizeCategory(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
