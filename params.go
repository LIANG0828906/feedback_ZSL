package audiofp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
)

// Params contains all user-tunable annotation parameters.
type Params struct {
	FrameLength         int     `json:"frame_length"`
	FrameShift          int     `json:"frame_shift"`
	BandCount           int     `json:"band_count"`
	GapToleranceSamples int     `json:"gap_tolerance_samples"`
	MatchThreshold      float64 `json:"match_threshold"`
	MinEnergy           uint8   `json:"min_energy"`
	MinHits             int     `json:"min_hits"`
}

func DefaultParams() Params {
	return Params{
		FrameLength:         1024,
		FrameShift:          256,
		BandCount:           32,
		GapToleranceSamples: 512,
		MatchThreshold:      0.82,
		MinEnergy:           42,
		MinHits:             2,
	}
}

// Normalize fills zero-valued optional thresholds with defaults. Geometry
// fields are not silently changed.
func (p Params) Normalize() Params {
	d := DefaultParams()
	if p.FrameLength == 0 {
		p.FrameLength = d.FrameLength
	}
	if p.FrameShift == 0 {
		p.FrameShift = d.FrameShift
	}
	if p.BandCount == 0 {
		p.BandCount = d.BandCount
	}
	if p.GapToleranceSamples == 0 {
		p.GapToleranceSamples = d.GapToleranceSamples
	}
	if p.MatchThreshold == 0 {
		p.MatchThreshold = d.MatchThreshold
	}
	if p.MinEnergy == 0 {
		p.MinEnergy = d.MinEnergy
	}
	if p.MinHits == 0 {
		p.MinHits = d.MinHits
	}
	return p
}

func (p Params) Validate() error {
	if p.FrameLength < 256 || p.FrameLength > 16384 {
		return paraError("frame_length", "must be in [256,16384], got %d", p.FrameLength)
	}
	if p.FrameShift < 32 || p.FrameShift > 8192 {
		return paraError("frame_shift", "must be in [32,8192], got %d", p.FrameShift)
	}
	if p.BandCount < 4 || p.BandCount > 128 {
		return paraError("band_count", "must be in [4,128], got %d", p.BandCount)
	}
	if p.BandCount > p.FrameLength/2 {
		return paraError("band_count", "must not exceed frame_length/2=%d, got %d", p.FrameLength/2, p.BandCount)
	}
	if p.GapToleranceSamples < 0 || p.GapToleranceSamples > 60000 {
		return paraError("gap_tolerance_samples", "must be in [0,60000], got %d", p.GapToleranceSamples)
	}
	if math.IsNaN(p.MatchThreshold) || math.IsInf(p.MatchThreshold, 0) || p.MatchThreshold < 0 || p.MatchThreshold > 1 {
		return paraError("match_threshold", "must be finite and in [0,1], got %v", p.MatchThreshold)
	}
	if p.MinHits < 1 || p.MinHits > 100000 {
		return paraError("min_hits", "must be in [1,100000], got %d", p.MinHits)
	}
	return nil
}

// QuantumStep is the sample grid on which frame-derived boundaries lie.
func (p Params) QuantumStep() int {
	return gcd(p.FrameLength, p.FrameShift)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

func (p Params) canonicalFeatures() string {
	return fmt.Sprintf("%s|hann|%.1f|%d|%d|%d",
		AlgorithmVersion, 1.0, p.FrameLength, p.FrameShift, p.BandCount)
}

func (p Params) FeatureVersion() string {
	sum := sha256.Sum256([]byte(p.canonicalFeatures()))
	return "feat-" + hex.EncodeToString(sum[:])[:16]
}

// Version identifies all parameters that can change an annotation result.
func (p Params) Version() string {
	canonical := fmt.Sprintf("%s|gap=%d|threshold=%.6f|energy=%d|minhits=%d",
		p.canonicalFeatures(), p.GapToleranceSamples, p.MatchThreshold, p.MinEnergy, p.MinHits)
	sum := sha256.Sum256([]byte(canonical))
	return "param-" + hex.EncodeToString(sum[:])[:16]
}

// GapFrames converts sample tolerance into the number of permitted missing
// frame positions.
func (p Params) GapFrames() int {
	if p.GapToleranceSamples <= 0 || p.FrameShift <= 0 {
		return 0
	}
	return int(math.Round(float64(p.GapToleranceSamples) / float64(p.FrameShift)))
}
