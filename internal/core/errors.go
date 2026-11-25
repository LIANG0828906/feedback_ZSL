package core

import (
	"encoding/json"
	"fmt"
)

const (
	CodePara    = "E_PARA"
	CodeDecode  = "E_DECODE"
	CodeRegion  = "E_REGION"
	CodeVersion = "E_VERSION"
	CodeDiverge = "E_DIVERGE"
	CodeState   = "E_STATE"
)

// AppError is the stable, machine-readable error envelope used by the CLI,
// HTTP diagnostics page, and tests.
type AppError struct {
	Code         string            `json:"code"`
	Message      string            `json:"message"`
	Field        string            `json:"field,omitempty"`
	Range        string            `json:"range,omitempty"`
	Snapshot     string            `json:"snapshot,omitempty"`
	Block        string            `json:"block,omitempty"`
	Layer        string            `json:"layer,omitempty"`
	From         string            `json:"from,omitempty"`
	To           string            `json:"to,omitempty"`
	Regions      []RegionBrief     `json:"regions,omitempty"`
	Divergence   *DivergenceReport `json:"divergence,omitempty"`
	ExpectedHash string            `json:"expected_hash,omitempty"`
	ActualHash   string            `json:"actual_hash,omitempty"`
}

type RegionBrief struct {
	ID     int    `json:"id"`
	Area   int    `json:"area"`
	Reason string `json:"reason,omitempty"`
}

type DivergenceReport struct {
	RegionCount        int     `json:"region_count"`
	PreviewRegionCount int     `json:"preview_region_count"`
	ExactRegionCount   int     `json:"exact_region_count"`
	MaxCentroidDelta   float64 `json:"max_centroid_delta"`
	MaxAreaRelError    float64 `json:"max_area_rel_error"`
	FirstRegionID      int     `json:"first_region_id"`
	DifferentPixels    int     `json:"different_pixels"`
	DiffPixelRatio     float64 `json:"diff_pixel_ratio"`
	CentroidTolerance  float64 `json:"centroid_tolerance"`
	AreaTolerance      float64 `json:"area_tolerance"`
	PixelTolerance     float64 `json:"pixel_tolerance"`
}

func (e *AppError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *AppError) JSON() string {
	b, _ := json.Marshal(e)
	return string(b)
}

func NewError(code, message string) *AppError {
	return &AppError{Code: code, Message: message}
}

func ParaError(field, rangeText, message string) *AppError {
	return &AppError{Code: CodePara, Field: field, Range: rangeText, Message: message}
}

func DecodeError(message string) *AppError {
	return &AppError{Code: CodeDecode, Message: message}
}

func RegionError(message string, regions []RegionBrief) *AppError {
	return &AppError{Code: CodeRegion, Message: message, Regions: regions}
}

func VersionError(snapshot, expected, actual string) *AppError {
	return &AppError{
		Code:         CodeVersion,
		Message:      "snapshot version mismatch",
		Snapshot:     snapshot,
		ExpectedHash: expected,
		ActualHash:   actual,
	}
}

func StateError(block, layer, from, to string) *AppError {
	return &AppError{
		Code:    CodeState,
		Message: "illegal state transition",
		Block:   block,
		Layer:   layer,
		From:    from,
		To:      to,
	}
}

func AsAppError(err error) *AppError {
	if err == nil {
		return nil
	}
	if app, ok := err.(*AppError); ok {
		return app
	}
	return NewError("E_INTERNAL", err.Error())
}
