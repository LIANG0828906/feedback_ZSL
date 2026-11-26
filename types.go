package lightprobe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
)

const (
	AlgorithmVersion = "lp-recon-v1.0.0"
	DefaultSeed      = int64(0x5EED_42A8_691E)
)

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3    { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3    { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Mul(s float64) Vec3 { return Vec3{v.X * s, v.Y * s, v.Z * s} }

type Bounds struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

func (b Bounds) Contains(p Vec3) bool {
	const eps = 1e-9
	return p.X >= b.Min.X-eps && p.X <= b.Max.X+eps &&
		p.Y >= b.Min.Y-eps && p.Y <= b.Max.Y+eps &&
		p.Z >= b.Min.Z-eps && p.Z <= b.Max.Z+eps
}

func (b Bounds) Size() Vec3 {
	return Vec3{b.Max.X - b.Min.X, b.Max.Y - b.Min.Y, b.Max.Z - b.Min.Z}
}

type Params struct {
	FixedScale   int64   `json:"fixed_scale"`
	WeightScale  int64   `json:"weight_scale"`
	BlendScale   int64   `json:"blend_scale"`
	SHMinFixed   int64   `json:"sh_min_fixed"`
	SHMaxFixed   int64   `json:"sh_max_fixed"`
	SampleLambda int     `json:"sample_lambda"`
	BlendWidth   float64 `json:"blend_width"`
	Tolerance    float64 `json:"tolerance"`
	SHCondition  float64 `json:"sh_condition"`
	ParamVersion string  `json:"param_version"`
	Workers      int     `json:"workers"`
	BufferLimit  int     `json:"buffer_limit"`
	QueueBudget  int     `json:"queue_budget"`
}

func DefaultParams() Params {
	scale := int64(1_000_000)
	return Params{
		FixedScale:   scale,
		WeightScale:  1_000_000,
		BlendScale:   1_000_000,
		SHMinFixed:   -4 * scale,
		SHMaxFixed:   4 * scale,
		SampleLambda: 8,
		BlendWidth:   1.0,
		Tolerance:    0.01,
		SHCondition:  48.0,
		ParamVersion: "params-standard-v1",
		Workers:      4,
		BufferLimit:  8,
		QueueBudget:  16,
	}
}

func (p Params) Validate() error {
	if p.FixedScale <= 0 {
		return domainError(EVal, "params", "fixed_scale", "must be positive")
	}
	if p.WeightScale <= 0 {
		return domainError(EVal, "params", "weight_scale", "must be positive")
	}
	if p.BlendScale <= 0 {
		return domainError(EVal, "params", "blend_scale", "must be positive")
	}
	if p.SHMinFixed > p.SHMaxFixed {
		return domainError(EVal, "params", "sh_range", "min must not exceed max")
	}
	if p.SampleLambda < 0 {
		return domainError(EVal, "params", "sample_lambda", "must be non-negative")
	}
	if p.BlendWidth <= 0 || math.IsNaN(p.BlendWidth) || math.IsInf(p.BlendWidth, 0) {
		return domainError(EVal, "params", "blend_width", "must be finite and positive")
	}
	if p.Tolerance < 0 || math.IsNaN(p.Tolerance) || math.IsInf(p.Tolerance, 0) {
		return domainError(EVal, "params", "tolerance", "must be finite and non-negative")
	}
	if p.Workers <= 0 {
		return domainError(EVal, "params", "workers", "must be positive")
	}
	if p.BufferLimit <= 0 {
		return domainError(EVal, "params", "buffer_limit", "must be positive")
	}
	if p.QueueBudget <= 0 {
		return domainError(EVal, "params", "queue_budget", "must be positive")
	}
	return nil
}

type Room struct {
	ID                string        `json:"id"`
	Bounds            Bounds        `json:"bounds"`
	Resolution        [3]int        `json:"resolution"`
	Offset            Vec3          `json:"offset"`
	Step              Vec3          `json:"step"`
	ProbeIDs          []string      `json:"probe_ids"`
	FieldCoefficients [3][8]float64 `json:"field_coefficients"`
	ResolutionVersion string        `json:"resolution_version"`
}

type Probe struct {
	ID             string        `json:"id"`
	RoomID         string        `json:"room_id"`
	IJK            [3]int        `json:"ijk"`
	Position       Vec3          `json:"position"`
	Radiance       [3]float64    `json:"radiance"`
	RawSampleCount float64       `json:"raw_sample_count"`
	SampleCount    int           `json:"sample_count"`
	SHScale        int64         `json:"sh_scale"`
	RawSH          [3][9]float64 `json:"raw_sh"`
	SH             [3][9]int64   `json:"sh"`
	ContentHash    string        `json:"content_hash"`
	SampleBlob     []byte        `json:"-"`
}

type ResampleDirective struct {
	ID           string  `json:"id"`
	Time         float64 `json:"time"`
	QueryID      string  `json:"query_id"`
	TargetRoomID string  `json:"target_room_id"`
	Position     Vec3    `json:"position"`
	Reason       string  `json:"reason"`
}

type CameraFrame struct {
	ID     string  `json:"id"`
	Time   float64 `json:"time"`
	Eye    Vec3    `json:"eye"`
	Target Vec3    `json:"target"`
}

type Query struct {
	ID                 string  `json:"id"`
	FrameIndex         int     `json:"frame_index"`
	PixelID            string  `json:"pixel_id"`
	Time               float64 `json:"time"`
	Position           Vec3    `json:"position"`
	PrimaryRoomID      string  `json:"primary_room_id"`
	NeighborRoomID     string  `json:"neighbor_room_id,omitempty"`
	BoundaryAxis       int     `json:"boundary_axis,omitempty"`
	BoundaryValue      float64 `json:"boundary_value,omitempty"`
	DistanceToBoundary float64 `json:"distance_to_boundary,omitempty"`
	BlendRatioExact    float64 `json:"blend_ratio_exact"`
	NeighborPosition   Vec3    `json:"neighbor_position,omitempty"`
}

type Pixel struct {
	ID      string `json:"id"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	QueryID string `json:"query_id"`
}

type TruthSample struct {
	QueryID  string     `json:"query_id"`
	Position Vec3       `json:"position"`
	Expected [3]float64 `json:"expected"`
}

type Scene struct {
	Seed       int64               `json:"seed"`
	Params     Params              `json:"params"`
	Screen     Screen              `json:"screen"`
	Rooms      []Room              `json:"rooms"`
	Probes     []Probe             `json:"probes"`
	Directives []ResampleDirective `json:"directives"`
	Frames     []CameraFrame       `json:"frames"`
	Queries    []Query             `json:"queries"`
	Pixels     []Pixel             `json:"pixels"`
	Truths     []TruthSample       `json:"truths"`
}

type Screen struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (s *Scene) Room(id string) *Room {
	for i := range s.Rooms {
		if s.Rooms[i].ID == id {
			return &s.Rooms[i]
		}
	}
	return nil
}

func (s *Scene) ProbeByID(id string) *Probe {
	for i := range s.Probes {
		if s.Probes[i].ID == id {
			return &s.Probes[i]
		}
	}
	return nil
}

func (s *Scene) QueryByID(id string) *Query {
	for i := range s.Queries {
		if s.Queries[i].ID == id {
			return &s.Queries[i]
		}
	}
	return nil
}

func SortedProbeIDs(probes []Probe) []string {
	ids := make([]string, len(probes))
	for i := range probes {
		ids[i] = probes[i].ID
	}
	sort.Strings(ids)
	return ids
}

func canonicalHash(prefix string, payload any) string {
	b, err := json.Marshal(payload)
	if err != nil {
		b = []byte("canonical-marshal-error")
	}
	sum := sha256.Sum256(append([]byte(prefix), b...))
	return prefix + hex.EncodeToString(sum[:])[:16]
}

func HashProbeContent(p Probe) string {
	type payload struct {
		ID             string      `json:"id"`
		IJK            [3]int      `json:"ijk"`
		Position       Vec3        `json:"position"`
		Radiance       [3]float64  `json:"radiance"`
		RawSampleCount float64     `json:"raw_sample_count"`
		SampleCount    int         `json:"sample_count"`
		SHScale        int64       `json:"sh_scale"`
		SH             [3][9]int64 `json:"sh"`
	}
	return canonicalHash("probe-content:", payload{
		ID: p.ID, IJK: p.IJK, Position: p.Position, Radiance: p.Radiance,
		RawSampleCount: p.RawSampleCount, SampleCount: p.SampleCount,
		SHScale: p.SHScale, SH: p.SH,
	})
}
