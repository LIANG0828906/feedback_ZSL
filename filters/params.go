// Package filters implements the deterministic, chunked brightness and chroma
// filtering stages used by the workbench.
package filters

import (
	"encoding/json"
	"fmt"
	"math"

	"yuvench/errs"
	"yuvench/yuv"
)

const (
	NodeYDenoise  = "Y:denoise"
	NodeYSharpen  = "Y:sharpen"
	NodeYContrast = "Y:contrast"
	NodeUSmooth   = "U:smooth"
	NodeUGain     = "U:gain"
	NodeVSmooth   = "V:smooth"
	NodeVGain     = "V:gain"
	NodeComposite = "COMPOSITE:interleave"
)

type DenoiseParams struct {
	Radius   int     `json:"radius"`
	Strength float64 `json:"strength"`
}

type SharpenParams struct {
	Radius int     `json:"radius"`
	Amount float64 `json:"amount"`
}

type ContrastParams struct {
	Window int     `json:"window"`
	Amount float64 `json:"amount"`
}

type SmoothParams struct {
	Radius   int     `json:"radius"`
	Strength float64 `json:"strength"`
}

type GainParams struct {
	Gain   float64 `json:"gain"`
	Offset float64 `json:"offset"`
}

type HeatmapParams struct {
	Normalization float64 `json:"normalization"`
}

type Params struct {
	YDenoise  DenoiseParams  `json:"yDenoise"`
	YSharpen  SharpenParams  `json:"ySharpen"`
	YContrast ContrastParams `json:"yContrast"`
	USmooth   SmoothParams   `json:"uSmooth"`
	UGain     GainParams     `json:"uGain"`
	VSmooth   SmoothParams   `json:"vSmooth"`
	VGain     GainParams     `json:"vGain"`
	Heatmap   HeatmapParams  `json:"heatmap"`
}

func DefaultParams() Params {
	return Params{
		YDenoise:  DenoiseParams{Radius: 1, Strength: 0.35},
		YSharpen:  SharpenParams{Radius: 1, Amount: 0.45},
		YContrast: ContrastParams{Window: 7, Amount: 0.30},
		USmooth:   SmoothParams{Radius: 2, Strength: 0.45},
		UGain:     GainParams{Gain: 1.025, Offset: 0},
		VSmooth:   SmoothParams{Radius: 2, Strength: 0.45},
		VGain:     GainParams{Gain: 1.025, Offset: 0},
		Heatmap:   HeatmapParams{Normalization: 63.75},
	}
}

func (p Params) Validate() error {
	if err := validateIntRange("yDenoise.radius", p.YDenoise.Radius, 1, 8); err != nil {
		return err
	}
	if err := validateFloatRange("yDenoise.strength", p.YDenoise.Strength, 0, 1); err != nil {
		return err
	}
	if err := validateIntRange("ySharpen.radius", p.YSharpen.Radius, 1, 8); err != nil {
		return err
	}
	if err := validateFloatRange("ySharpen.amount", p.YSharpen.Amount, 0, 2); err != nil {
		return err
	}
	if err := validateIntRange("yContrast.window", p.YContrast.Window, 3, 31); err != nil {
		return err
	}
	if p.YContrast.Window%2 == 0 {
		return errs.WithField(errs.CodeParameter, "yContrast.window", "window must be odd")
	}
	if err := validateFloatRange("yContrast.amount", p.YContrast.Amount, 0, 2); err != nil {
		return err
	}
	if err := validateIntRange("uSmooth.radius", p.USmooth.Radius, 1, 8); err != nil {
		return err
	}
	if err := validateFloatRange("uSmooth.strength", p.USmooth.Strength, 0, 1); err != nil {
		return err
	}
	if err := validateGain("uGain", p.UGain); err != nil {
		return err
	}
	if err := validateIntRange("vSmooth.radius", p.VSmooth.Radius, 1, 8); err != nil {
		return err
	}
	if err := validateFloatRange("vSmooth.strength", p.VSmooth.Strength, 0, 1); err != nil {
		return err
	}
	if err := validateGain("vGain", p.VGain); err != nil {
		return err
	}
	if err := validateFloatRange("heatmap.normalization", p.Heatmap.Normalization, 1e-6, 4*65535); err != nil {
		return err
	}
	return nil
}

func validateGain(prefix string, p GainParams) error {
	if err := validateFloatRange(prefix+".gain", p.Gain, 0.25, 4); err != nil {
		return err
	}
	return validateFloatRange(prefix+".offset", p.Offset, -0.5, 0.5)
}

func validateIntRange(field string, value, low, high int) error {
	if value < low || value > high {
		return &errs.Error{Code: errs.CodeParameter, Field: field, Message: "parameter outside allowed range", Expected: fmt.Sprintf("[%d,%d]", low, high), Actual: value}
	}
	return nil
}

func validateFloatRange(field string, value, low, high float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < low || value > high {
		return &errs.Error{Code: errs.CodeParameter, Field: field, Message: "parameter outside allowed range", Expected: fmt.Sprintf("[%g,%g]", low, high), Actual: value}
	}
	return nil
}

func (p Params) StageParams(node string) (any, error) {
	switch node {
	case NodeYDenoise:
		return p.YDenoise, nil
	case NodeYSharpen:
		return p.YSharpen, nil
	case NodeYContrast:
		return p.YContrast, nil
	case NodeUSmooth:
		return p.USmooth, nil
	case NodeUGain:
		return p.UGain, nil
	case NodeVSmooth:
		return p.VSmooth, nil
	case NodeVGain:
		return p.VGain, nil
	case NodeComposite:
		return struct{}{}, nil
	default:
		return nil, &errs.Error{Code: errs.CodeParameter, Message: "unknown stage", Node: node}
	}
}

func (p Params) StageParamsJSON(node string) ([]byte, error) {
	value, err := p.StageParams(node)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (p Params) StageParamsHash(node string) (string, error) {
	data, err := p.StageParamsJSON(node)
	if err != nil {
		return "", err
	}
	return yuv.HashBytes(data), nil
}

func (p Params) ImageParamsHash() string {
	copy := p
	copy.Heatmap.Normalization = 0
	return hashJSON(copy)
}

func (p Params) FullParamsHash() string {
	return hashJSON(p)
}

func hashJSON(value any) string {
	data, _ := json.Marshal(value)
	return yuv.HashBytes(data)
}

type Limits struct {
	MaxParallel          int
	MaxBufferBytes       int64
	PerWorkerBufferBytes int64
	ChunkRows            int
}

func DefaultLimits() Limits {
	return Limits{
		MaxParallel:          3,
		MaxBufferBytes:       16 << 20,
		PerWorkerBufferBytes: 1 << 20,
		ChunkRows:            16,
	}
}

func (l Limits) Validate() error {
	if l.MaxParallel < 1 || l.MaxParallel > 64 {
		return &errs.Error{Code: errs.CodeResource, Field: "maxParallel", Message: "parallelism outside allowed range", Expected: "[1,64]", Actual: l.MaxParallel}
	}
	if l.MaxBufferBytes < 64<<10 {
		return &errs.Error{Code: errs.CodeResource, Field: "maxBufferBytes", Message: "buffer budget is too small", Expected: ">=65536", Actual: l.MaxBufferBytes}
	}
	if l.PerWorkerBufferBytes <= 0 || l.PerWorkerBufferBytes > l.MaxBufferBytes {
		return &errs.Error{Code: errs.CodeResource, Field: "perWorkerBufferBytes", Message: "per-worker buffer must be positive and within the global budget", Expected: fmt.Sprintf("(0,%d]", l.MaxBufferBytes), Actual: l.PerWorkerBufferBytes}
	}
	if l.ChunkRows < 1 || l.ChunkRows > 4096 {
		return &errs.Error{Code: errs.CodeResource, Field: "chunkRows", Message: "chunk size outside allowed range", Expected: "[1,4096]", Actual: l.ChunkRows}
	}
	return nil
}
