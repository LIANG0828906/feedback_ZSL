package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
)

const (
	VersionPLYASCII  = "ply-ascii/v1"
	VersionPLYBinary = "ply-binary/v1"
	VersionPCBin     = "pcbin/v1"
	AlgorithmVersion = "strata-local-fit/1.4.0"
	CanonicalStep    = 0.001
)

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3 {
	return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z}
}

func (v Vec3) Sub(o Vec3) Vec3 {
	return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z}
}

func (v Vec3) Mul(s float64) Vec3 {
	return Vec3{v.X * s, v.Y * s, v.Z * s}
}

func (v Vec3) Dot(o Vec3) float64 {
	return v.X*o.X + v.Y*o.Y + v.Z*o.Z
}

func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		X: v.Y*o.Z - v.Z*o.Y,
		Y: v.Z*o.X - v.X*o.Z,
		Z: v.X*o.Y - v.Y*o.X,
	}
}

func (v Vec3) Length() float64 {
	return math.Sqrt(v.Dot(v))
}

func (v Vec3) Normalize() Vec3 {
	l := v.Length()
	if l < 1e-12 {
		return Vec3{Z: 1}
	}
	return v.Mul(1 / l)
}

type Bounds struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

func EmptyBounds() Bounds {
	return Bounds{
		Min: Vec3{math.Inf(1), math.Inf(1), math.Inf(1)},
		Max: Vec3{math.Inf(-1), math.Inf(-1), math.Inf(-1)},
	}
}

func (b Bounds) IsEmpty() bool {
	return b.Min.X > b.Max.X || b.Min.Y > b.Max.Y || b.Min.Z > b.Max.Z
}

func (b *Bounds) Include(p Vec3) {
	if p.X < b.Min.X {
		b.Min.X = p.X
	}
	if p.Y < b.Min.Y {
		b.Min.Y = p.Y
	}
	if p.Z < b.Min.Z {
		b.Min.Z = p.Z
	}
	if p.X > b.Max.X {
		b.Max.X = p.X
	}
	if p.Y > b.Max.Y {
		b.Max.Y = p.Y
	}
	if p.Z > b.Max.Z {
		b.Max.Z = p.Z
	}
}

func (b *Bounds) Union(o Bounds) {
	if o.IsEmpty() {
		return
	}
	b.Include(o.Min)
	b.Include(o.Max)
}

func (b Bounds) Expanded(amount float64) Bounds {
	return Bounds{
		Min: Vec3{b.Min.X - amount, b.Min.Y - amount, b.Min.Z - amount},
		Max: Vec3{b.Max.X + amount, b.Max.Y + amount, b.Max.Z + amount},
	}
}

func (b Bounds) ContainsXY(p Vec3) bool {
	return p.X >= b.Min.X && p.X <= b.Max.X && p.Y >= b.Min.Y && p.Y <= b.Max.Y
}

func (b Bounds) Center() Vec3 {
	return Vec3{(b.Min.X + b.Max.X) * 0.5, (b.Min.Y + b.Max.Y) * 0.5, (b.Min.Z + b.Max.Z) * 0.5}
}

func (b Bounds) Size() Vec3 {
	return Vec3{b.Max.X - b.Min.X, b.Max.Y - b.Min.Y, b.Max.Z - b.Min.Z}
}

func (b Bounds) Within(outer Bounds, eps float64) bool {
	return b.Min.X >= outer.Min.X-eps && b.Min.Y >= outer.Min.Y-eps && b.Min.Z >= outer.Min.Z-eps &&
		b.Max.X <= outer.Max.X+eps && b.Max.Y <= outer.Max.Y+eps && b.Max.Z <= outer.Max.Z+eps
}

type Point struct {
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Z         float64 `json:"z"`
	NX        float64 `json:"nx,omitempty"`
	NY        float64 `json:"ny,omitempty"`
	NZ        float64 `json:"nz,omitempty"`
	R         uint8   `json:"r,omitempty"`
	G         uint8   `json:"g,omitempty"`
	B         uint8   `json:"b,omitempty"`
	Intensity float64 `json:"intensity,omitempty"`
}

func (p Point) Position() Vec3 {
	return Vec3{p.X, p.Y, p.Z}
}

func (p Point) HasNormal() bool {
	return p.NX != 0 || p.NY != 0 || p.NZ != 0
}

func QuantizeCoordinate(v float64) float64 {
	if math.Abs(v) < CanonicalStep*0.5 {
		return 0
	}
	return math.Round(v/CanonicalStep) * CanonicalStep
}

func CanonicalizePoints(points []Point) {
	for i := range points {
		points[i].X = QuantizeCoordinate(points[i].X)
		points[i].Y = QuantizeCoordinate(points[i].Y)
		points[i].Z = QuantizeCoordinate(points[i].Z)
	}
	sort.Slice(points, func(i, j int) bool {
		a, b := points[i], points[j]
		if a.X != b.X {
			return a.X < b.X
		}
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		if a.Z != b.Z {
			return a.Z < b.Z
		}
		if a.Intensity != b.Intensity {
			return a.Intensity < b.Intensity
		}
		return a.R < b.R
	})
}

type FieldSpec struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Path     string `json:"path"`
	Offset   int64  `json:"offset"`
	Optional bool   `json:"optional"`
}

type Quantization struct {
	Kind string    `json:"kind"`
	Min  []float64 `json:"min,omitempty"`
	Max  []float64 `json:"max,omitempty"`
}

type HeaderMeta struct {
	Seed         int               `json:"seed,omitempty"`
	Generator    string            `json:"generator,omitempty"`
	SourceNote   string            `json:"sourceNote,omitempty"`
	Custom       map[string]string `json:"custom,omitempty"`
	HeaderBytes  int               `json:"headerBytes"`
	RecordBytes  int               `json:"recordBytes"`
	PayloadBytes int               `json:"payloadBytes"`
	Checksum     string            `json:"checksum,omitempty"`
}

type SourceMeta struct {
	VersionID          string       `json:"versionId"`
	VersionNumber      int          `json:"versionNumber"`
	DisplayName        string       `json:"displayName"`
	Format             string       `json:"format"`
	Encoding           string       `json:"encoding"`
	File               string       `json:"file"`
	FileBytes          int64        `json:"fileBytes"`
	PointCount         int          `json:"pointCount"`
	FaceCount          int          `json:"faceCount"`
	Bounds             Bounds       `json:"bounds"`
	FieldOrder         []string     `json:"fieldOrder"`
	OptionalAttributes []string     `json:"optionalAttributes"`
	Fields             []FieldSpec  `json:"fields"`
	Quantization       Quantization `json:"quantization"`
	HeaderMeta         HeaderMeta   `json:"headerMeta"`
	HasNormals         bool         `json:"hasNormals"`
	HasColors          bool         `json:"hasColors"`
	HasIntensity       bool         `json:"hasIntensity"`
}

type Face struct {
	A int `json:"a"`
	B int `json:"b"`
	C int `json:"c"`
}

type Geometry struct {
	Points []Point
	Faces  []Face
	Meta   SourceMeta
}

type MeshVertex struct {
	Position  Vec3    `json:"position"`
	Normal    Vec3    `json:"normal"`
	Color     [3]byte `json:"color"`
	Intensity float64 `json:"intensity"`
}

type MeshFace struct {
	A     int    `json:"a"`
	B     int    `json:"b"`
	C     int    `json:"c"`
	Chunk string `json:"chunk"`
}

type ChunkMesh struct {
	ID            string `json:"id"`
	Row           int    `json:"row"`
	Col           int    `json:"col"`
	Bounds        Bounds `json:"bounds"`
	VertexStart   int    `json:"vertexStart"`
	VertexCount   int    `json:"vertexCount"`
	FaceStart     int    `json:"faceStart"`
	FaceCount     int    `json:"faceCount"`
	PointIndices  []int  `json:"pointIndices"`
	ContentKey    string `json:"contentKey,omitempty"`
	ContentHash   string `json:"contentHash,omitempty"`
	ParamsVersion int    `json:"paramsVersion"`
	GridN         int    `json:"gridN"`
	Rebuilt       bool   `json:"rebuilt"`
	Reused        bool   `json:"reused"`
	Reason        string `json:"reason"`
	Error         string `json:"error,omitempty"`
}

type Mesh struct {
	Vertices        []MeshVertex `json:"vertices"`
	Faces           []MeshFace   `json:"faces"`
	Chunks          []ChunkMesh  `json:"chunks"`
	Bounds          Bounds       `json:"bounds"`
	ParamsHash      string       `json:"paramsHash"`
	ParamsVersion   int          `json:"paramsVersion"`
	Algorithm       string       `json:"algorithm"`
	BuildMode       string       `json:"buildMode"`
	SampleRate      float64      `json:"sampleRate"`
	ContentHash     string       `json:"contentHash"`
	RequestedChunks []string     `json:"requestedChunks,omitempty"`
}

type FeatureSet struct {
	Peak      Vec3 `json:"peak"`
	Trough    Vec3 `json:"trough"`
	Ridge     Vec3 `json:"ridge"`
	MinCorner Vec3 `json:"minCorner"`
	MaxCorner Vec3 `json:"maxCorner"`
}

type ChunkTruth struct {
	ID               string `json:"id"`
	Row              int    `json:"row"`
	Col              int    `json:"col"`
	Bounds           Bounds `json:"bounds"`
	ExpectedVertices int    `json:"expectedVertices"`
	ExpectedFaces    int    `json:"expectedFaces"`
}

type Truth struct {
	Seed             int          `json:"seed"`
	SeedName         string       `json:"seedName"`
	Surface          string       `json:"surface"`
	GridWidth        int          `json:"gridWidth"`
	GridHeight       int          `json:"gridHeight"`
	SourcePoints     int          `json:"sourcePoints"`
	PipelinePoints   int          `json:"pipelinePoints"`
	Outliers         int          `json:"outliers"`
	ExpectedFaces    int          `json:"expectedFaces"`
	SourceBounds     Bounds       `json:"sourceBounds"`
	CleanBounds      Bounds       `json:"cleanBounds"`
	MeshBounds       Bounds       `json:"meshBounds"`
	Features         FeatureSet   `json:"features"`
	Chunks           []ChunkTruth `json:"chunks"`
	FeatureTolerance float64      `json:"featureTolerance"`
	BoundsExpansion  float64      `json:"boundsExpansion"`
}

type VersionInfo struct {
	ID                 string       `json:"id"`
	Number             int          `json:"number"`
	DisplayName        string       `json:"displayName"`
	Short              string       `json:"short"`
	Format             string       `json:"format"`
	Encoding           string       `json:"encoding"`
	File               string       `json:"file"`
	FileBytes          int64        `json:"fileBytes"`
	PointCount         int          `json:"pointCount"`
	FieldOrder         []string     `json:"fieldOrder"`
	OptionalAttributes []string     `json:"optionalAttributes"`
	Quantization       Quantization `json:"quantization"`
	HasNormals         bool         `json:"hasNormals"`
	HasColors          bool         `json:"hasColors"`
	HasIntensity       bool         `json:"hasIntensity"`
}

func HashBytes(chunks ...[]byte) string {
	h := sha256.New()
	for _, chunk := range chunks {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(chunk)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(chunk)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func EncodeVec3(v Vec3) []byte {
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint64(buf[0:8], math.Float64bits(v.X))
	binary.LittleEndian.PutUint64(buf[8:16], math.Float64bits(v.Y))
	binary.LittleEndian.PutUint64(buf[16:24], math.Float64bits(v.Z))
	return buf
}

func EncodeInts(values []int) []byte {
	buf := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
	}
	return buf
}

func DecodeInts(data []byte) ([]int, error) {
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("integer payload length %d is not divisible by 4", len(data))
	}
	out := make([]int, len(data)/4)
	for i := range out {
		out[i] = int(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return out, nil
}

func EncodeMeshPayload(vertices []MeshVertex, faces []MeshFace) []byte {
	buf := make([]byte, 0, len(vertices)*64+len(faces)*16)
	for _, v := range vertices {
		buf = append(buf, EncodeVec3(v.Position)...)
		buf = append(buf, EncodeVec3(v.Normal)...)
		buf = append(buf, v.Color[0], v.Color[1], v.Color[2])
		var f [8]byte
		binary.LittleEndian.PutUint64(f[:], math.Float64bits(v.Intensity))
		buf = append(buf, f[:]...)
	}
	for _, f := range faces {
		var rec [16]byte
		binary.LittleEndian.PutUint32(rec[0:4], uint32(f.A))
		binary.LittleEndian.PutUint32(rec[4:8], uint32(f.B))
		binary.LittleEndian.PutUint32(rec[8:12], uint32(f.C))
		binary.LittleEndian.PutUint32(rec[12:16], uint32(len(f.Chunk)))
		buf = append(buf, rec[:]...)
		buf = append(buf, f.Chunk...)
	}
	return buf
}

func HashMesh(vertices []MeshVertex, faces []MeshFace, chunks []ChunkMesh) string {
	h := sha256.New()
	for _, v := range vertices {
		_, _ = h.Write(EncodeVec3(v.Position))
		_, _ = h.Write(EncodeVec3(v.Normal))
	}
	for _, f := range faces {
		_, _ = h.Write(EncodeInts([]int{f.A, f.B, f.C}))
	}
	for _, c := range chunks {
		_, _ = h.Write([]byte(c.ID))
	}
	return hex.EncodeToString(h.Sum(nil))
}
