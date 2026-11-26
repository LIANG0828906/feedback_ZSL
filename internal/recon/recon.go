// Package recon reconstructs target linear chromatic channels from quantized bands.
package recon

import (
	"fmt"

	"msworkbench/internal/conv"
	"msworkbench/internal/domain"
	"msworkbench/internal/e"
	"msworkbench/internal/quant"
)

type PixelColor struct {
	LinearBP       [3]int64 `json:"linear_bp"`
	ChromaticityBP [2]int64 `json:"chromaticity_bp"`
}

type BlockResult struct {
	Coord          domain.BlockCoord `json:"coord"`
	Exact          bool              `json:"exact"`
	Approximate    bool              `json:"approximate"`
	Method         quant.Method      `json:"method"`
	Pixels         []PixelColor      `json:"pixels"`
	PeakLinearBP   [3]int64          `json:"peak_linear_bp"`
	PeakYBP        int64             `json:"peak_y_bp"`
	ErrorBoundBP   [3]int64          `json:"error_bound_bp"`
	MaxDeviationBP [3]int64          `json:"max_deviation_bp,omitempty"`
	ReconHash      string            `json:"recon_hash"`
}

func ceilMul(a, b int64, field string) (int64, error) {
	q, rem, err := conv.MulDivFloor(a, b, conv.PermyriadScale, field, nil, "ceil_mul")
	if err != nil {
		return 0, err
	}
	if rem != 0 {
		q++
	}
	return q, nil
}

func ReconstructBlock(p *domain.Project, qr *quant.Result) (*BlockResult, error) {
	n := 0
	for _, br := range qr.Bands {
		n = len(br.Values)
		break
	}
	if n == 0 {
		return nil, fmt.Errorf("quant result for block %s has no pixels", qr.Coord)
	}
	out := &BlockResult{
		Coord: qr.Coord, Method: qr.Decision.Method,
		Exact:       qr.Decision.Method != quant.MethodLUT,
		Approximate: qr.Decision.Method == quant.MethodLUT,
		Pixels:      make([]PixelColor, n),
	}

	for li := 0; li < n; li++ {
		var linear [3]int64
		for _, band := range p.Bands {
			br := qr.Bands[band.ID]
			if br == nil {
				return nil, fmt.Errorf("missing quant result band %s", band.ID)
			}
			q := br.Values[li]
			for c := 0; c < 3; c++ {
				contrib, _, err := conv.MulDivFloor(q, band.Response[c], domain.InternalQuantScale, "reconstruction.response", nil, "response")
				if err != nil {
					return nil, err
				}
				linear[c] += contrib
			}
		}
		for c := 0; c < 3; c++ {
			v, _, err := conv.MulDivFloor(linear[c], p.WhitePointBP[c], domain.InternalQuantScale, "reconstruction.white", nil, "white")
			if err != nil {
				return nil, err
			}
			linear[c] = v
		}
		sum := linear[0] + linear[1] + linear[2]
		xy := [2]int64{}
		if sum > 0 {
			x, _, err := conv.MulDivFloor(linear[0], domain.InternalQuantScale, sum, "chromaticity.x", nil, "chromaticity")
			if err != nil {
				return nil, err
			}
			y, _, err := conv.MulDivFloor(linear[1], domain.InternalQuantScale, sum, "chromaticity.y", nil, "chromaticity")
			if err != nil {
				return nil, err
			}
			xy = [2]int64{x, y}
		} else {
			wsum := p.WhitePointBP[0] + p.WhitePointBP[1] + p.WhitePointBP[2]
			x, _, _ := conv.MulDivFloor(p.WhitePointBP[0], domain.InternalQuantScale, wsum, "white.x", nil, "white_chromaticity")
			y, _, _ := conv.MulDivFloor(p.WhitePointBP[1], domain.InternalQuantScale, wsum, "white.y", nil, "white_chromaticity")
			xy = [2]int64{x, y}
		}
		out.Pixels[li] = PixelColor{LinearBP: linear, ChromaticityBP: xy}
		for c := 0; c < 3; c++ {
			if linear[c] > out.PeakLinearBP[c] {
				out.PeakLinearBP[c] = linear[c]
			}
		}
		if linear[1] > out.PeakYBP {
			out.PeakYBP = linear[1]
		}
	}

	bound, err := channelErrorBound(p, qr)
	if err != nil {
		return nil, err
	}
	out.ErrorBoundBP = bound
	out.ReconHash = domain.HashJSON(map[string]any{"block": qr.Coord.String(), "method": out.Method, "pixels": out.Pixels, "peak": out.PeakLinearBP})
	return out, nil
}

func channelErrorBound(p *domain.Project, qr *quant.Result) ([3]int64, error) {
	var bound [3]int64
	for _, band := range p.Bands {
		br := qr.Bands[band.ID]
		if br == nil {
			continue
		}
		for c := 0; c < 3; c++ {
			part, err := ceilMul(br.ErrorBoundQ, band.Response[c], "error_bound.response")
			if err != nil {
				return bound, err
			}
			bound[c] += part
		}
	}
	for c := 0; c < 3; c++ {
		v, err := ceilMul(bound[c], p.WhitePointBP[c], "error_bound.white")
		if err != nil {
			return bound, err
		}
		bound[c] = v + int64(len(p.Bands))
	}
	return bound, nil
}

// CheckDrift compares a candidate reconstruction against the exact/sparse
// reconstruction. A violation returns E_DRIFT with block and contributing band.
func CheckDrift(p *domain.Project, exactQ, candidateQ *quant.Result, exact, candidate *BlockResult) error {
	if exact.Coord != candidate.Coord {
		return e.New(e.Diff, "reconstruction block mismatch %s != %s", exact.Coord, candidate.Coord)
	}
	n := len(exact.Pixels)
	if len(candidate.Pixels) != n {
		return e.New(e.Diff, "reconstruction pixel count mismatch at block %s", exact.Coord)
	}
	var maxDev [3]int64
	var worstPixel int
	var worstChannel int
	for i := 0; i < n; i++ {
		for c := 0; c < 3; c++ {
			d := candidate.Pixels[i].LinearBP[c] - exact.Pixels[i].LinearBP[c]
			if d < 0 {
				d = -d
			}
			if d > maxDev[c] {
				maxDev[c] = d
				worstPixel = i
				worstChannel = c
			}
		}
	}
	candidate.MaxDeviationBP = maxDev
	for c := 0; c < 3; c++ {
		if maxDev[c] > candidate.ErrorBoundBP[c] {
			band := worstBand(p, exactQ, candidateQ, worstPixel, worstChannel)
			return &e.Error{
				Code: e.Drift, Block: exact.Coord.String(), Band: band,
				Channel: channelName(worstChannel), Observed: maxDev[c], Bound: candidate.ErrorBoundBP[c],
				Field: "reconstruction.linear_bp", Path: fmt.Sprintf("$.blocks[%s].pixels[%d].linear_bp[%d]", exact.Coord, worstPixel, worstChannel),
				Message: "approximate reconstruction deviation exceeds derived error bound",
			}
		}
	}
	return nil
}

func worstBand(p *domain.Project, exactQ, candidateQ *quant.Result, pixel, channel int) string {
	worst := p.Bands[0].ID
	var worstImpact int64
	for _, b := range p.Bands {
		eq := exactQ.Bands[b.ID].Values[pixel]
		cq := candidateQ.Bands[b.ID].Values[pixel]
		d := cq - eq
		if d < 0 {
			d = -d
		}
		impact := d * b.Response[channel]
		if impact > worstImpact {
			worstImpact = impact
			worst = b.ID
		}
	}
	return worst
}

func channelName(c int) string {
	switch c {
	case 0:
		return "X/R"
	case 1:
		return "Y/G"
	default:
		return "Z/B"
	}
}
