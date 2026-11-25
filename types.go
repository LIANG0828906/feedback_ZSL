package colortrace

import "time"

const (
	AlgorithmVersion = "fixedpoint-trace-v1"
	SchemaVersion    = "texture-trace/v1"
	DefaultSeed      = int64(20261126)
	MinQuantBits     = 4
	MaxQuantBits     = 12
)

// Coord is a plane-local integer sample coordinate.
type Coord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// ChunkCoord identifies a chunk in the stage chunk grid.
type ChunkCoord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Region is a half-open integer rectangle.
type Region struct {
	MinX int `json:"min_x"`
	MinY int `json:"min_y"`
	MaxX int `json:"max_x"`
	MaxY int `json:"max_y"`
}

func (r Region) Contains(x, y int) bool {
	return x >= r.MinX && x < r.MaxX && y >= r.MinY && y < r.MaxY
}

func (r Region) Width() int  { return r.MaxX - r.MinX }
func (r Region) Height() int { return r.MaxY - r.MinY }
func (r Region) Samples() int {
	if r.Width() <= 0 || r.Height() <= 0 {
		return 0
	}
	return r.Width() * r.Height()
}

// PlaneDesc describes one source or target plane.
type PlaneDesc struct {
	Name       string `json:"name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Chroma     bool   `json:"chroma"`
	SubsampleX int    `json:"subsample_x,omitempty"`
	SubsampleY int    `json:"subsample_y,omitempty"`
}

// Plane is a decoded source plane. Values are integer codes at Bits depth.
type Plane struct {
	PlaneDesc
	Bits   int     `json:"bits,omitempty"`
	Values []int64 `json:"values"`
}

// Tap is one integer kernel coefficient.
type Tap struct {
	DX int   `json:"dx"`
	DY int   `json:"dy"`
	W  int64 `json:"w"`
}

// Kernel is a declared fixed-point resampling kernel.
type Kernel struct {
	Name      string `json:"name"`
	RadiusX   int    `json:"radius_x"`
	RadiusY   int    `json:"radius_y"`
	NormScale int64  `json:"norm_scale"`
	Taps      []Tap  `json:"taps"`
}

// WeightSum returns the declared coefficient sum.
func (k Kernel) WeightSum() int64 {
	var sum int64
	for _, tap := range k.Taps {
		sum += tap.W
	}
	return sum
}

// StageConfig is one ordered processing level.
type StageConfig struct {
	Index        int     `json:"index"`
	ID           string  `json:"id"`
	Description  string  `json:"description,omitempty"`
	FactorX      int     `json:"factor_x"`
	FactorY      int     `json:"factor_y"`
	PhaseX       int     `json:"phase_x"`
	PhaseY       int     `json:"phase_y"`
	AccumScale   int64   `json:"accum_scale"`
	Bits         int     `json:"bits"`
	Kernel       Kernel  `json:"kernel"`
	ChromaKernel *Kernel `json:"chroma_kernel,omitempty"`
	ParamVersion string  `json:"param_version"`
}

// TraceQuery selects a sample that must appear in the truth manifest.
type TraceQuery struct {
	Plane string `json:"plane"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
}

// Project is the complete deterministic sample and configuration contract.
type Project struct {
	SchemaVersion          string         `json:"schema_version"`
	ProjectID              string         `json:"project_id"`
	Seed                   int64          `json:"seed"`
	BaseBits               int            `json:"base_bits"`
	Width                  int            `json:"width"`
	Height                 int            `json:"height"`
	ChunkSize              int            `json:"chunk_size"`
	BandTolerance          int64          `json:"band_tolerance"`
	DominantShareThreshold float64        `json:"dominant_share_threshold"`
	Planes                 []*Plane       `json:"planes"`
	Stages                 []StageConfig  `json:"stages"`
	Targets                []*Plane       `json:"targets,omitempty"`
	TracePoints            []TraceQuery   `json:"trace_points"`
	Truth                  *TruthManifest `json:"truth,omitempty"`
}

func (p *Project) Plane(name string) *Plane {
	for _, plane := range p.Planes {
		if plane.Name == name {
			return plane
		}
	}
	return nil
}

func (p *Project) PlaneNames() []string {
	names := make([]string, 0, len(p.Planes))
	for _, plane := range p.Planes {
		names = append(names, plane.Name)
	}
	return names
}

// OperatorBreakdown is the exact additive decomposition at one sample.
type OperatorBreakdown struct {
	ResampleNorm Fraction `json:"resample_norm"`
	AccumTrunc   Fraction `json:"accum_trunc"`
	BitRound     Fraction `json:"bit_round"`
	Chroma       Fraction `json:"chroma"`
}

func (b OperatorBreakdown) Sum() Fraction {
	return SumFracs(b.ResampleNorm, b.AccumTrunc, b.BitRound, b.Chroma)
}

// TruncationRecord identifies an integer truncation location and amount.
type TruncationRecord struct {
	Stage         int      `json:"stage"`
	Plane         string   `json:"plane"`
	Coord         Coord    `json:"coord"`
	ChunkID       string   `json:"chunk_id"`
	Kind          string   `json:"kind"`
	Position      string   `json:"position"`
	RawValue      int64    `json:"raw_value"`
	Divisor       int64    `json:"divisor"`
	Quotient      int64    `json:"quotient"`
	Remainder     int64    `json:"remainder"`
	Discarded     Fraction `json:"discarded"`
	GridLSB       int64    `json:"grid_lsb,omitempty"`
	RoundingDelta int64    `json:"rounding_delta,omitempty"`
}

// PointLedger is the per-sample provenance record.
type PointLedger struct {
	Stage               int                `json:"stage"`
	Plane               string             `json:"plane"`
	Coord               Coord              `json:"coord"`
	Chunk               ChunkCoord         `json:"chunk"`
	ChunkID             string             `json:"chunk_id"`
	InputHash           string             `json:"input_hash"`
	ContentHash         string             `json:"content_hash"`
	CacheKey            string             `json:"cache_key"`
	Anchor              Coord              `json:"anchor"`
	AccumScale          int64              `json:"accum_scale"`
	FactorX             int                `json:"factor_x"`
	FactorY             int                `json:"factor_y"`
	Bits                int                `json:"bits"`
	KernelRadiusX       int                `json:"kernel_radius_x"`
	KernelRadiusY       int                `json:"kernel_radius_y"`
	WeightSum           int64              `json:"weight_sum"`
	NormScale           int64              `json:"norm_scale"`
	RawWeightedSum      int64              `json:"raw_weighted_sum"`
	Remainder           int64              `json:"remainder"`
	PreQuantFixed       int64              `json:"pre_quant_fixed"`
	QuantGrid           int64              `json:"quant_grid"`
	Code                int64              `json:"code"`
	OutputFixed         int64              `json:"output_fixed"`
	RawReferenceFixed   Fraction           `json:"raw_reference_fixed"`
	TotalDeviation      Fraction           `json:"total_deviation"`
	CumulativeDeviation Fraction           `json:"cumulative_deviation"`
	Breakdown           OperatorBreakdown  `json:"breakdown"`
	Truncations         []TruncationRecord `json:"truncations"`
}

// PlaneChunk is one plane's output inside a chunk-stage unit.
type PlaneChunk struct {
	Plane       string             `json:"plane"`
	Width       int                `json:"width"`
	Height      int                `json:"height"`
	Region      Region             `json:"region"`
	Values      []int64            `json:"values"`
	Codes       []int64            `json:"codes"`
	Ledgers     []PointLedger      `json:"ledgers"`
	Truncations []TruncationRecord `json:"truncations"`
}

// ChunkResult is the deterministic output of one stage-chunk unit.
type ChunkResult struct {
	ProjectID    string                 `json:"project_id"`
	Stage        int                    `json:"stage"`
	Chunk        ChunkCoord             `json:"chunk"`
	ChunkID      string                 `json:"chunk_id"`
	Region       Region                 `json:"region"`
	InputHash    string                 `json:"input_hash"`
	ContentHash  string                 `json:"content_hash"`
	CacheKey     string                 `json:"cache_key"`
	UpstreamKeys []string               `json:"upstream_keys"`
	Planes       map[string]*PlaneChunk `json:"planes"`
	PlaneNames   []string               `json:"plane_names"`
	StartedAt    time.Time              `json:"started_at,omitempty"`
	FinishedAt   time.Time              `json:"finished_at,omitempty"`
}

// PlaneSample is an assembled full plane at one stage.
type PlaneSample struct {
	Name    string        `json:"name"`
	Width   int           `json:"width"`
	Height  int           `json:"height"`
	Values  []int64       `json:"values"`
	Codes   []int64       `json:"codes"`
	Ledgers []PointLedger `json:"ledgers"`
}

func (p *PlaneSample) Index(x, y int) int { return y*p.Width + x }

func (p *PlaneSample) LedgerAt(x, y int) *PointLedger {
	idx := p.Index(x, y)
	if idx < 0 || idx >= len(p.Ledgers) {
		return nil
	}
	return &p.Ledgers[idx]
}

// Sample is an assembled stage output.
type Sample struct {
	Stage  int                     `json:"stage"`
	Planes map[string]*PlaneSample `json:"planes"`
}

// StageResult contains output and all chunk products for a level.
type StageResult struct {
	Config StageConfig             `json:"config"`
	Sample *Sample                 `json:"sample"`
	Chunks map[string]*ChunkResult `json:"chunks"`
}

// BandReport is the first tolerances-exceeding adjacent band.
type BandReport struct {
	Stage            int               `json:"stage"`
	Plane            string            `json:"plane"`
	Coord            Coord             `json:"coord"`
	Neighbor         Coord             `json:"neighbor"`
	Chunk            ChunkCoord        `json:"chunk"`
	ChunkID          string            `json:"chunk_id"`
	Jump             int64             `json:"jump"`
	Tolerance        int64             `json:"tolerance"`
	DominantOperator string            `json:"dominant_operator"`
	Breakdown        OperatorBreakdown `json:"breakdown"`
	Shares           []Share           `json:"shares"`
	Ambiguous        bool              `json:"ambiguous"`
}

// StageBound is the observed-error envelope for one stage.
type StageBound struct {
	Stage          int      `json:"stage"`
	FactorX        int      `json:"factor_x"`
	FactorY        int      `json:"factor_y"`
	AccumScale     int64    `json:"accum_scale"`
	Bits           int      `json:"bits"`
	RadiusX        int      `json:"radius_x"`
	RadiusY        int      `json:"radius_y"`
	ResampleNorm   Fraction `json:"resample_norm"`
	AccumTrunc     Fraction `json:"accum_trunc"`
	BitRound       Fraction `json:"bit_round"`
	Chroma         Fraction `json:"chroma"`
	LocalMagnitude Fraction `json:"local_magnitude"`
	PropagatedLow  Fraction `json:"propagated_low"`
	PropagatedHigh Fraction `json:"propagated_high"`
	Low            Fraction `json:"low"`
	High           Fraction `json:"high"`
}

// BoundReport checks measured and truth-expected deviations against envelopes.
type BoundReport struct {
	Stage               int          `json:"stage"`
	Plane               string       `json:"plane"`
	Coord               Coord        `json:"coord"`
	Measured            Fraction     `json:"measured"`
	TruthExpected       Fraction     `json:"truth_expected"`
	Low                 Fraction     `json:"low"`
	High                Fraction     `json:"high"`
	CumulativeMagnitude Fraction     `json:"cumulative_magnitude"`
	StageBounds         []StageBound `json:"stage_bounds"`
	OK                  bool         `json:"ok"`
}

// TruthStageSample is the analytic expected value for one trace point/stage.
type TruthStageSample struct {
	Stage              int               `json:"stage"`
	Coord              Coord             `json:"coord"`
	Chunk              ChunkCoord        `json:"chunk"`
	ExpectedCode       int64             `json:"expected_code"`
	ExpectedFixed      int64             `json:"expected_fixed"`
	ExpectedDeviation  Fraction          `json:"expected_deviation"`
	ExpectedCumulative Fraction          `json:"expected_cumulative"`
	Breakdown          OperatorBreakdown `json:"breakdown"`
}

// TruthPoint is a trace coordinate with all level expectations.
type TruthPoint struct {
	Plane  string             `json:"plane"`
	Source Coord              `json:"source"`
	Stages []TruthStageSample `json:"stages"`
}

// TruthBand is the analytic first-band answer.
type TruthBand struct {
	Stage            int        `json:"stage"`
	Plane            string     `json:"plane"`
	Coord            Coord      `json:"coord"`
	Neighbor         Coord      `json:"neighbor"`
	Chunk            ChunkCoord `json:"chunk"`
	DominantOperator string     `json:"dominant_operator"`
}

// TruthManifest is the sole analytic precision oracle.
type TruthManifest struct {
	Seed        int64        `json:"seed"`
	Fingerprint string       `json:"fingerprint"`
	Points      []TruthPoint `json:"points"`
	Band        TruthBand    `json:"band"`
}

// SnapshotMeta identifies a persistent checkpoint generation.
type SnapshotMeta struct {
	ProjectID                string         `json:"project_id"`
	StageSequenceFingerprint string         `json:"stage_sequence_fingerprint"`
	ParamVersionFingerprint  string         `json:"param_version_fingerprint"`
	AlgorithmVersion         string         `json:"algorithm_version"`
	TruthFingerprint         string         `json:"truth_fingerprint"`
	ParamVersions            map[int]string `json:"param_versions,omitempty"`
}

// UnitState is the chunk-level state machine state.
type UnitState string

const (
	StatePending   UnitState = "pending"
	StateRunning   UnitState = "processing"
	StateCommitted UnitState = "committed"
	StateFailed    UnitState = "failed"
)

// UnitRecord is the atomic persistent checkpoint row.
type UnitRecord struct {
	ProjectID                string    `json:"project_id"`
	Stage                    int       `json:"stage"`
	ChunkID                  string    `json:"chunk_id"`
	ChunkX                   int       `json:"chunk_x"`
	ChunkY                   int       `json:"chunk_y"`
	State                    UnitState `json:"state"`
	OutputDigest             string    `json:"output_digest"`
	TruncationDigest         string    `json:"truncation_digest"`
	TruncationCount          int       `json:"truncation_count"`
	TruncationAbsoluteTotal  int64     `json:"truncation_absolute_total"`
	InputContentHash         string    `json:"input_content_hash"`
	ContentHash              string    `json:"content_hash"`
	CacheKey                 string    `json:"cache_key"`
	StageSequenceFingerprint string    `json:"stage_sequence_fingerprint"`
	ParamVersion             string    `json:"param_version"`
	AlgorithmVersion         string    `json:"algorithm_version"`
	OutputJSON               []byte    `json:"output_json"`
	CommitSeq                int64     `json:"commit_seq"`
	Attempt                  int       `json:"attempt"`
	LastError                string    `json:"last_error,omitempty"`
	UpdatedAt                time.Time `json:"updated_at"`
}

// QueueWait records a blocking budget event.
type QueueWait struct {
	Stage   int           `json:"stage"`
	ChunkID string        `json:"chunk_id"`
	Reason  string        `json:"reason"`
	Waited  time.Duration `json:"waited_ns"`
	At      time.Time     `json:"at"`
}

// CommitEntry is one stable serialized commit position.
type CommitEntry struct {
	Seq      int64  `json:"seq"`
	Stage    int    `json:"stage"`
	ChunkID  string `json:"chunk_id"`
	CacheKey string `json:"cache_key"`
}

// Mismatch identifies the first path comparison violation.
type Mismatch struct {
	Stage    int    `json:"stage"`
	ChunkID  string `json:"chunk_id,omitempty"`
	Plane    string `json:"plane,omitempty"`
	Coord    *Coord `json:"coord,omitempty"`
	Field    string `json:"field"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
}
