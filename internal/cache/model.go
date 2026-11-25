package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"uvenhance/internal/yuv"
)

const (
	StageYDenoise   = "luma.denoise"
	StageYSharpen   = "luma.sharpen"
	StageYContrast  = "luma.local_contrast"
	StageUSmooth    = "chroma_u.band_smooth"
	StageUGain      = "chroma_u.gain"
	StageVSmooth    = "chroma_v.band_smooth"
	StageVGain      = "chroma_v.gain"
	StageInterleave = "interleave"
)

// PipelineVersion must be bumped whenever stage semantics change. It is part of
// the snapshot manifest and cache key version so old algorithm outputs can never
// be silently reused after an upgrade.
const PipelineVersion = 2

const keyVersion = "uv-cache-key-v2-pipeline-2"

const (
	KindPlane = "plane"
	KindImage = "image"
)

type StageSpec struct {
	ID    string
	Plane string
	Kind  string
	Deps  []string
}

var Stages = []StageSpec{
	{ID: StageYDenoise, Plane: "Y", Kind: KindPlane},
	{ID: StageYSharpen, Plane: "Y", Kind: KindPlane, Deps: []string{StageYDenoise}},
	{ID: StageYContrast, Plane: "Y", Kind: KindPlane, Deps: []string{StageYSharpen}},
	{ID: StageUSmooth, Plane: "U", Kind: KindPlane},
	{ID: StageUGain, Plane: "U", Kind: KindPlane, Deps: []string{StageUSmooth}},
	{ID: StageVSmooth, Plane: "V", Kind: KindPlane},
	{ID: StageVGain, Plane: "V", Kind: KindPlane, Deps: []string{StageVSmooth}},
	{ID: StageInterleave, Plane: "YUV", Kind: KindImage, Deps: []string{StageYContrast, StageUGain, StageVGain}},
}

func StageByID(id string) (StageSpec, bool) {
	for _, s := range Stages {
		if s.ID == id {
			return s, true
		}
	}
	return StageSpec{}, false
}

func StageIndex(id string) int {
	for i, s := range Stages {
		if s.ID == id {
			return i
		}
	}
	return -1
}

type Output struct {
	StageID      string     `json:"stage_id"`
	Plane        string     `json:"plane"`
	Kind         string     `json:"kind"`
	Key          string     `json:"cache_key"`
	InputHash    string     `json:"input_content_hash"`
	UpstreamKey  string     `json:"upstream_cache_key"`
	ParamDigest  string     `json:"parameter_digest"`
	OutputHash   string     `json:"output_digest"`
	Width        int        `json:"width"`
	Height       int        `json:"height"`
	BitDepth     int        `json:"bit_depth"`
	QuantMin     uint16     `json:"quant_min"`
	QuantMax     uint16     `json:"quant_max"`
	Alignment    string     `json:"alignment,omitempty"`
	PlaneData    *yuv.Plane `json:"-"`
	ImageData    *yuv.Image `json:"-"`
	CommittedSeq int        `json:"committed_sequence"`
}

func NewPlaneOutput(stageID, key, inputHash, upstreamKey, paramDigest string, p *yuv.Plane) *Output {
	return &Output{
		StageID: stageID, Plane: p.Name, Kind: KindPlane, Key: key,
		InputHash: inputHash, UpstreamKey: upstreamKey, ParamDigest: paramDigest,
		OutputHash: p.ContentHash(), Width: p.Width, Height: p.Height,
		BitDepth: p.BitDepth, QuantMin: p.QuantMin, QuantMax: p.QuantMax,
		PlaneData: p,
	}
}

func NewImageOutput(stageID, key, inputHash, upstreamKey, paramDigest, alignment string, img *yuv.Image) *Output {
	return &Output{
		StageID: stageID, Plane: "YUV", Kind: KindImage, Key: key,
		InputHash: inputHash, UpstreamKey: upstreamKey, ParamDigest: paramDigest,
		OutputHash: img.Digest(), Width: img.Width, Height: img.Height,
		BitDepth: img.BitDepth, QuantMin: img.QuantMin, QuantMax: img.QuantMax,
		Alignment: alignment, ImageData: img,
	}
}

func (o *Output) ValidatePayload() error {
	switch o.Kind {
	case KindPlane:
		if o.PlaneData == nil {
			return fmt.Errorf("%s: missing plane payload", o.StageID)
		}
		if o.PlaneData.ContentHash() != o.OutputHash {
			return fmt.Errorf("%s: output hash mismatch", o.StageID)
		}
	case KindImage:
		if o.ImageData == nil {
			return fmt.Errorf("%s: missing image payload", o.StageID)
		}
		if o.ImageData.Digest() != o.OutputHash {
			return fmt.Errorf("%s: output hash mismatch", o.StageID)
		}
	default:
		return fmt.Errorf("%s: unknown kind %q", o.StageID, o.Kind)
	}
	return nil
}

func ParamsForStage(p yuv.Params, stageID string) any {
	switch stageID {
	case StageYDenoise:
		return struct {
			Strength float64 `json:"strength"`
		}{p.Luma.DenoiseStrength}
	case StageYSharpen:
		return struct {
			Amount    float64 `json:"amount"`
			Threshold float64 `json:"threshold"`
		}{p.Luma.SharpenAmount, p.Luma.SharpenThreshold}
	case StageYContrast:
		return struct {
			Strength float64 `json:"strength"`
			Radius   int     `json:"radius"`
		}{p.Luma.ContrastStrength, p.Luma.ContrastRadius}
	case StageUSmooth:
		return struct {
			Sigma float64 `json:"sigma"`
		}{p.ChromaU.SmoothSigma}
	case StageUGain:
		return struct {
			Gain   float64 `json:"gain"`
			Offset float64 `json:"offset"`
		}{p.ChromaU.Gain, p.ChromaU.Offset}
	case StageVSmooth:
		return struct {
			Sigma float64 `json:"sigma"`
		}{p.ChromaV.SmoothSigma}
	case StageVGain:
		return struct {
			Gain   float64 `json:"gain"`
			Offset float64 `json:"offset"`
		}{p.ChromaV.Gain, p.ChromaV.Offset}
	}
	return nil
}

type InterleaveParams struct {
	Upsample  string `json:"upsample"`
	Alignment string `json:"alignment"`
	BitDepth  int    `json:"bit_depth"`
	QuantMin  uint16 `json:"quant_min"`
	QuantMax  uint16 `json:"quant_max"`
}

func ParamDigest(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func MakeKey(inputHash, stageID string, params any, upstreamKeys []string) (string, string, error) {
	paramDigest, err := ParamDigest(params)
	if err != nil {
		return "", "", err
	}
	envelope := struct {
		Version      string   `json:"key_version"`
		InputHash    string   `json:"input_content_hash"`
		StageID      string   `json:"stage_id"`
		ParamDigest  string   `json:"parameter_digest"`
		UpstreamKeys []string `json:"upstream_cache_keys"`
	}{
		Version: keyVersion, InputHash: inputHash, StageID: stageID,
		ParamDigest:  paramDigest,
		UpstreamKeys: append([]string(nil), upstreamKeys...),
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), paramDigest, nil
}

func JoinHashes(hashes ...string) string {
	h := sha256.New()
	for _, v := range hashes {
		_, _ = h.Write([]byte(v))
		_, _ = h.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func JoinKeys(keys ...string) string {
	return JoinHashes(keys...)
}
