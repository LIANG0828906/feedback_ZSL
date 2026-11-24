// Package convert implements the deterministic HDR-to-SDR conversion chain and
// self-proof record construction.
package convert

import (
	"encoding/json"
	"fmt"
	"math"

	"hdrproof/internal/apperr"
	"hdrproof/internal/color"
	"hdrproof/internal/model"
)

type chainSpec struct {
	AlgorithmVersion string           `json:"algorithm_version"`
	SourceColorSpace model.ColorSpace `json:"source_color_space"`
	TargetPrimaries  model.Primaries  `json:"target_primaries"`
	Params           model.Params     `json:"params"`
	Steps            []string         `json:"steps"`
	OutputEncoding   string           `json:"output_encoding"`
}

func ChainSummary(input model.ColorSpace, p model.Params) string {
	spec := chainSpec{
		AlgorithmVersion: model.AlgorithmVersion,
		SourceColorSpace: input,
		TargetPrimaries:  color.TargetPrimaries(),
		Params:           p,
		Steps: []string{
			"decode-little-endian",
			"scene-linear-exposure",
			"primary-matrix-to-bt709",
			"gamut-bound",
			"tone-map-" + p.ToneMap,
			"clamp-0-1",
			"encode-srgb8",
		},
		OutputEncoding: "rgb8le",
	}
	return model.HashJSON(spec)
}

func ValidateParams(p model.Params, route string, segment int) error {
	c := model.DefaultConstraints
	if !model.Finite(p.TargetPeakNits) || p.TargetPeakNits < c.TargetPeakMin || p.TargetPeakNits > c.TargetPeakMax {
		return apperr.New(apperr.CodePara, route, segment, -1, "target_peak_nits",
			fmt.Sprintf("%.0f..%.0f", c.TargetPeakMin, c.TargetPeakMax),
			fmt.Sprintf("value %v is outside the accepted range", p.TargetPeakNits), nil)
	}
	if !model.Finite(p.ExposureEV) || p.ExposureEV < c.ExposureMin || p.ExposureEV > c.ExposureMax {
		return apperr.New(apperr.CodePara, route, segment, -1, "exposure_ev",
			fmt.Sprintf("%.0f..%.0f", c.ExposureMin, c.ExposureMax),
			fmt.Sprintf("value %v is outside the accepted range", p.ExposureEV), nil)
	}
	if p.ToneMap != "aces" && p.ToneMap != "reinhard" {
		return apperr.New(apperr.CodePara, route, segment, -1, "tone_map", "aces|reinhard",
			fmt.Sprintf("unsupported value %q", p.ToneMap), nil)
	}
	if p.GamutMapping != "desaturate" && p.GamutMapping != "clip" {
		return apperr.New(apperr.CodePara, route, segment, -1, "gamut_mapping", "desaturate|clip",
			fmt.Sprintf("unsupported value %q", p.GamutMapping), nil)
	}
	if p.OutputBits != 8 {
		return apperr.New(apperr.CodePara, route, segment, -1, "output_bits", "8",
			fmt.Sprintf("unsupported value %d", p.OutputBits), nil)
	}
	return nil
}

// ConvertFrame converts one decoded source frame and emits exactly one proof
// record. The pixel and record hashes cover the quantized output, so a proof
// can be independently checked without invoking this package.
func ConvertFrame(
	route model.Route,
	frame model.DecodedFrame,
	truth model.FrameTruth,
	p model.Params,
	truthFingerprint string,
) (model.FrameResult, error) {
	segment := model.SegmentOf(route, frame.TimestampMS)
	if err := ValidateParams(p, route.ID, segment); err != nil {
		return model.FrameResult{}, err
	}
	expectedSamples := route.Width * route.Height * route.Channels
	if route.Channels != 3 || len(frame.LinearRGB) != expectedSamples {
		return model.FrameResult{}, apperr.New(apperr.CodeFrame, route.ID, segment, frame.Index,
			"frame_payload", fmt.Sprintf("%d linear samples", expectedSamples),
			fmt.Sprintf("received %d", len(frame.LinearRGB)), nil)
	}
	if truth.FrameIndex != frame.Index || truth.TimestampMS != frame.TimestampMS {
		return model.FrameResult{}, apperr.New(apperr.CodeSeg, route.ID, segment, frame.Index,
			"truth_alignment", fmt.Sprintf("frame %d timestamp %d", frame.Index, frame.TimestampMS),
			fmt.Sprintf("truth frame %d timestamp %d", truth.FrameIndex, truth.TimestampMS), nil)
	}
	pixels := make([]byte, route.Width*route.Height*3)
	matrix := color.PrimariesToTarget(route.ColorSpace.Primaries)
	exposure := math.Pow(2, p.ExposureEV)
	for px := 0; px < route.Width*route.Height; px++ {
		src := [3]float64{
			frame.LinearRGB[px*3] * exposure,
			frame.LinearRGB[px*3+1] * exposure,
			frame.LinearRGB[px*3+2] * exposure,
		}
		lin := matrix[0][0]*src[0] + matrix[0][1]*src[1] + matrix[0][2]*src[2]
		gin := matrix[1][0]*src[0] + matrix[1][1]*src[1] + matrix[1][2]*src[2]
		bin := matrix[2][0]*src[0] + matrix[2][1]*src[1] + matrix[2][2]*src[2]
		target := [3]float64{lin, gin, bin}
		if p.GamutMapping == "desaturate" {
			target = color.Desaturate(target)
		} else {
			for i := range target {
				target[i] = model.Clamp(target[i], 0, 1)
			}
		}
		for c := 0; c < 3; c++ {
			mapped := color.TransferForward(p.ToneMap, target[c])
			pixels[px*3+c] = color.EncodeSRGBByte(mapped)
		}
	}
	stats := computeStats(pixels, route.Width, route.Height, p.TargetPeakNits)
	patches, err := computePatches(pixels, route, truth)
	if err != nil {
		return model.FrameResult{}, err
	}
	proof := model.Proof{
		RecordVersion:    model.RecordVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		ParamVersion:     model.ParamVersion(p),
		RouteID:          route.ID,
		FrameIndex:       frame.Index,
		Segment:          segment,
		TimestampMS:      frame.TimestampMS,
		Input: model.InputDeclaration{
			Width: route.Width, Height: route.Height, Channels: route.Channels,
			Encoding: route.Encoding, PeakNits: route.PeakNits, FPS: route.FPS,
			ColorSpace: route.ColorSpace, TimestampMS: frame.TimestampMS,
			SourceFrameSHA256: model.SHA256Hex(frame.Payload),
		},
		Params:           p,
		ChainSummary:     ChainSummary(route.ColorSpace, p),
		OutputStats:      stats,
		Patches:          patches,
		TruthFingerprint: truthFingerprint,
	}
	proof.RecordSHA256 = model.ComputeRecordHash(proof)
	proofJSON, err := json.Marshal(proof)
	if err != nil {
		return model.FrameResult{}, err
	}
	return model.FrameResult{
		RouteID: route.ID, Frame: frame.Index, Segment: segment,
		Pixels: pixels, Proof: proof, ProofJSON: proofJSON,
	}, nil
}

func computeStats(pixels []byte, width, height int, targetPeak float64) model.Stats {
	stats := model.Stats{
		Width: width, Height: height, Channels: 3,
		MinLinear:   [3]float64{math.Inf(1), math.Inf(1), math.Inf(1)},
		MaxLinear:   [3]float64{math.Inf(-1), math.Inf(-1), math.Inf(-1)},
		PixelSHA256: model.SHA256Hex(pixels),
	}
	var sums [3]float64
	n := width * height
	for i := 0; i < n; i++ {
		for c := 0; c < 3; c++ {
			v := color.DecodeSRGBByte(pixels[i*3+c])
			if v < stats.MinLinear[c] {
				stats.MinLinear[c] = v
			}
			if v > stats.MaxLinear[c] {
				stats.MaxLinear[c] = v
			}
			sums[c] += v
		}
	}
	for c := 0; c < 3; c++ {
		stats.MeanLinear[c] = sums[c] / float64(n)
	}
	maxAll := math.Max(stats.MaxLinear[0], math.Max(stats.MaxLinear[1], stats.MaxLinear[2]))
	stats.PeakNits = maxAll * targetPeak
	return stats
}

func computePatches(pixels []byte, route model.Route, truth model.FrameTruth) ([]model.PatchProof, error) {
	out := make([]model.PatchProof, 0, len(truth.Patches))
	for _, patch := range truth.Patches {
		if patch.X < 0 || patch.Y < 0 || patch.Width <= 0 || patch.Height <= 0 ||
			patch.X+patch.Width > route.Width || patch.Y+patch.Height > route.Height {
			return nil, apperr.New(apperr.CodeFrame, route.ID, model.SegmentOf(route, truth.TimestampMS),
				truth.FrameIndex, "truth_patch_rect", "rectangle inside output frame",
				fmt.Sprintf("patch %s is outside %dx%d", patch.Name, route.Width, route.Height), nil)
		}
		var sums [3]float64
		count := float64(patch.Width * patch.Height)
		for y := patch.Y; y < patch.Y+patch.Height; y++ {
			for x := patch.X; x < patch.X+patch.Width; x++ {
				idx := (y*route.Width + x) * 3
				for c := 0; c < 3; c++ {
					sums[c] += color.DecodeSRGBByte(pixels[idx+c])
				}
			}
		}
		var actual [3]float64
		var delta [3]float64
		deltaMax := 0.0
		for c := 0; c < 3; c++ {
			actual[c] = sums[c] / count
			delta[c] = actual[c] - patch.ExpectedLinearRGB[c]
			deltaMax = math.Max(deltaMax, math.Abs(delta[c]))
		}
		out = append(out, model.PatchProof{
			Name: patch.Name, X: patch.X, Y: patch.Y, Width: patch.Width, Height: patch.Height,
			ExpectedLinearRGB: patch.ExpectedLinearRGB,
			ActualLinearRGB:   actual,
			DeltaRGB:          delta,
			DeltaMax:          deltaMax,
		})
	}
	return out, nil
}
