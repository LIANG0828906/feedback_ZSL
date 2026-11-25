package acoustics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	CodeSchema     = "E_SCHEMA"
	CodeGeom       = "E_GEOM"
	CodeState      = "E_STATE"
	CodeUnreach    = "E_UNREACH"
	CodeDiff       = "E_DIFF"
	CodeCheckpoint = "E_CHECKPOINT"

	LevelSchemaVersion = "level-v1"
	TruthSchemaVersion = "truth-v1"
	ParamVersion       = "acoustic-params-v1"
	AlgorithmVersion   = "fixed-mirror-v1"
)

// Point is an API/JSON coordinate in metres.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Rect is an axis-aligned rectangle in metres.
type Rect struct {
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
	X2 float64 `json:"x2"`
	Y2 float64 `json:"y2"`
}

func (r Rect) Min() Point { return Point{X: r.X1, Y: r.Y1} }
func (r Rect) Max() Point { return Point{X: r.X2, Y: r.Y2} }

type Room struct {
	ID   string `json:"id"`
	Rect Rect   `json:"rect"`
}

type Wall struct {
	ID    string `json:"id"`
	RoomA string `json:"room_a"`
	RoomB string `json:"room_b"`
	A     Point  `json:"a"`
	B     Point  `json:"b"`
}

type Door struct {
	ID     string  `json:"id"`
	WallID string  `json:"wall_id"`
	Center Point   `json:"center"`
	Width  float64 `json:"width_m"`
	Open   *bool   `json:"open,omitempty"`
}

func (d Door) IsOpen() bool { return d.Open == nil || *d.Open }

type Reflector struct {
	ID          string  `json:"id"`
	RoomID      string  `json:"room_id"`
	A           Point   `json:"a"`
	B           Point   `json:"b"`
	Normal      Point   `json:"normal"`
	Absorption  float64 `json:"absorption"`
	Description string  `json:"description,omitempty"`
}

type Source struct {
	ID       string  `json:"id"`
	RoomID   string  `json:"room_id"`
	Position Point   `json:"position"`
	PowerDB  float64 `json:"power_db"`
}

type Listener struct {
	ID       string `json:"id"`
	RoomID   string `json:"room_id"`
	Position Point  `json:"position"`
}

type Params struct {
	ParamVersion     string  `json:"param_version"`
	AlgorithmVersion string  `json:"algorithm_version"`
	CoordToleranceM  float64 `json:"coord_tolerance_m"`
	LengthToleranceM float64 `json:"length_tolerance_m"`
	SPLToleranceDB   float64 `json:"spl_tolerance_db"`
}

func DefaultParams() Params {
	return Params{
		ParamVersion:     ParamVersion,
		AlgorithmVersion: AlgorithmVersion,
		CoordToleranceM:  1e-4,
		LengthToleranceM: 2e-3,
		SPLToleranceDB:   0.05,
	}
}

type Level struct {
	SchemaVersion string      `json:"schema_version"`
	Seed          int64       `json:"seed"`
	Units         string      `json:"units"`
	Params        Params      `json:"params"`
	Rooms         []Room      `json:"rooms"`
	Walls         []Wall      `json:"walls"`
	Doors         []Door      `json:"doors"`
	Reflectors    []Reflector `json:"reflectors"`
	Sources       []Source    `json:"sources"`
	Listeners     []Listener  `json:"listeners"`
}

// Issue is one structured validation/diagnostic finding.
type Issue struct {
	Code     string `json:"code"`
	ObjectID string `json:"object_id"`
	Field    string `json:"field,omitempty"`
	Message  string `json:"message"`
}

func (i Issue) Error() string {
	return fmt.Sprintf("%s %s %s: %s", i.Code, i.ObjectID, i.Field, i.Message)
}

// DomainError carries a stable error code and optional structured details.
type DomainError struct {
	Code     string `json:"code"`
	ObjectID string `json:"object_id,omitempty"`
	Field    string `json:"field,omitempty"`
	Message  string `json:"message"`
	Details  any    `json:"details,omitempty"`
}

func (e *DomainError) Error() string {
	if e.ObjectID != "" {
		return fmt.Sprintf("%s[%s] %s: %s", e.Code, e.ObjectID, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func Errorf(code, objectID, field, format string, args ...any) *DomainError {
	return &DomainError{Code: code, ObjectID: objectID, Field: field, Message: fmt.Sprintf(format, args...)}
}

// ValidationError aggregates all schema/geometry findings.
type ValidationError struct {
	Issues []Issue `json:"issues"`
}

func (e *ValidationError) Error() string {
	if len(e.Issues) == 0 {
		return "validation failed"
	}
	return e.Issues[0].Error()
}

func (e *ValidationError) OrNil() error {
	if e == nil || len(e.Issues) == 0 {
		return nil
	}
	return e
}

func AsDomainError(err error) (*DomainError, bool) {
	var de *DomainError
	if errors.As(err, &de) {
		return de, true
	}
	var ve *ValidationError
	if errors.As(err, &ve) && len(ve.Issues) > 0 {
		return &DomainError{Code: ve.Issues[0].Code, ObjectID: ve.Issues[0].ObjectID, Field: ve.Issues[0].Field, Message: ve.Issues[0].Message, Details: ve.Issues}, true
	}
	return nil, false
}

// HashCanonical returns a stable SHA-256 hash of JSON-canonicalizable data.
func HashCanonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// All persisted/domain structures use JSON-safe fields.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (l Level) RoomIDs() []string {
	out := make([]string, 0, len(l.Rooms))
	for _, r := range l.Rooms {
		out = append(out, r.ID)
	}
	return out
}
