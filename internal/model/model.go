package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
)

const (
	SchemaVersion     = "1"
	RecordVersion     = "proof-v1"
	AlgorithmVersion  = "hdr2sdr-2026.11.1"
	EncodingLinearF32 = "linear_float32_le"
	EncodingRGB10LE   = "rgb10le"
)

type Chromaticity struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Primaries struct {
	Red   Chromaticity `json:"red"`
	Green Chromaticity `json:"green"`
	Blue  Chromaticity `json:"blue"`
	White Chromaticity `json:"white"`
}

type ColorSpace struct {
	Name      string    `json:"name"`
	Primaries Primaries `json:"primaries"`
	Transfer  string    `json:"transfer"`
}

type Route struct {
	ID                string     `json:"id"`
	Width             int        `json:"width"`
	Height            int        `json:"height"`
	Channels          int        `json:"channels"`
	Encoding          string     `json:"encoding"`
	PeakNits          float64    `json:"peak_nits"`
	FPS               float64    `json:"fps"`
	FrameCount        int        `json:"frame_count"`
	SegmentDurationMS int64      `json:"segment_duration_ms"`
	TimestampsMS      []int64    `json:"timestamps_ms"`
	ColorSpace        ColorSpace `json:"color_space"`
	FrameFile         string     `json:"frame_file"`
}

type Manifest struct {
	SchemaVersion string  `json:"schema_version"`
	Seed          int64   `json:"seed"`
	Routes        []Route `json:"routes"`
}

type PatchTruth struct {
	Name              string     `json:"name"`
	X                 int        `json:"x"`
	Y                 int        `json:"y"`
	Width             int        `json:"width"`
	Height            int        `json:"height"`
	ExpectedLinearRGB [3]float64 `json:"expected_linear_rgb"`
}

type FrameTruth struct {
	FrameIndex    int            `json:"frame_index"`
	TimestampMS   int64          `json:"timestamp_ms"`
	PeakNits      float64        `json:"peak_nits"`
	GamutBoundary []Chromaticity `json:"gamut_boundary"`
	Patches       []PatchTruth   `json:"patches"`
}

type RouteTruth struct {
	RouteID     string       `json:"route_id"`
	Fingerprint string       `json:"fingerprint"`
	Frames      []FrameTruth `json:"frames"`
}

type Truth struct {
	SchemaVersion string       `json:"schema_version"`
	Seed          int64        `json:"seed"`
	Fingerprint   string       `json:"fingerprint"`
	Routes        []RouteTruth `json:"routes"`
}

type Params struct {
	TargetPeakNits float64 `json:"target_peak_nits"`
	ExposureEV     float64 `json:"exposure_ev"`
	ToneMap        string  `json:"tone_map"`
	GamutMapping   string  `json:"gamut_mapping"`
	OutputBits     int     `json:"output_bits"`
}

func DefaultParams() Params {
	return Params{
		TargetPeakNits: 100,
		ExposureEV:     0,
		ToneMap:        "aces",
		GamutMapping:   "desaturate",
		OutputBits:     8,
	}
}

type InputDeclaration struct {
	Width             int        `json:"width"`
	Height            int        `json:"height"`
	Channels          int        `json:"channels"`
	Encoding          string     `json:"encoding"`
	PeakNits          float64    `json:"peak_nits"`
	FPS               float64    `json:"fps"`
	ColorSpace        ColorSpace `json:"color_space"`
	TimestampMS       int64      `json:"timestamp_ms"`
	SourceFrameSHA256 string     `json:"source_frame_sha256"`
}

type Stats struct {
	Width       int        `json:"width"`
	Height      int        `json:"height"`
	Channels    int        `json:"channels"`
	MinLinear   [3]float64 `json:"min_linear"`
	MaxLinear   [3]float64 `json:"max_linear"`
	MeanLinear  [3]float64 `json:"mean_linear"`
	PeakNits    float64    `json:"peak_nits"`
	PixelSHA256 string     `json:"pixel_sha256"`
}

type PatchProof struct {
	Name              string     `json:"name"`
	X                 int        `json:"x"`
	Y                 int        `json:"y"`
	Width             int        `json:"width"`
	Height            int        `json:"height"`
	ExpectedLinearRGB [3]float64 `json:"expected_linear_rgb"`
	ActualLinearRGB   [3]float64 `json:"actual_linear_rgb"`
	DeltaRGB          [3]float64 `json:"delta_rgb"`
	DeltaMax          float64    `json:"delta_max"`
}

type Proof struct {
	RecordVersion    string           `json:"record_version"`
	AlgorithmVersion string           `json:"algorithm_version"`
	ParamVersion     string           `json:"param_version"`
	RouteID          string           `json:"route_id"`
	FrameIndex       int              `json:"frame_index"`
	Segment          int              `json:"segment"`
	TimestampMS      int64            `json:"timestamp_ms"`
	Input            InputDeclaration `json:"input"`
	Params           Params           `json:"params"`
	ChainSummary     string           `json:"chain_summary"`
	OutputStats      Stats            `json:"output_stats"`
	Patches          []PatchProof     `json:"patches"`
	TruthFingerprint string           `json:"truth_fingerprint"`
	RecordSHA256     string           `json:"record_sha256"`
}

type Constraints struct {
	TargetPeakMin float64 `json:"target_peak_min"`
	TargetPeakMax float64 `json:"target_peak_max"`
	ExposureMin   float64 `json:"exposure_min"`
	ExposureMax   float64 `json:"exposure_max"`
}

var DefaultConstraints = Constraints{
	TargetPeakMin: 50,
	TargetPeakMax: 300,
	ExposureMin:   -4,
	ExposureMax:   4,
}

type BadFrame struct {
	RouteID    string `json:"route_id"`
	Segment    int    `json:"segment"`
	FrameIndex int    `json:"frame_index"`
	Field      string `json:"field"`
}

type MetaConflict struct {
	RouteID string      `json:"route_id"`
	Segment int         `json:"segment"`
	Field   string      `json:"field"`
	Value   interface{} `json:"value"`
}

type ParameterInjection struct {
	RouteID string      `json:"route_id"`
	Segment int         `json:"segment"`
	Field   string      `json:"field"`
	Value   interface{} `json:"value"`
}

type SegmentFailure struct {
	RouteID string `json:"route_id"`
	Segment int    `json:"segment"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

type Injections struct {
	MetaConflicts       []MetaConflict       `json:"meta_conflicts"`
	BadFrames           []BadFrame           `json:"bad_frames"`
	ParameterInjections []ParameterInjection `json:"parameter_injections"`
	SegmentFailures     []SegmentFailure     `json:"segment_failures"`
}

type RawFrame struct {
	Index       int
	TimestampMS int64
	Payload     []byte
}

type DecodedFrame struct {
	RawFrame
	LinearRGB []float64
}

type RouteData struct {
	Route         Route
	Truth         RouteTruth
	Frames        []RawFrame
	DecodedFrames []DecodedFrame
}

type Dataset struct {
	Dir              string
	ManifestPath     string
	TruthPath        string
	Manifest         Manifest
	Truth            Truth
	Routes           []*RouteData
	TruthFingerprint string
}

type FrameResult struct {
	RouteID   string
	Frame     int
	Segment   int
	Pixels    []byte
	Proof     Proof
	ProofJSON []byte
}

type ParamsVersioned struct {
	Params  Params `json:"params"`
	Version string `json:"version"`
}

func canonical(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func HashJSON(v interface{}) string {
	return SHA256Hex(canonical(v))
}

func ParamVersion(p Params) string {
	return HashJSON(p)
}

func ComputeRecordHash(p Proof) string {
	p.RecordSHA256 = ""
	return HashJSON(p)
}

func SegmentCount(r Route) int {
	if r.SegmentDurationMS <= 0 {
		return 0
	}
	last := int64(-1)
	if len(r.TimestampsMS) > 0 {
		last = r.TimestampsMS[len(r.TimestampsMS)-1]
	}
	if last < 0 {
		return 0
	}
	return int(last/r.SegmentDurationMS) + 1
}

func SegmentOf(r Route, timestampMS int64) int {
	if r.SegmentDurationMS <= 0 {
		return -1
	}
	return int(timestampMS / r.SegmentDurationMS)
}

func FrameIndicesForSegment(r Route, segment int) []int {
	var out []int
	for i, ts := range r.TimestampsMS {
		if SegmentOf(r, ts) == segment {
			out = append(out, i)
		}
	}
	return out
}

func CanonicalMap(v interface{}) (map[string]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func SortedRouteIDs(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func Finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func Clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func GamutBoundary(p Primaries) []Chromaticity {
	return []Chromaticity{p.Red, p.Green, p.Blue}
}
