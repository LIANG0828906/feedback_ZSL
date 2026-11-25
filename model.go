package acousticfield

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const AlgorithmVersion = "acoustic-field-v1"

type Band struct {
	ID       string `json:"id"`
	CenterHz int64  `json:"center_hz"`
}

type Rect struct {
	MinX int64 `json:"min_x"`
	MinY int64 `json:"min_y"`
	MaxX int64 `json:"max_x"`
	MaxY int64 `json:"max_y"`
}

func (r Rect) Width() int64  { return r.MaxX - r.MinX }
func (r Rect) Height() int64 { return r.MaxY - r.MinY }
func (r Rect) Center() Point {
	return Point{X: r.MinX + r.Width()/2, Y: r.MinY + r.Height()/2}
}

type Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

// Vec is a Q16 fixed-point vector. A unit normal has squared length Q16^2.
type Vec struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type Absorption struct {
	BandID      string `json:"band_id"`
	Coefficient int64  `json:"coefficient_q16"`
}

type Reflector struct {
	ID       string `json:"id"`
	A        Point  `json:"a"`
	B        Point  `json:"b"`
	Normal   Vec    `json:"normal"`
	Material string `json:"material"`
}

type Chunk struct {
	ID         int          `json:"id"`
	Bounds     Rect         `json:"bounds"`
	Absorption []Absorption `json:"absorption"`
	Reflectors []Reflector  `json:"reflectors"`
}

type BandEnergy struct {
	BandID string `json:"band_id"`
	Energy int64  `json:"energy"`
}

type Source struct {
	ID       string       `json:"id"`
	ChunkID  int          `json:"chunk_id"`
	Position Point        `json:"position"`
	Emission []BandEnergy `json:"emission"`
}

type Listener struct {
	ID      string `json:"id"`
	ChunkID int    `json:"chunk_id"`
	Point   Point  `json:"point"`
}

type Level struct {
	ID        string     `json:"id"`
	Seed      int64      `json:"seed"`
	Bounds    Rect       `json:"bounds"`
	Bands     []Band     `json:"bands"`
	Chunks    []Chunk    `json:"chunks"`
	Sources   []Source   `json:"sources"`
	Listeners []Listener `json:"listeners"`
}

type Path struct {
	ID                   string       `json:"id"`
	ChunkID              int          `json:"chunk_id"`
	Reflectors           []string     `json:"reflectors"`
	Points               []Point      `json:"points"`
	LengthMicros         int64        `json:"length_micros"`
	Energy               []BandEnergy `json:"energy"`
	QuantizationResidual int64        `json:"quantization_residual"`
}

type Quantization struct {
	Stage          string `json:"stage"`
	ChunkID        int    `json:"chunk_id"`
	PathID         string `json:"path_id,omitempty"`
	BandID         string `json:"band_id,omitempty"`
	ExactNumerator int64  `json:"exact_numerator"`
	Denominator    int64  `json:"denominator"`
	Value          int64  `json:"value"`
	Residual       int64  `json:"residual"`
}

// Truncation records where a finite-budget calculation was stopped. It is
// deliberately separate from physical boundary escape.
type Truncation struct {
	Stage    string `json:"stage"`
	ChunkID  int    `json:"chunk_id"`
	BandID   string `json:"band_id,omitempty"`
	Wave     int    `json:"wave"`
	Position Point  `json:"position"`
	Residual int64  `json:"residual"`
}

type Boundary struct {
	Side     string `json:"side"`
	Position Point  `json:"position"`
	Length   int64  `json:"length_micros"`
}

type Transfer struct {
	ID                   string       `json:"id"`
	SourceID             string       `json:"source_id"`
	FromChunk            int          `json:"from_chunk"`
	ToChunk              int          `json:"to_chunk"`
	Boundary             Boundary     `json:"boundary"`
	Wave                 int          `json:"wave"`
	Energy               []BandEnergy `json:"energy"`
	QuantizationResidual []BandEnergy `json:"quantization_residual"`
	Version              string       `json:"version"`
}

type Escape struct {
	SourceID string    `json:"source_id"`
	ChunkID  int       `json:"chunk_id"`
	BandID   string    `json:"band_id"`
	Energy   int64     `json:"energy"`
	Reason   string    `json:"reason"`
	Boundary *Boundary `json:"boundary,omitempty"`
	Wave     int       `json:"wave"`
}

type SourceEnergy struct {
	SourceID string       `json:"source_id"`
	Energy   []BandEnergy `json:"energy"`
}

type Dependency struct {
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	Version  string `json:"version"`
	CacheKey string `json:"cache_key,omitempty"`
	Wave     int    `json:"wave,omitempty"`
}

type ChunkResult struct {
	ChunkID          int            `json:"chunk_id"`
	RoundID          int64          `json:"round_id"`
	State            ChunkState     `json:"state"`
	Paths            []Path         `json:"paths"`
	PathDigest       string         `json:"path_digest"`
	ArrivalEnergy    []BandEnergy   `json:"arrival_energy"`
	SourceEnergy     []SourceEnergy `json:"source_energy"`
	Incoming         []Transfer     `json:"incoming"`
	Outgoing         []Transfer     `json:"outgoing"`
	Escapes          []Escape       `json:"escapes"`
	Dependencies     []Dependency   `json:"dependencies"`
	Quantization     []Quantization `json:"quantization"`
	Truncations      []Truncation   `json:"truncations"`
	ContentHash      string         `json:"content_hash"`
	ParamsVersion    string         `json:"params_version"`
	AlgorithmVersion string         `json:"algorithm_version"`
	CacheKey         string         `json:"cache_key"`
	Reused           bool           `json:"reused"`
}

type TruthPath struct {
	ChunkID      int      `json:"chunk_id"`
	PathID       string   `json:"path_id"`
	Reflectors   []string `json:"reflectors"`
	LengthMicros int64    `json:"length_micros"`
}

type TruthChunk struct {
	ChunkID      int            `json:"chunk_id"`
	Energy       []BandEnergy   `json:"energy"`
	SourceEnergy []SourceEnergy `json:"source_energy"`
}

type TruthListener struct {
	ListenerID  string       `json:"listener_id"`
	ChunkID     int          `json:"chunk_id"`
	BandEnergy  []BandEnergy `json:"band_energy"`
	TotalEnergy int64        `json:"total_energy"`
}

type TruthSource struct {
	SourceID string       `json:"source_id"`
	Emission []BandEnergy `json:"emission"`
	Escape   []BandEnergy `json:"escape"`
}

type Truth struct {
	LevelID     string          `json:"level_id"`
	Bands       []string        `json:"bands"`
	Chunks      []TruthChunk    `json:"chunks"`
	Paths       []TruthPath     `json:"paths"`
	Listeners   []TruthListener `json:"listeners"`
	Sources     []TruthSource   `json:"sources"`
	Fingerprint string          `json:"fingerprint"`
}

type Sample struct {
	Level *Level `json:"level"`
	Truth Truth  `json:"truth"`
}

func canonicalHash(v any) string {
	// All persisted objects use sorted slices and maps are avoided. Encoding
	// therefore has a stable, version-local byte representation.
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func bandValue(v []BandEnergy, band string) int64 {
	for _, e := range v {
		if e.BandID == band {
			return e.Energy
		}
	}
	return 0
}

func addBandEnergy(dst []BandEnergy, band string, amount int64) []BandEnergy {
	for i := range dst {
		if dst[i].BandID == band {
			dst[i].Energy += amount
			return dst
		}
	}
	dst = append(dst, BandEnergy{BandID: band, Energy: amount})
	sortBandEnergy(dst)
	return dst
}

func chunkByID(level *Level, id int) (*Chunk, bool) {
	for i := range level.Chunks {
		if level.Chunks[i].ID == id {
			return &level.Chunks[i], true
		}
	}
	return nil, false
}

func cloneChunkResult(in ChunkResult) ChunkResult {
	out := in
	out.Paths = make([]Path, len(in.Paths))
	for i, p := range in.Paths {
		out.Paths[i] = p
		out.Paths[i].Reflectors = append([]string(nil), p.Reflectors...)
		out.Paths[i].Points = append([]Point(nil), p.Points...)
		out.Paths[i].Energy = cloneBandEnergy(p.Energy)
	}
	out.ArrivalEnergy = cloneBandEnergy(in.ArrivalEnergy)
	out.SourceEnergy = make([]SourceEnergy, len(in.SourceEnergy))
	for i, se := range in.SourceEnergy {
		out.SourceEnergy[i] = SourceEnergy{SourceID: se.SourceID, Energy: cloneBandEnergy(se.Energy)}
	}
	out.Incoming = cloneTransfers(in.Incoming)
	out.Outgoing = cloneTransfers(in.Outgoing)
	out.Escapes = make([]Escape, len(in.Escapes))
	for i, e := range in.Escapes {
		out.Escapes[i] = e
		if e.Boundary != nil {
			b := *e.Boundary
			out.Escapes[i].Boundary = &b
		}
	}
	out.Dependencies = append([]Dependency(nil), in.Dependencies...)
	out.Quantization = append([]Quantization(nil), in.Quantization...)
	out.Truncations = append([]Truncation(nil), in.Truncations...)
	return out
}

func cloneTransfers(in []Transfer) []Transfer {
	out := make([]Transfer, len(in))
	for i, t := range in {
		out[i] = t
		out[i].Energy = cloneBandEnergy(t.Energy)
		out[i].QuantizationResidual = cloneBandEnergy(t.QuantizationResidual)
	}
	return out
}

func sourceByID(level *Level, id string) (*Source, bool) {
	for i := range level.Sources {
		if level.Sources[i].ID == id {
			return &level.Sources[i], true
		}
	}
	return nil, false
}

func listenerByChunk(level *Level, chunkID int) (*Listener, bool) {
	for i := range level.Listeners {
		if level.Listeners[i].ChunkID == chunkID {
			return &level.Listeners[i], true
		}
	}
	return nil, false
}

func sourceByChunk(level *Level, chunkID int) (*Source, bool) {
	for i := range level.Sources {
		if level.Sources[i].ChunkID == chunkID {
			return &level.Sources[i], true
		}
	}
	return nil, false
}
