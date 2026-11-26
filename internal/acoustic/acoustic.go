// Package acoustic contains the deterministic domain types, codes and fixed-point scales.
package acoustic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// GeoScale represents one millimetre in geometry fixed-point values.
	GeoScale int64 = 1000
	// EnergyScale represents one energy unit in micro-units.
	EnergyScale int64 = 1_000_000
	// NormalScale represents unit normal components in fixed-point values.
	NormalScale int64 = 1_000_000
	// AbsorptionQuantum is the declared absorption table tick: 0.001.
	AbsorptionQuantum int64 = 1_000

	AlgorithmVersion = "acoustic-field-v1.0"
	SchemaVersion    = "acoustic-level-v1"
	StoreSchema      = "acoustic-checkpoint-v1"
)

type ErrorCode string

const (
	CodeSchema       ErrorCode = "E_SCHEMA"
	CodeState        ErrorCode = "E_STATE"
	CodeConservation ErrorCode = "E_CONS"
	CodeBudget       ErrorCode = "E_BUDGET"
	CodeCheckpoint   ErrorCode = "E_CKPT"
	CodeHash         ErrorCode = "E_HASH"
)

// Error is the structured domain error used by CLI and tests.
type Error struct {
	Code     ErrorCode      `json:"code"`
	ObjectID string         `json:"object_id,omitempty"`
	Field    string         `json:"field,omitempty"`
	Message  string         `json:"message"`
	Details  map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", e.Code)
	if e.ObjectID != "" {
		fmt.Fprintf(&b, " object=%s", e.ObjectID)
	}
	if e.Field != "" {
		fmt.Fprintf(&b, " field=%s", e.Field)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	return b.String()
}

func NewError(code ErrorCode, objectID, field, format string, args ...any) *Error {
	return &Error{Code: code, ObjectID: objectID, Field: field, Message: fmt.Sprintf(format, args...)}
}

func SchemaError(objectID, field, format string, args ...any) *Error {
	return NewError(CodeSchema, objectID, field, format, args...)
}

func StateError(objectID, from, event string) *Error {
	return NewError(CodeState, objectID, "state", "illegal transition from %s via %s", from, event)
}

func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if e, ok := err.(*Error); ok {
		return e, true
	}
	return nil, false
}

// Point is a fixed-point 2D coordinate in millimetres.
type Point struct {
	X int64 `json:"x_q"`
	Y int64 `json:"y_q"`
}

// Rect is a closed axis-aligned bounding rectangle in fixed-point coordinates.
type Rect struct {
	Min Point `json:"min"`
	Max Point `json:"max"`
}

func (r Rect) WidthQ() int64  { return r.Max.X - r.Min.X }
func (r Rect) HeightQ() int64 { return r.Max.Y - r.Min.Y }

func (r Rect) Contains(p Point) bool {
	return p.X >= r.Min.X && p.X <= r.Max.X && p.Y >= r.Min.Y && p.Y <= r.Max.Y
}

func (r Rect) IntersectsInterior(o Rect) bool {
	return r.Min.X < o.Max.X && r.Max.X > o.Min.X &&
		r.Min.Y < o.Max.Y && r.Max.Y > o.Min.Y
}

type Band struct {
	ID     int    `json:"id"`
	Label  string `json:"label"`
	Center int    `json:"center_hz"`
	// AlphaQ is absorption coefficient multiplied by EnergyScale.
	AlphaQ int64 `json:"alpha_q"`
}

type Surface struct {
	ID       string `json:"id"`
	ChunkID  string `json:"chunk_id,omitempty"`
	A        Point  `json:"a"`
	B        Point  `json:"b"`
	Normal   Point  `json:"normal_q"`
	Material string `json:"material,omitempty"`
}

type Chunk struct {
	ID       string    `json:"id"`
	Row      int       `json:"row"`
	Col      int       `json:"col"`
	Bounds   Rect      `json:"bounds"`
	Bands    []Band    `json:"bands"`
	Surfaces []Surface `json:"surfaces"`
}

type Source struct {
	ID        string  `json:"id"`
	ChunkID   string  `json:"chunk_id"`
	Position  Point   `json:"position"`
	EmissionQ []int64 `json:"emission_q"`
}

type Listener struct {
	ID       string `json:"id"`
	ChunkID  string `json:"chunk_id"`
	Position Point  `json:"position"`
}

type Budget struct {
	MaxParallel         int `json:"max_parallel"`
	PathBufferPerWorker int `json:"path_buffer_per_worker"`
	MaxQueuedChunks     int `json:"max_queued_chunks"`
}

type Params struct {
	MaxBounces                   int    `json:"max_bounces"`
	BoundaryWeight               int    `json:"boundary_weight"`
	BoundaryTransmissionPermille int64  `json:"boundary_transmission_permille"`
	ConservationToleranceQ       int64  `json:"conservation_tolerance_q"`
	ParamVersion                 string `json:"param_version,omitempty"`
	AlgorithmVersion             string `json:"algorithm_version"`
	Budget                       Budget `json:"budget"`
}

func DefaultParams() Params {
	p := Params{
		MaxBounces:                   2,
		BoundaryWeight:               4,
		BoundaryTransmissionPermille: 900,
		ConservationToleranceQ:       1,
		AlgorithmVersion:             AlgorithmVersion,
		Budget: Budget{
			MaxParallel:         3,
			PathBufferPerWorker: 64,
			MaxQueuedChunks:     2,
		},
	}
	p.ParamVersion = p.Version()
	return p
}

// Version returns a content-derived parameter version. Budget is excluded because it changes scheduling, not acoustics.
func (p Params) Version() string {
	core := struct {
		MaxBounces                   int
		BoundaryWeight               int
		BoundaryTransmissionPermille int64
		ConservationToleranceQ       int64
		AlgorithmVersion             string
	}{p.MaxBounces, p.BoundaryWeight, p.BoundaryTransmissionPermille, p.ConservationToleranceQ, p.AlgorithmVersion}
	return "param-" + ShortHash(core, 16)
}

type Level struct {
	SchemaVersion string     `json:"schema_version"`
	Seed          int64      `json:"seed"`
	GridRows      int        `json:"grid_rows"`
	GridCols      int        `json:"grid_cols"`
	Bounds        Rect       `json:"bounds"`
	BandIDs       []int      `json:"band_ids"`
	Chunks        []Chunk    `json:"chunks"`
	Sources       []Source   `json:"sources"`
	Listeners     []Listener `json:"listeners"`
	Params        Params     `json:"params"`
}

type QuantRecord struct {
	PathID       string `json:"path_id,omitempty"`
	BandID       int    `json:"band_id,omitempty"`
	Stage        string `json:"stage"`
	Location     string `json:"location"`
	InputQ       int64  `json:"input_q,omitempty"`
	NumeratorQ   int64  `json:"numerator_q,omitempty"`
	DenominatorQ int64  `json:"denominator_q,omitempty"`
	TruncatedQ   int64  `json:"truncated_q"`
	ResidualQ    int64  `json:"residual_q"`
}

type PathResult struct {
	Key             string        `json:"key"`
	ListenerID      string        `json:"listener_id"`
	Sequence        []string      `json:"sequence"`
	Bounces         int           `json:"bounces"`
	LengthQ         int64         `json:"length_q"`
	SegmentLengthsQ []int64       `json:"segment_lengths_q"`
	BandEnergyQ     []int64       `json:"band_energy_q"`
	AbsorbedQ       []int64       `json:"absorbed_q"`
	Quantization    []QuantRecord `json:"quantization,omitempty"`
}

type ListenerEnergy struct {
	ListenerID   string  `json:"listener_id"`
	BandArrivalQ []int64 `json:"band_arrival_q"`
}

type Transfer struct {
	ID                   string  `json:"id"`
	FromChunkID          string  `json:"from_chunk_id"`
	ToChunkID            string  `json:"to_chunk_id"`
	Boundary             string  `json:"boundary"`
	Position             Point   `json:"position_q"`
	BandSentQ            []int64 `json:"band_sent_q"`
	BandDeliveredQ       []int64 `json:"band_delivered_q"`
	BandLossQ            []int64 `json:"band_loss_q"`
	TransmissionPermille int64   `json:"transmission_permille"`
	TransferVersion      string  `json:"transfer_version"`
}

type Dependency struct {
	Key           string `json:"key"`
	Kind          string `json:"kind"`
	ChunkID       string `json:"chunk_id"`
	TargetChunkID string `json:"target_chunk_id,omitempty"`
	Field         string `json:"field,omitempty"`
}

type ChunkResult struct {
	ChunkID            string           `json:"chunk_id"`
	Row                int              `json:"row"`
	Col                int              `json:"col"`
	ContentHash        string           `json:"content_hash"`
	LocalCacheKey      string           `json:"local_cache_key"`
	CacheKey           string           `json:"cache_key"`
	ParamVersion       string           `json:"param_version"`
	AlgorithmVersion   string           `json:"algorithm_version"`
	PathDigest         string           `json:"path_digest"`
	Paths              []PathResult     `json:"paths"`
	ListenerEnergies   []ListenerEnergy `json:"listener_energies"`
	ListenerArrivalQ   []int64          `json:"listener_arrival_q"`
	ArrivalEnergyQ     []int64          `json:"arrival_energy_q"`
	AbsorbedQ          []int64          `json:"absorbed_q"`
	InboundQ           []int64          `json:"inbound_q"`
	RetainedQ          []int64          `json:"retained_q"`
	OutboundSentQ      []int64          `json:"outbound_sent_q"`
	Transfers          []Transfer       `json:"transfers"`
	InboundTransferIDs []string         `json:"inbound_transfer_ids"`
	InboundTransfers   []Transfer       `json:"inbound_transfers,omitempty"`
	Dependencies       []string         `json:"dependencies"`
	DependencyRecords  []Dependency     `json:"dependency_records,omitempty"`
	Quantization       []QuantRecord    `json:"quantization,omitempty"`
	PathBufferUsed     int              `json:"path_buffer_used"`
}

type PathTruth struct {
	ListenerID   string   `json:"listener_id"`
	PathKey      string   `json:"path_key"`
	Sequence     []string `json:"sequence"`
	Bounces      int      `json:"bounces"`
	LengthQ      int64    `json:"length_q"`
	BandArrivalQ []int64  `json:"band_arrival_q"`
}

type SampleTruth struct {
	ListenerID           string      `json:"listener_id"`
	ChunkID              string      `json:"chunk_id"`
	ExpectedArrivalQ     []int64     `json:"expected_arrival_q"`
	ExpectedPathLengthsQ []int64     `json:"expected_path_lengths_q"`
	Paths                []PathTruth `json:"paths"`
}

type Truth struct {
	Seed                  int64         `json:"seed"`
	OracleVersion         string        `json:"oracle_version"`
	Fingerprint           string        `json:"fingerprint"`
	BandIDs               []int         `json:"band_ids"`
	Samples               []SampleTruth `json:"samples"`
	ConservationResidualQ []int64       `json:"conservation_residual_q"`
}

type ConservationViolation struct {
	Code          ErrorCode `json:"code"`
	BandID        int       `json:"band_id"`
	SourceChunkID string    `json:"source_chunk_id"`
	Kind          string    `json:"kind"`
	ResidualQ     int64     `json:"residual_q"`
	ToleranceQ    int64     `json:"tolerance_q"`
	Message       string    `json:"message"`
}

type ConservationReport struct {
	OK            bool                    `json:"ok"`
	ToleranceQ    int64                   `json:"tolerance_q"`
	BandIDs       []int                   `json:"band_ids"`
	EmittedQ      []int64                 `json:"emitted_q"`
	RetainedQ     []int64                 `json:"retained_q"`
	TransferLossQ []int64                 `json:"transfer_loss_q"`
	ResidualQ     []int64                 `json:"residual_q"`
	Violations    []ConservationViolation `json:"violations,omitempty"`
}

type Invalidation struct {
	ChunkID       string `json:"chunk_id"`
	Reason        string `json:"reason"`
	DependencyKey string `json:"dependency_key"`
}

type QueueRecord struct {
	ChunkID    string `json:"chunk_id"`
	WorkerID   int    `json:"worker_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Blocked    bool   `json:"blocked"`
	WaitMillis int64  `json:"wait_ms"`
}

type CommitInfo struct {
	ChunkID  string `json:"chunk_id"`
	Seq      int    `json:"seq"`
	TxnSeq   int64  `json:"txn_seq,omitempty"`
	CacheKey string `json:"cache_key"`
	Source   string `json:"source"` // computed, reused, recovered
}

type ChunkFailure struct {
	ChunkID string `json:"chunk_id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Report struct {
	SchemaVersion    string             `json:"schema_version"`
	Seed             int64              `json:"seed"`
	RoundID          string             `json:"round_id"`
	Mode             string             `json:"mode"`
	LevelHash        string             `json:"level_hash"`
	ParamVersion     string             `json:"param_version"`
	AlgorithmVersion string             `json:"algorithm_version"`
	TruthFingerprint string             `json:"truth_fingerprint"`
	ChunkOrder       []string           `json:"chunk_order"`
	CommitSequence   []CommitInfo       `json:"commit_sequence"`
	Chunks           []ChunkResult      `json:"chunks"`
	Conservation     ConservationReport `json:"conservation"`
	Invalidations    []Invalidation     `json:"invalidations,omitempty"`
	Queue            []QueueRecord      `json:"queue,omitempty"`
	Failures         []ChunkFailure     `json:"failures,omitempty"`
	Progress         Progress           `json:"progress"`
}

type Progress struct {
	CommittedChunks int   `json:"committed_chunks"`
	TotalChunks     int   `json:"total_chunks"`
	CommittedRounds int64 `json:"committed_rounds"`
}

// CanonicalJSON provides deterministic bytes for hashing and checkpoints.
func CanonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("acoustic: canonical marshal failed: %v", err))
	}
	return b
}

func HashBytes(prefix string, b []byte) string {
	sum := sha256.Sum256(append([]byte(prefix), b...))
	return hex.EncodeToString(sum[:])
}

func HashValue(prefix string, v any) string {
	return HashBytes(prefix, CanonicalJSON(v))
}

func ShortHash(v any, n int) string {
	h := HashValue("", v)
	if n > len(h) {
		n = len(h)
	}
	return h[:n]
}

func CopyInt64s(in []int64) []int64 {
	if in == nil {
		return nil
	}
	out := make([]int64, len(in))
	copy(out, in)
	return out
}

func Abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// IsQuantumAligned reports whether value is a positive declared-tick coefficient.
func IsQuantumAligned(value, quantum int64) bool {
	return value > 0 && value%quantum == 0
}
