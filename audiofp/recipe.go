package audiofp

import (
	"math"
	"math/bits"
)

const (
	MinFrameLen       = 64
	MaxFrameLen       = 8192
	MinBands          = 1
	MaxBands          = 32
	MinQuantBits      = 1
	MaxQuantBits      = 16
	MinCandidateLimit = 1
	MaxCandidateLimit = 1000
)

// ValidateRecipe validates all numeric boundaries and reports the JSON field
// name in E_PARA.
func ValidateRecipe(r Recipe) error {
	if r.FrameLen <= 0 {
		return parameterError("frame_len", "must be positive")
	}
	if r.FrameLen < MinFrameLen || r.FrameLen > MaxFrameLen {
		return parameterError("frame_len", "must be in [64,8192]")
	}
	if r.FrameShift <= 0 {
		return parameterError("frame_shift", "must be positive")
	}
	if r.FrameShift > r.FrameLen {
		return parameterError("frame_shift", "must not exceed frame_len")
	}
	if r.Bands < MinBands || r.Bands > MaxBands {
		return parameterError("bands", "must be in [1,32]")
	}
	if positiveBins(r.FrameLen) < r.Bands {
		return parameterError("bands", "too many bands for the FFT size implied by frame_len")
	}
	if r.QuantBits < MinQuantBits || r.QuantBits > MaxQuantBits {
		return parameterError("quant_bits", "must be in [1,16]")
	}
	if math.IsNaN(r.SimilarityThreshold) || math.IsInf(r.SimilarityThreshold, 0) ||
		r.SimilarityThreshold < 0 || r.SimilarityThreshold > 1 {
		return parameterError("similarity_threshold", "must be in [0,1]")
	}
	if r.CandidateLimit < MinCandidateLimit || r.CandidateLimit > MaxCandidateLimit {
		return parameterError("candidate_limit", "must be in [1,1000]")
	}
	return nil
}

func nextPowerOfTwo(n int) int {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}

func positiveBins(frameLen int) int {
	n := nextPowerOfTwo(frameLen)
	return n/2 - 1
}

func frameCount(totalSamples int, r Recipe) int {
	if totalSamples < r.FrameLen {
		return 0
	}
	return (totalSamples-r.FrameLen)/r.FrameShift + 1
}

// FrameStartOffset returns the first sample index of a frame.
func FrameStartOffset(frameNo int, r Recipe) int {
	return frameNo * r.FrameShift
}

// FrameEndOffset returns the exclusive sample end of one frame.
func FrameEndOffset(frameNo int, r Recipe) int {
	return FrameStartOffset(frameNo, r) + r.FrameLen
}

func recipeIsFrameAffecting(a, b Recipe) bool {
	return a.FrameLen != b.FrameLen || a.FrameShift != b.FrameShift ||
		a.Bands != b.Bands || a.QuantBits != b.QuantBits
}

func recipeIsQueryAffecting(a, b Recipe) bool {
	return recipeIsFrameAffecting(a, b) || a.SimilarityThreshold != b.SimilarityThreshold ||
		a.CandidateLimit != b.CandidateLimit
}

// ApplyRecipeParam mutates a recipe through an edit operation.
func ApplyRecipeParam(r *Recipe, param string, value float64) error {
	probe := *r
	asInt := func(field string) (int, error) {
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
			return 0, parameterError(field, "must be an integer")
		}
		return int(value), nil
	}
	switch param {
	case "frame_len":
		v, err := asInt(param)
		if err != nil {
			return err
		}
		probe.FrameLen = v
	case "frame_shift":
		v, err := asInt(param)
		if err != nil {
			return err
		}
		probe.FrameShift = v
	case "bands":
		v, err := asInt(param)
		if err != nil {
			return err
		}
		probe.Bands = v
	case "quant_bits":
		v, err := asInt(param)
		if err != nil {
			return err
		}
		probe.QuantBits = v
	case "similarity_threshold":
		probe.SimilarityThreshold = value
	case "candidate_limit":
		v, err := asInt(param)
		if err != nil {
			return err
		}
		probe.CandidateLimit = v
	default:
		return schemaError("param", "unknown recipe parameter")
	}
	if err := ValidateRecipe(probe); err != nil {
		if typed, ok := AsError(err); ok {
			typed.Field = param
		}
		return err
	}
	*r = probe
	return nil
}

func frameCacheKey(contentHash string, frameNo int, r Recipe) string {
	rv := recipeVersion(r)
	contentKey := digestString("asset-content|" + contentHash)
	upstream := digestString("recipe-frame|" + rv)
	return digestString("frame|" + contentKey + "|frame_no=" + itoa(frameNo) +
		"|start=" + itoa(FrameStartOffset(frameNo, r)) +
		"|frame_len=" + itoa(r.FrameLen) +
		"|frame_shift=" + itoa(r.FrameShift) +
		"|bands=" + itoa(r.Bands) +
		"|quant_bits=" + itoa(r.QuantBits) +
		"|upstream=" + upstream)
}

func indexCacheKey(frameKey string, frameNo, startSample int) string {
	return digestString("index|" + frameKey + "|frame_no=" + itoa(frameNo) +
		"|start=" + itoa(startSample))
}

func indexEntryDigest(assetID string, frameNo, startSample int, cacheKey string) string {
	return digestString("index-entry|" + assetID + "|" + itoa(frameNo) + "|" +
		itoa(startSample) + "|" + cacheKey)
}

func shardCacheKey(contentHash string, r Recipe, firstFrame, lastFrame int) string {
	contentKey := digestString("asset-content|" + contentHash)
	rv := recipeVersion(r)
	return digestString("shard|" + contentKey + "|recipe=" + rv +
		"|first=" + itoa(firstFrame) + "|last=" + itoa(lastFrame) +
		"|frame_len=" + itoa(r.FrameLen) + "|frame_shift=" + itoa(r.FrameShift) +
		"|bands=" + itoa(r.Bands) + "|quant_bits=" + itoa(r.QuantBits) +
		"|algorithm=" + AlgorithmVersion)
}

func itoa(v int) string {
	// Small local helper avoids formatting-related locale or spacing surprises.
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
