package lightprobe

import (
	"math"
	"math/big"
	"sort"
	"strconv"
)

type TruncationRecord struct {
	ProbeID       string `json:"probe_id"`
	Channel       int    `json:"channel"`
	Stage         string `json:"stage"`
	Position      string `json:"position"`
	DecimalDigits int    `json:"decimal_digits"`
	Scale         int64  `json:"scale"`
	Denominator   int64  `json:"denominator"`
	Remainder     int64  `json:"remainder"`
}

type DirectionSample struct {
	Direction     Vec3       `json:"direction"`
	RadianceFixed [3]int64   `json:"radiance_fixed"`
	Radiance      [3]float64 `json:"radiance"`
}

type ProbeResult struct {
	Index                int                `json:"index,omitempty"`
	ProbeID              string             `json:"probe_id"`
	RoomID               string             `json:"room_id"`
	IJK                  [3]int             `json:"ijk"`
	RadianceFixed        [3]int64           `json:"radiance_fixed"`
	Radiance             [3]float64         `json:"radiance"`
	Distribution         []DirectionSample  `json:"distribution,omitempty"`
	Truncations          []TruncationRecord `json:"truncations"`
	SHQuantResidualMax   float64            `json:"sh_quant_residual_max"`
	CoefficientResiduals [3][9]float64      `json:"coefficient_residuals"`
	CountRoundingError   float64            `json:"count_rounding_error"`
	RawSampleCount       float64            `json:"raw_sample_count"`
	SampleCount          int                `json:"sample_count"`
	ContentHash          string             `json:"content_hash"`
	CacheKey             string             `json:"cache_key,omitempty"`
}

func ReconstructProbe(p Probe, params Params) (ProbeResult, error) {
	return reconstructProbeWithOrder(p, params, nil)
}

func ReconstructProbeWithTermOrder(p Probe, params Params, order []int) (ProbeResult, error) {
	return reconstructProbeWithOrder(p, params, order)
}

func reconstructProbeWithOrder(p Probe, params Params, order []int) (ProbeResult, error) {
	if err := params.Validate(); err != nil {
		return ProbeResult{}, err
	}
	bundle := SampleBundle{
		ProbeID:        p.ID,
		RoomID:         p.RoomID,
		Radiance:       p.Radiance,
		RawSampleCount: p.RawSampleCount,
		SampleCount:    p.SampleCount,
		SHScale:        p.SHScale,
		SH:             p.SH,
		ContentHash:    p.ContentHash,
	}
	if len(p.SampleBlob) > 0 {
		decoded, err := DecodeSampleBundle(p.SampleBlob)
		if err != nil {
			return ProbeResult{}, err
		}
		if decoded.ProbeID != p.ID {
			return ProbeResult{}, domainError(EDecode, p.ID, "sample_bundle.probe_id",
				"bundle identifies %q", decoded.ProbeID)
		}
		bundle = *decoded
	}
	if bundle.SHScale != params.FixedScale {
		return ProbeResult{}, domainError(EVal, p.ID, "sh_scale",
			"sample scale %d does not match parameter scale %d", bundle.SHScale, params.FixedScale)
	}

	result := ProbeResult{
		ProbeID:            p.ID,
		RoomID:             p.RoomID,
		IJK:                p.IJK,
		SHQuantResidualMax: 0,
		CountRoundingError: math.Abs(bundle.RawSampleCount - float64(bundle.SampleCount)),
		RawSampleCount:     bundle.RawSampleCount,
		SampleCount:        bundle.SampleCount,
		ContentHash:        p.ContentHash,
	}
	for ch := 0; ch < 3; ch++ {
		for k := 0; k < 9; k++ {
			residual := math.Abs(p.RawSH[ch][k] - float64(bundle.SH[ch][k])/float64(params.FixedScale))
			result.CoefficientResiduals[ch][k] = residual
			result.SHQuantResidualMax = math.Max(result.SHQuantResidualMax, residual)
		}
	}

	digits := fixedDigits(params.FixedScale)
	directions := shDirections()
	basisFixed := make([][9]int64, len(directions))
	for dIndex, d := range directions {
		basis := SHBasis(d)
		for k := 0; k < 9; k++ {
			basisFixed[dIndex][k] = quantizeFixed(basis[k], params.FixedScale)
		}
	}
	distribution := make([]DirectionSample, len(directions))
	for dIndex, d := range directions {
		distribution[dIndex].Direction = d
		for ch := 0; ch < 3; ch++ {
			dirSum := new(big.Int)
			for k := 0; k < 9; k++ {
				term := big.NewInt(bundle.SH[ch][k])
				term.Mul(term, big.NewInt(basisFixed[dIndex][k]))
				dirSum.Add(dirSum, term)
			}
			value := new(big.Int)
			value.QuoRem(dirSum, big.NewInt(params.FixedScale), new(big.Int))
			distribution[dIndex].RadianceFixed[ch] = value.Int64()
			distribution[dIndex].Radiance[ch] = float64(distribution[dIndex].RadianceFixed[ch]) / float64(params.FixedScale)
		}
	}
	result.Distribution = distribution

	canonicalOrder := make([]int, 0, len(directions)*9)
	termsByChannel := make([][]*big.Int, 3)
	for dIndex := range directions {
		for k := 0; k < 9; k++ {
			idx := dIndex*9 + k
			canonicalOrder = append(canonicalOrder, idx)
			for ch := 0; ch < 3; ch++ {
				prod := big.NewInt(bundle.SH[ch][k])
				prod.Mul(prod, big.NewInt(basisFixed[dIndex][k]))
				termsByChannel[ch] = append(termsByChannel[ch], prod)
			}
		}
	}
	useOrder := canonicalOrder
	if order != nil {
		if len(order) != len(canonicalOrder) {
			return ProbeResult{}, domainError(EVal, p.ID, "term_order",
				"order length %d, want %d", len(order), len(canonicalOrder))
		}
		seen := make([]bool, len(order))
		for _, idx := range order {
			if idx < 0 || idx >= len(order) || seen[idx] {
				return ProbeResult{}, domainError(EVal, p.ID, "term_order", "invalid permutation index %d", idx)
			}
			seen[idx] = true
		}
		useOrder = order
	}

	for ch := 0; ch < 3; ch++ {
		total := new(big.Int)
		for _, idx := range useOrder {
			total.Add(total, termsByChannel[ch][idx])
		}
		denSH := big.NewInt(int64(len(directions)) * params.FixedScale)
		shFixedBig, rem := new(big.Int), new(big.Int)
		shFixedBig.QuoRem(total, denSH, rem)
		shFixed := shFixedBig.Int64()
		result.Truncations = append(result.Truncations, TruncationRecord{
			ProbeID: p.ID, Channel: ch, Stage: "sh-direction-aggregate",
			Position:      "fixed-scale:decimal-" + strconv.Itoa(digits) + ":channel-" + strconv.Itoa(ch),
			DecimalDigits: digits, Scale: params.FixedScale, Denominator: denSH.Int64(),
			Remainder: rem.Int64(),
		})

		radianceFixed := quantizeFixed(bundle.Radiance[ch], params.FixedScale)
		n := int64(bundle.SampleCount)
		lambda := int64(params.SampleLambda)
		num := big.NewInt(n)
		num.Mul(num, big.NewInt(radianceFixed))
		shPart := big.NewInt(lambda)
		shPart.Mul(shPart, big.NewInt(shFixed))
		num.Add(num, shPart)
		denCount := big.NewInt(n + lambda)
		if denCount.Sign() <= 0 {
			return ProbeResult{}, domainError(EVal, p.ID, "sample_count", "sample weight denominator must be positive")
		}
		outFixed, countRem := new(big.Int), new(big.Int)
		outFixed.QuoRem(num, denCount, countRem)
		result.RadianceFixed[ch] = outFixed.Int64()
		result.Radiance[ch] = float64(result.RadianceFixed[ch]) / float64(params.FixedScale)
		result.Truncations = append(result.Truncations, TruncationRecord{
			ProbeID: p.ID, Channel: ch, Stage: "sample-count-weighted",
			Position:      "fixed-scale:decimal-" + strconv.Itoa(digits) + ":channel-" + strconv.Itoa(ch),
			DecimalDigits: digits, Scale: params.FixedScale, Denominator: denCount.Int64(),
			Remainder: countRem.Int64(),
		})
	}

	sort.Slice(result.Truncations, func(i, j int) bool {
		if result.Truncations[i].Channel != result.Truncations[j].Channel {
			return result.Truncations[i].Channel < result.Truncations[j].Channel
		}
		return result.Truncations[i].Stage < result.Truncations[j].Stage
	})
	return result, nil
}

func fixedDigits(scale int64) int {
	d := 0
	for scale > 1 {
		scale /= 10
		d++
	}
	return d
}

func SortedProbeResults(results map[string]ProbeResult) []ProbeResult {
	out := make([]ProbeResult, 0, len(results))
	for _, r := range results {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProbeID < out[j].ProbeID })
	return out
}
