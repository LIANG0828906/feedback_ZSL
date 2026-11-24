package model

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Axis indices are physical axes, independent of the storage axis order.
const (
	AxisX = 0
	AxisY = 1
	AxisZ = 2
)

var axisNames = [3]string{"X", "Y", "Z"}

type Plane string

const (
	PlaneAxial    Plane = "axial"
	PlaneCoronal  Plane = "coronal"
	PlaneSagittal Plane = "sagittal"
	StorageFormat       = "volumelab.volume/v1"
)

var AllPlanes = []Plane{PlaneAxial, PlaneCoronal, PlaneSagittal}

func AxisName(axis int) string {
	if axis < 0 || axis >= len(axisNames) {
		return fmt.Sprintf("axis[%d]", axis)
	}
	return axisNames[axis]
}

func AxisFromName(name string) (int, bool) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "X":
		return AxisX, true
	case "Y":
		return AxisY, true
	case "Z":
		return AxisZ, true
	default:
		return 0, false
	}
}

type Vec3 [3]float64
type IVec3 [3]int

func (v Vec3) Add(o Vec3) Vec3 {
	return Vec3{v[0] + o[0], v[1] + o[1], v[2] + o[2]}
}

func (v Vec3) Sub(o Vec3) Vec3 {
	return Vec3{v[0] - o[0], v[1] - o[1], v[2] - o[2]}
}

func (v Vec3) Mul(s float64) Vec3 {
	return Vec3{v[0] * s, v[1] * s, v[2] * s}
}

func (v Vec3) Dot(o Vec3) float64 {
	return v[0]*o[0] + v[1]*o[1] + v[2]*o[2]
}

func (v Vec3) Norm() float64 {
	return math.Sqrt(v.Dot(v))
}

func (v Vec3) String() string {
	return fmt.Sprintf("(%.6f, %.6f, %.6f)", v[0], v[1], v[2])
}

func (v IVec3) Float() Vec3 {
	return Vec3{float64(v[0]), float64(v[1]), float64(v[2])}
}

type Window struct {
	Width float64 `json:"width"`
	Level float64 `json:"level"`
}

type PlaneWindow struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

func WindowFromRange(widthSpan float64) Window {
	return Window{Width: widthSpan, Level: 0}
}

type PlaneInfo struct {
	Plane    Plane   `json:"plane"`
	Normal   int     `json:"normalAxis"`
	U        int     `json:"uAxis"`
	V        int     `json:"vAxis"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	NormalMM float64 `json:"normalSpacingMM"`
	UMM      float64 `json:"uSpacingMM"`
	VMM      float64 `json:"vSpacingMM"`
}

func PlaneGeometry(plane Plane, shape [3]int, spacing [3]float64) (PlaneInfo, bool) {
	var normal, u, v int
	switch plane {
	case PlaneAxial:
		normal, u, v = AxisZ, AxisX, AxisY
	case PlaneCoronal:
		normal, u, v = AxisY, AxisX, AxisZ
	case PlaneSagittal:
		normal, u, v = AxisX, AxisY, AxisZ
	default:
		return PlaneInfo{}, false
	}
	return PlaneInfo{
		Plane:    plane,
		Normal:   normal,
		U:        u,
		V:        v,
		Width:    shape[u],
		Height:   shape[v],
		NormalMM: spacing[normal],
		UMM:      spacing[u],
		VMM:      spacing[v],
	}, true
}

func PlaneFromString(s string) (Plane, bool) {
	p := Plane(strings.ToLower(strings.TrimSpace(s)))
	switch p {
	case PlaneAxial, PlaneCoronal, PlaneSagittal:
		return p, true
	default:
		return "", false
	}
}

type PointReading struct {
	World Vec3 `json:"world"`
	Voxel Vec3 `json:"voxel"`
}

type Annotation struct {
	ID              string       `json:"id"`
	Plane           Plane        `json:"plane"`
	EndpointA       PointReading `json:"endpointA"`
	EndpointB       PointReading `json:"endpointB"`
	WorldLengthMM   float64      `json:"worldLengthMM"`
	VoxelLength     float64      `json:"voxelLength"`
	Interpolation   int          `json:"interpolationOrder"`
	CreatedAtSeq    uint64       `json:"createdAtSequence"`
	StructureID     string       `json:"structureId,omitempty"`
	MeasurementMode string       `json:"measurementMode,omitempty"`
}

type Structure struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Center      *Vec3    `json:"center,omitempty"`
	Radius      *float64 `json:"radius,omitempty"`
	Start       *Vec3    `json:"start,omitempty"`
	End         *Vec3    `json:"end,omitempty"`
	Min         *Vec3    `json:"min,omitempty"`
	Max         *Vec3    `json:"max,omitempty"`
	BoundingBox [2]Vec3  `json:"boundingBox"`
	Label       string   `json:"label"`
}

type VolumeSummary struct {
	ID                   string      `json:"id"`
	File                 string      `json:"file"`
	AxisOrder            string      `json:"axisOrder"`
	Dimensions           [3]int      `json:"dimensions"`
	Spacing              [3]float64  `json:"spacing"`
	Slope                float64     `json:"slope"`
	Intercept            float64     `json:"intercept"`
	LayerCount           int         `json:"layerCount"`
	GrayRange            [2]float64  `json:"grayRange"`
	ContentHash          string      `json:"contentHash"`
	Structures           []Structure `json:"structures"`
	PrecisionToleranceMM float64     `json:"precisionToleranceMM"`
}

type AnnotationRequest struct {
	Plane            Plane  `json:"plane"`
	A                Vec3   `json:"a"`
	B                Vec3   `json:"b"`
	CoordinateSystem string `json:"coordinateSystem"`
	StructureID      string `json:"structureId,omitempty"`
	MeasurementMode  string `json:"measurementMode,omitempty"`
}

type PrecisionReport struct {
	Pass               bool     `json:"pass"`
	AnnotationID       string   `json:"annotationId"`
	StructureID        string   `json:"structureId"`
	StructureType      string   `json:"structureType"`
	Mode               string   `json:"mode"`
	MeasuredLengthMM   float64  `json:"measuredLengthMM"`
	TruthLengthMM      float64  `json:"truthLengthMM"`
	DeviationMM        float64  `json:"deviationMM"`
	UpperBoundMM       float64  `json:"upperBoundMM"`
	ToleranceMM        float64  `json:"toleranceMM"`
	BoundaryResidualMM float64  `json:"boundaryResidualMM"`
	Formula            string   `json:"formula"`
	Inputs             []string `json:"inputs"`
}

// AppError is the stable, machine-readable error contract exposed by the API.
type AppError struct {
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Field    string         `json:"field,omitempty"`
	Axis     string         `json:"axis,omitempty"`
	Endpoint int            `json:"endpoint,omitempty"`
	Actual   any            `json:"actual,omitempty"`
	Allowed  any            `json:"allowed,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

func (e *AppError) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Code + ": " + e.Message
	if e.Field != "" {
		msg += fmt.Sprintf(" [field=%s]", e.Field)
	}
	if e.Axis != "" {
		msg += fmt.Sprintf(" [axis=%s]", e.Axis)
	}
	if e.Endpoint != 0 {
		msg += fmt.Sprintf(" [endpoint=%d]", e.Endpoint)
	}
	if e.Actual != nil {
		msg += fmt.Sprintf(" [actual=%v]", e.Actual)
	}
	if e.Allowed != nil {
		msg += fmt.Sprintf(" [allowed=%v]", e.Allowed)
	}
	return msg
}

func NewError(code, message string) *AppError {
	return &AppError{Code: code, Message: message}
}

func AsAppError(err error) *AppError {
	if err == nil {
		return nil
	}
	if appErr, ok := err.(*AppError); ok {
		return appErr
	}
	return &AppError{Code: "E_INTERNAL", Message: err.Error()}
}

func ErrorCode(err error) string {
	if appErr, ok := err.(*AppError); ok {
		return appErr.Code
	}
	return "E_INTERNAL"
}

func NormalizeVec(v Vec3) Vec3 {
	return Vec3{normalizeZero(v[0]), normalizeZero(v[1]), normalizeZero(v[2])}
}

func normalizeZero(v float64) float64 {
	if math.Abs(v) < 1e-12 {
		return 0
	}
	return v
}

func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s Structure) MarshalJSON() ([]byte, error) {
	type alias Structure
	return json.Marshal(alias(s))
}
