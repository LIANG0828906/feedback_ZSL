package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	AlgorithmVersion    = "maskqc-engine/1"
	ConversionVersion   = "maskqc-conversion/1"
	DefaultHardness     = 70
	DefaultDirection    = DirectionUnknown
	DefaultComposite    = 1
	MaxHardness         = 100
	MaxCoverage         = 10000
	CoverageScale       = 10000
	ForegroundThreshold = 128
)

type SchemaVersion string

const (
	SchemaV1 SchemaVersion = "v1"
	SchemaV2 SchemaVersion = "v2"
	SchemaV3 SchemaVersion = "v3"
)

func (v SchemaVersion) Rank() int {
	switch v {
	case SchemaV1:
		return 1
	case SchemaV2:
		return 2
	case SchemaV3:
		return 3
	default:
		return 0
	}
}

func ParseSchemaVersion(raw string) (SchemaVersion, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "v1", "1", "1.0":
		return SchemaV1, true
	case "v2", "2", "2.0":
		return SchemaV2, true
	case "v3", "3", "3.0":
		return SchemaV3, true
	default:
		return "", false
	}
}

type Direction string

const (
	DirectionUnknown       Direction = "unknown"
	DirectionEast          Direction = "east"
	DirectionSouthEast     Direction = "southeast"
	DirectionSouth         Direction = "south"
	DirectionSouthWest     Direction = "southwest"
	DirectionWest          Direction = "west"
	DirectionNorthWest     Direction = "northwest"
	DirectionNorth         Direction = "north"
	DirectionNorthEast     Direction = "northeast"
	DirectionCoDirectional Direction = "co_directional"
	DirectionCounter       Direction = "counter_directional"
	DirectionPerpendicular Direction = "perpendicular"
)

func ParseDirection(raw string) (Direction, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "unknown", "none":
		return DirectionUnknown, true
	case "east", "right", "ltr", "left-right":
		return DirectionEast, true
	case "southeast", "down-right", "se":
		return DirectionSouthEast, true
	case "south", "down", "ttb", "top-bottom":
		return DirectionSouth, true
	case "southwest", "down-left", "sw":
		return DirectionSouthWest, true
	case "west", "left", "rtl", "right-left":
		return DirectionWest, true
	case "northwest", "up-left", "nw":
		return DirectionNorthWest, true
	case "north", "up", "btt", "bottom-top":
		return DirectionNorth, true
	case "northeast", "up-right", "ne":
		return DirectionNorthEast, true
	case "co_directional", "co-directional", "with":
		return DirectionCoDirectional, true
	case "counter_directional", "counter-directional", "against":
		return DirectionCounter, true
	case "perpendicular", "across":
		return DirectionPerpendicular, true
	default:
		return "", false
	}
}

type Rect struct {
	MinX int `json:"minX"`
	MinY int `json:"minY"`
	MaxX int `json:"maxX"`
	MaxY int `json:"maxY"`
}

func (r Rect) Empty() bool {
	return r.MaxX < r.MinX || r.MaxY < r.MinY
}

func (r Rect) Width() int {
	if r.Empty() {
		return 0
	}
	return r.MaxX - r.MinX + 1
}

func (r Rect) Height() int {
	if r.Empty() {
		return 0
	}
	return r.MaxY - r.MinY + 1
}

func (r Rect) Intersects(other Rect) bool {
	if r.Empty() || other.Empty() {
		return false
	}
	return r.MinX <= other.MaxX && other.MinX <= r.MaxX && r.MinY <= other.MaxY && other.MinY <= r.MaxY
}

func (r Rect) Intersect(other Rect) Rect {
	if !r.Intersects(other) {
		return Rect{MinX: 0, MinY: 0, MaxX: -1, MaxY: -1}
	}
	return Rect{
		MinX: max(r.MinX, other.MinX),
		MinY: max(r.MinY, other.MinY),
		MaxX: min(r.MaxX, other.MaxX),
		MaxY: min(r.MaxY, other.MaxY),
	}
}

func (r Rect) Union(other Rect) Rect {
	if r.Empty() {
		return other
	}
	if other.Empty() {
		return r
	}
	return Rect{
		MinX: min(r.MinX, other.MinX),
		MinY: min(r.MinY, other.MinY),
		MaxX: max(r.MaxX, other.MaxX),
		MaxY: max(r.MaxY, other.MaxY),
	}
}

func (r Rect) Clip(width, height int) Rect {
	out := r
	out.MinX = max(0, min(width, out.MinX))
	out.MinY = max(0, min(height, out.MinY))
	out.MaxX = max(-1, min(width-1, out.MaxX))
	out.MaxY = max(-1, min(height-1, out.MaxY))
	if out.MaxX < out.MinX || out.MaxY < out.MinY {
		return Rect{MinX: 0, MinY: 0, MaxX: -1, MaxY: -1}
	}
	return out
}

type Brush struct {
	Radius   int `json:"radius"`
	Hardness int `json:"hardness"`
}

type Stroke struct {
	ID             string        `json:"id"`
	Seq            int           `json:"seq"`
	X0             int           `json:"x0"`
	Y0             int           `json:"y0"`
	X1             int           `json:"x1"`
	Y1             int           `json:"y1"`
	Radius         int           `json:"radius"`
	Hardness       int           `json:"hardness"`
	Weight         int           `json:"weight"`
	Priority       int           `json:"priority"`
	Direction      Direction     `json:"direction"`
	GroupID        string        `json:"groupId,omitempty"`
	CompositeOrder int           `json:"compositeOrder"`
	SourceVersion  SchemaVersion `json:"sourceVersion"`
}

func (s Stroke) Bounds() Rect {
	radius := max(0, s.Radius)
	return Rect{
		MinX: min(s.X0, s.X1) - radius,
		MinY: min(s.Y0, s.Y1) - radius,
		MaxX: max(s.X0, s.X1) + radius,
		MaxY: max(s.Y0, s.Y1) + radius,
	}
}

func (s Stroke) Signature() string {
	return HashStrings(
		s.ID,
		fmt.Sprint(s.Seq),
		fmt.Sprint(s.X0),
		fmt.Sprint(s.Y0),
		fmt.Sprint(s.X1),
		fmt.Sprint(s.Y1),
		fmt.Sprint(s.Radius),
		fmt.Sprint(s.Hardness),
		fmt.Sprint(s.Weight),
		fmt.Sprint(s.Priority),
		string(s.Direction),
		s.GroupID,
		fmt.Sprint(s.CompositeOrder),
	)
}

func (s Stroke) RasterSignature() string {
	return HashStrings(
		fmt.Sprint(s.X0),
		fmt.Sprint(s.Y0),
		fmt.Sprint(s.X1),
		fmt.Sprint(s.Y1),
		fmt.Sprint(s.Radius),
		fmt.Sprint(s.Hardness),
		fmt.Sprint(s.Weight),
	)
}

type StrokeGroup struct {
	ID             string   `json:"id"`
	CompositeOrder int      `json:"compositeOrder"`
	StrokeIDs      []string `json:"strokeIds"`
}

type TruthSegment struct {
	X         int       `json:"x"`
	Y         int       `json:"y"`
	NX        int       `json:"nx"`
	NY        int       `json:"ny"`
	Normal    Direction `json:"normal"`
	Direction Direction `json:"direction"`
}

type DomainTruth struct {
	ID               string         `json:"id"`
	CentroidX        float64        `json:"centroidX"`
	CentroidY        float64        `json:"centroidY"`
	Area             int            `json:"area"`
	BBox             Rect           `json:"bbox"`
	Holes            int            `json:"holes"`
	RingWidth        int            `json:"ringWidth"`
	Perimeter        int            `json:"perimeter"`
	ReferenceMask    []byte         `json:"referenceMask,omitempty"`
	Direction        Direction      `json:"direction"`
	BoundarySegments []TruthSegment `json:"boundarySegments,omitempty"`
}

type Project struct {
	ID                 string        `json:"id"`
	Version            SchemaVersion `json:"version"`
	Seed               int64         `json:"seed"`
	Width              int           `json:"width"`
	Height             int           `json:"height"`
	CandidateMask      []byte        `json:"candidateMask,omitempty"`
	Strokes            []Stroke      `json:"strokes"`
	Groups             []StrokeGroup `json:"groups,omitempty"`
	Truth              []DomainTruth `json:"truth"`
	CompositeOrderHint int           `json:"compositeOrderHint,omitempty"`
}

func (p *Project) Normalize() {
	for i := range p.Strokes {
		if p.Strokes[i].SourceVersion == "" {
			p.Strokes[i].SourceVersion = p.Version
		}
		if p.Strokes[i].Hardness == 0 {
			p.Strokes[i].Hardness = DefaultHardness
		}
		if p.Strokes[i].CompositeOrder == 0 {
			p.Strokes[i].CompositeOrder = DefaultComposite
		}
		if p.Strokes[i].Direction == "" {
			p.Strokes[i].Direction = DefaultDirection
		}
	}
	sort.SliceStable(p.Strokes, func(i, j int) bool { return p.Strokes[i].Seq < p.Strokes[j].Seq })
}

func (p Project) StrokeByID(id string) (Stroke, bool) {
	for _, stroke := range p.Strokes {
		if stroke.ID == id {
			return stroke, true
		}
	}
	return Stroke{}, false
}

func (p Project) StrokeIDsInOrder() []string {
	ordered := OrderedStrokes(p.Strokes)
	ids := make([]string, len(ordered))
	for i := range ordered {
		ids[i] = ordered[i].ID
	}
	return ids
}

func OrderedStrokes(strokes []Stroke) []Stroke {
	out := append([]Stroke(nil), strokes...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		if out[i].CompositeOrder != out[j].CompositeOrder {
			return out[i].CompositeOrder < out[j].CompositeOrder
		}
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func OrderVersion(strokes []Stroke) string {
	parts := make([]string, 0, len(strokes)*4)
	for _, stroke := range OrderedStrokes(strokes) {
		parts = append(parts, stroke.ID, fmt.Sprint(stroke.Priority), fmt.Sprint(stroke.CompositeOrder), fmt.Sprint(stroke.Seq))
	}
	return HashStrings(parts...)
}

type ConversionTrace struct {
	Field         string        `json:"field"`
	ObjectID      string        `json:"objectId"`
	From          string        `json:"from"`
	To            string        `json:"to"`
	OldValue      string        `json:"oldValue,omitempty"`
	DefaultValue  string        `json:"defaultValue,omitempty"`
	DefaultSource string        `json:"defaultSource,omitempty"`
	Handling      string        `json:"handling,omitempty"`
	OperandA      int64         `json:"operandA"`
	OperandB      int64         `json:"operandB"`
	Quotient      int64         `json:"quotient"`
	Remainder     int64         `json:"remainder"`
	Result        int64         `json:"result"`
	Rounding      string        `json:"rounding"`
	Source        SchemaVersion `json:"sourceVersion"`
	Target        SchemaVersion `json:"targetVersion"`
}

type ConversionReport struct {
	Traces         []ConversionTrace `json:"traces"`
	ResidualDigest string            `json:"residualDigest"`
	ParamsVersion  string            `json:"paramsVersion"`
}

func (r *ConversionReport) Finish() {
	parts := make([]string, 0, len(r.Traces))
	for _, trace := range r.Traces {
		parts = append(parts, fmt.Sprintf("%s:%d:%d:%d", trace.Field, trace.OperandA, trace.OperandB, trace.Remainder))
	}
	r.ResidualDigest = HashStrings(parts...)
	r.ParamsVersion = ConversionVersion
}

const (
	CodeSchema    = "E_SCHEMA"
	CodeLossy     = "E_LOSSY"
	CodeOverflow  = "E_OVF"
	CodeParam     = "E_PARA"
	CodeTopology  = "E_TOP"
	CodeDirection = "E_DIR"
	CodeBudget    = "E_BUDGET"
)

type Error struct {
	Code          string `json:"code"`
	Path          string `json:"path,omitempty"`
	ObjectID      string `json:"objectId,omitempty"`
	Field         string `json:"field,omitempty"`
	Message       string `json:"message"`
	OldValue      string `json:"oldValue,omitempty"`
	DefaultValue  string `json:"defaultValue,omitempty"`
	DefaultSource string `json:"defaultSource,omitempty"`
	Handling      string `json:"handling,omitempty"`
	OperandA      string `json:"operandA,omitempty"`
	OperandB      string `json:"operandB,omitempty"`
	Allowed       string `json:"allowed,omitempty"`
	Domain        int    `json:"domain,omitempty"`
	Measured      string `json:"measured,omitempty"`
}

func (e *Error) Error() string {
	parts := []string{e.Code}
	if e.ObjectID != "" {
		parts = append(parts, "object="+e.ObjectID)
	}
	if e.Path != "" {
		parts = append(parts, "path="+e.Path)
	}
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.OperandA != "" || e.OperandB != "" {
		parts = append(parts, "operands="+e.OperandA+","+e.OperandB)
	}
	if e.Allowed != "" {
		parts = append(parts, "allowed="+e.Allowed)
	}
	return strings.Join(parts, ": ")
}

func NewError(code, path, objectID, message string) *Error {
	return &Error{Code: code, Path: path, ObjectID: objectID, Message: message}
}

type Errors []*Error

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i := range e {
		parts[i] = e[i].Error()
	}
	return strings.Join(parts, "; ")
}

func HasCode(err error, code string) bool {
	switch typed := err.(type) {
	case *Error:
		return typed.Code == code
	case Errors:
		for _, item := range typed {
			if item.Code == code {
				return true
			}
		}
	}
	return false
}

func IsVerificationError(err error) bool {
	if err == nil {
		return false
	}
	switch typed := err.(type) {
	case *Error:
		return typed.Code == CodeTopology || typed.Code == CodeDirection
	case Errors:
		if len(typed) == 0 {
			return false
		}
		for _, item := range typed {
			if item.Code != CodeTopology && item.Code != CodeDirection {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashStrings(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func HashJSON(value any) string {
	data, _ := json.Marshal(value)
	return HashBytes(data)
}
