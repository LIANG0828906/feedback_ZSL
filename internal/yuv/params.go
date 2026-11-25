package yuv

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	"uvenhance/internal/apperr"
)

type LumaParams struct {
	DenoiseStrength  float64 `json:"denoise_strength"`
	SharpenAmount    float64 `json:"sharpen_amount"`
	SharpenThreshold float64 `json:"sharpen_threshold"`
	ContrastStrength float64 `json:"contrast_strength"`
	ContrastRadius   int     `json:"contrast_radius"`
}

type ChromaParams struct {
	SmoothSigma float64 `json:"smooth_sigma"`
	Gain        float64 `json:"gain"`
	Offset      float64 `json:"offset"`
}

type Params struct {
	Luma    LumaParams   `json:"luma"`
	ChromaU ChromaParams `json:"chroma_u"`
	ChromaV ChromaParams `json:"chroma_v"`
}

func DefaultParams() Params {
	return Params{
		Luma: LumaParams{
			DenoiseStrength: 0.32, SharpenAmount: 0.65, SharpenThreshold: 0.008,
			ContrastStrength: 0.24, ContrastRadius: 4,
		},
		ChromaU: ChromaParams{SmoothSigma: 1.1, Gain: 1.04, Offset: 0.0},
		ChromaV: ChromaParams{SmoothSigma: 1.1, Gain: 1.03, Offset: 0.0},
	}
}

func LoadParams(path string) (Params, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Params{}, apperr.IO("read parameters "+path, err)
	}
	var p Params
	if err := json.Unmarshal(raw, &p); err != nil {
		return Params{}, apperr.Para("parameters", "invalid JSON: "+err.Error())
	}
	if err := p.Validate(); err != nil {
		return Params{}, err
	}
	return p, nil
}

func (p Params) Validate() error {
	checks := []struct {
		field string
		value float64
		lo    float64
		hi    float64
	}{
		{"luma.denoise_strength", p.Luma.DenoiseStrength, 0, 1},
		{"luma.sharpen_amount", p.Luma.SharpenAmount, 0, 2},
		{"luma.sharpen_threshold", p.Luma.SharpenThreshold, 0, 0.5},
		{"luma.contrast_strength", p.Luma.ContrastStrength, 0, 1},
		{"chroma_u.smooth_sigma", p.ChromaU.SmoothSigma, 0.1, 4},
		{"chroma_u.gain", p.ChromaU.Gain, 0.5, 1.5},
		{"chroma_u.offset", p.ChromaU.Offset, -0.25, 0.25},
		{"chroma_v.smooth_sigma", p.ChromaV.SmoothSigma, 0.1, 4},
		{"chroma_v.gain", p.ChromaV.Gain, 0.5, 1.5},
		{"chroma_v.offset", p.ChromaV.Offset, -0.25, 0.25},
	}
	for _, c := range checks {
		if !(c.value >= c.lo && c.value <= c.hi) {
			return apperr.Para(c.field, fmt.Sprintf("value %v outside [%v,%v]", c.value, c.lo, c.hi))
		}
	}
	if p.Luma.ContrastRadius < 1 || p.Luma.ContrastRadius > 8 {
		return apperr.Para("luma.contrast_radius", fmt.Sprintf("value %d outside [1,8]", p.Luma.ContrastRadius))
	}
	return nil
}

func (p Params) Equal(other Params) bool { return reflect.DeepEqual(p, other) }
