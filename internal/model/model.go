package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	CodeSchema   = "E_SCHEMA"
	CodeMeta     = "E_META"
	CodeBudget   = "E_BUDGET"
	CodeAttr     = "E_ATTR"
	CodePara     = "E_PARA"
	CodeDiff     = "E_DIFF"
	CodeResource = "E_RESOURCE"
	CodeInternal = "E_INTERNAL"

	SchemaVersion    = "terrain-sample/v1"
	AlgorithmVersion = "recursive-triangle-v1.0.0"
	FixedScale       = int64(1000)
)

type AppError struct {
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	TileID   string         `json:"tileId,omitempty"`
	Boundary string         `json:"boundaryId,omitempty"`
	Field    string         `json:"field,omitempty"`
	Actual   any            `json:"actual,omitempty"`
	Limit    any            `json:"limit,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

func (e *AppError) Error() string {
	if e == nil {
		return ""
	}
	where := ""
	if e.TileID != "" {
		where = " tile=" + e.TileID
	}
	if e.Boundary != "" {
		where += " boundary=" + e.Boundary
	}
	if e.Field != "" {
		where += " field=" + e.Field
	}
	return fmt.Sprintf("%s%s%s: %s", e.Code, where, func() string {
		if where != "" {
			return ""
		}
		return ""
	}(), e.Message)
}

func NewError(code, message string) *AppError {
	return &AppError{Code: code, Message: message}
}

func (e *AppError) WithTile(id, field string) *AppError {
	e.TileID = id
	e.Field = field
	return e
}

func (e *AppError) WithBoundary(id, field string) *AppError {
	e.Boundary = id
	e.Field = field
	return e
}

func (e *AppError) WithActual(actual any) *AppError {
	e.Actual = actual
	return e
}

func (e *AppError) WithLimit(limit any) *AppError {
	e.Limit = limit
	return e
}

func (e *AppError) WithDetails(d map[string]any) *AppError {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	for k, v := range d {
		e.Details[k] = v
	}
	return e
}

type Grid struct {
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	CellSize int64     `json:"cellSize"`
	Heights  [][]int64 `json:"heights"`
}

type Coverage struct {
	MinX int64 `json:"minX"`
	MinY int64 `json:"minY"`
	MaxX int64 `json:"maxX"`
	MaxY int64 `json:"maxY"`
}

type Tile struct {
	ID           string   `json:"id"`
	Order        int      `json:"order"`
	OriginX      int64    `json:"originX"`
	OriginY      int64    `json:"originY"`
	Coverage     Coverage `json:"coverage"`
	SampleBudget int      `json:"sampleBudget"`
	Tolerance    int64    `json:"tolerance"`
	MaxTriangles int      `json:"maxTriangles"`
	ParamVersion int      `json:"paramVersion"`
	HeightMin    int64    `json:"heightMin"`
	HeightMax    int64    `json:"heightMax"`
	Grid         Grid     `json:"grid"`
}

type BoundaryColumn struct {
	Order int   `json:"order"`
	X     int64 `json:"x"`
	Y     int64 `json:"y"`
}

type BoundarySide struct {
	TileID  string           `json:"tileId"`
	Edge    string           `json:"edge"`
	Columns []BoundaryColumn `json:"columns"`
}

type Boundary struct {
	ID                     string       `json:"id"`
	Axis                   string       `json:"axis"`
	Value                  int64        `json:"value"`
	AngleThresholdMicroDeg int64        `json:"angleThresholdMicroDeg"`
	SideA                  BoundarySide `json:"sideA"`
	SideB                  BoundarySide `json:"sideB"`
	OrderSignature         string       `json:"orderSignature"`
}

type ExpectedHeight struct {
	TileID string `json:"tileId"`
	X      int64  `json:"x"`
	Y      int64  `json:"y"`
	Z      int64  `json:"z"`
}

type TriangleLimitTruth struct {
	TileID          string `json:"tileId"`
	MaxTriangles    int    `json:"maxTriangles"`
	ActualTriangles int    `json:"actualTriangles,omitempty"`
}

type FaultTruth struct {
	ID           string `json:"id"`
	BoundaryID   string `json:"boundaryId"`
	Category     string `json:"category"`
	ExpectedCode string `json:"expectedCode,omitempty"`
	X            int64  `json:"x"`
	Y            int64  `json:"y"`
	Note         string `json:"note,omitempty"`
}

type Truth struct {
	ExpectedHeights  []ExpectedHeight       `json:"expectedHeights"`
	TriangleLimits   []TriangleLimitTruth   `json:"triangleLimits"`
	AlignedSequences []AlignedSequenceTruth `json:"alignedSequences"`
	Faults           []FaultTruth           `json:"faults"`
}

type AlignedSequenceTruth struct {
	BoundaryID string           `json:"boundaryId"`
	Columns    []BoundaryColumn `json:"columns"`
}

type ProbePoint struct {
	Order int   `json:"order"`
	X     int64 `json:"x"`
	Y     int64 `json:"y"`
	Z     int64 `json:"z"`
}

type ProbeReference struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

type AttributionProbe struct {
	ID         string           `json:"id"`
	BoundaryID string           `json:"boundaryId"`
	Axis       string           `json:"axis"`
	ToleranceA int64            `json:"toleranceA"`
	ToleranceB int64            `json:"toleranceB"`
	Threshold  int64            `json:"angleThresholdMicroDeg"`
	SideA      []ProbePoint     `json:"sideA"`
	SideB      []ProbePoint     `json:"sideB"`
	ReferenceA []ProbeReference `json:"referenceA,omitempty"`
	ReferenceB []ProbeReference `json:"referenceB,omitempty"`
	Truth      FaultTruth       `json:"truth"`
}

type Project struct {
	SchemaVersion          string             `json:"schemaVersion"`
	Seed                   int64              `json:"seed"`
	FixedScale             int64              `json:"fixedScale"`
	AngleThresholdMicroDeg int64              `json:"angleThresholdMicroDeg"`
	Tiles                  []Tile             `json:"tiles"`
	Boundaries             []Boundary         `json:"boundaries"`
	Truth                  Truth              `json:"truth"`
	Probes                 []AttributionProbe `json:"probes,omitempty"`
	Dir                    string             `json:"-"`
}

func (p *Project) Tile(id string) *Tile {
	for i := range p.Tiles {
		if p.Tiles[i].ID == id {
			return &p.Tiles[i]
		}
	}
	return nil
}

func (p *Project) Boundary(id string) *Boundary {
	for i := range p.Boundaries {
		if p.Boundaries[i].ID == id {
			return &p.Boundaries[i]
		}
	}
	return nil
}

func SortedTileIDs(tiles []Tile) []string {
	ids := make([]string, len(tiles))
	for i, t := range tiles {
		ids[i] = t.ID
	}
	sort.SliceStable(ids, func(i, j int) bool {
		ai, bi := tileOrder(tiles, ids[i]), tileOrder(tiles, ids[j])
		if ai != bi {
			return ai < bi
		}
		return ids[i] < ids[j]
	})
	return ids
}

func tileOrder(tiles []Tile, id string) int {
	for _, t := range tiles {
		if t.ID == id {
			return t.Order
		}
	}
	return 1 << 30
}

type Vertex struct {
	ID int   `json:"id"`
	X  int64 `json:"x"`
	Y  int64 `json:"y"`
	Z  int64 `json:"z"`
}

type Triangle struct {
	ID int `json:"id"`
	V0 int `json:"v0"`
	V1 int `json:"v1"`
	V2 int `json:"v2"`
}

type TriangleError struct {
	TriangleID  int   `json:"triangleId"`
	MaxAbsError int64 `json:"maxAbsError"`
	L1Error     int64 `json:"l1Error"`
	SampleCount int   `json:"sampleCount"`
}

type Mesh struct {
	TileID         string          `json:"tileId"`
	ParamVersion   int             `json:"paramVersion"`
	Algorithm      string          `json:"algorithmVersion"`
	Depth          int             `json:"depth"`
	Vertices       []Vertex        `json:"vertices"`
	Triangles      []Triangle      `json:"triangles"`
	TriangleErrors []TriangleError `json:"triangleErrors"`
	MaxError       int64           `json:"maxError"`
}

func CanonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var sorted any
	if err := json.Unmarshal(b, &sorted); err != nil {
		panic(err)
	}
	out, err := json.Marshal(sorted)
	if err != nil {
		panic(err)
	}
	return out
}

func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func DigestJSON(v any) string {
	return SHA256Hex(CanonicalJSON(v))
}
