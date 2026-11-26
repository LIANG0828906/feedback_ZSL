package vpf

import (
	"encoding/json"
)

const DefaultSeed int64 = 20261127

type WeightSampleTruth struct {
	FrameIndex       int    `json:"frame_index"`
	AtNS             int64  `json:"at_ns"`
	LayerID          string `json:"layer_id"`
	SourceID         string `json:"source_id"`
	X                int    `json:"x"`
	Y                int    `json:"y"`
	DistancePX       int    `json:"distance_px"`
	ExpectedWeightBP int    `json:"expected_weight_bp"`
}

type FrameTruth struct {
	FrameIndex     int                 `json:"frame_index"`
	AtNS           int64               `json:"at_ns"`
	ActiveLayerIDs []string            `json:"active_layer_ids"`
	ChunkCount     int                 `json:"chunk_count"`
	SampleWeights  []WeightSampleTruth `json:"sample_weights"`
}

type TruthSet struct {
	Seed             int64        `json:"seed"`
	AlgorithmVersion string       `json:"algorithm_version"`
	Width            int          `json:"width"`
	Height           int          `json:"height"`
	Channels         int          `json:"channels"`
	TileWidth        int          `json:"tile_width"`
	TileHeight       int          `json:"tile_height"`
	Frames           []FrameTruth `json:"frames"`
}

func LoadTruth(data []byte) (*TruthSet, error) {
	var truth TruthSet
	root, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	// Re-decode through encoding/json into the typed truth contract.
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, &Error{Code: CodeSchema, Message: "invalid truth document", FieldPath: "$", Details: err.Error()}
	}
	if truth.AlgorithmVersion == "" {
		return nil, schemaError(0, "$.algorithm_version", "required truth field is missing")
	}
	_ = root
	return &truth, nil
}

// integerDistance returns floor(sqrt(dx²+dy²)) using integer arithmetic.
func integerDistance(x1, y1, x2, y2 int) int {
	dx := int64(x1 - x2)
	dy := int64(y1 - y2)
	squared := dx*dx + dy*dy
	return int(isqrt64(squared))
}

func isqrt64(n int64) int64 {
	if n <= 0 {
		return 0
	}
	var lo, hi uint64 = 0, 1 << 32
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		if mid <= uint64(n)/mid {
			lo = mid
		} else {
			hi = mid
		}
	}
	return int64(lo)
}

// RippleWeightBP is the declared analytic ripple attenuation formula.
// It is the sole oracle used by truth generation and sample assertions.
func RippleWeightBP(source RippleSource, x, y int) (distance, weightBP int) {
	distance = integerDistance(x, y, source.Center.X, source.Center.Y)
	if source.Radius <= 0 {
		return distance, 0
	}
	falloff := int64(source.Radius)*OpacityScale - int64(source.DecayBP)*int64(distance)
	if falloff <= 0 {
		return distance, 0
	}
	weight := int64(source.AmplitudeBP) * falloff / (int64(source.Radius) * OpacityScale)
	if weight < 0 {
		weight = 0
	}
	if weight > OpacityScale {
		weight = OpacityScale
	}
	return distance, int(weight)
}

// EffectiveRipples applies time-ordered parameter instructions at or before atNS.
func EffectiveRipples(p *Project, layer Layer, frame Frame) []RippleSource {
	sources := append([]RippleSource(nil), layer.Ripples...)
	for _, instruction := range p.Instructions {
		if instruction.LayerID != layer.ID || instruction.AtNS > frame.AtNS {
			continue
		}
		for i := range sources {
			if instruction.SourceID != "" && sources[i].ID != instruction.SourceID {
				continue
			}
			switch instruction.Op {
			case "set_ripple_radius":
				if instruction.Radius > 0 {
					sources[i].Radius = instruction.Radius
				}
			case "set_ripple_strength":
				if instruction.AmplitudeBP > 0 {
					sources[i].AmplitudeBP = instruction.AmplitudeBP
				}
			}
		}
	}
	return sources
}

type TruthMismatch struct {
	FrameIndex       int    `json:"frame_index"`
	SourceID         string `json:"source_id"`
	X                int    `json:"x"`
	Y                int    `json:"y"`
	ExpectedDistance int    `json:"expected_distance"`
	ObservedDistance int    `json:"observed_distance"`
	ExpectedWeight   int    `json:"expected_weight"`
	ObservedWeight   int    `json:"observed_weight"`
}

// VerifyTruth uses the generator-created truth as the oracle. Observations are
// the declared analytic weight function, never values reverse-engineered from
// composited pixels.
func VerifyTruth(p *Project, truth *TruthSet) ([]TruthMismatch, error) {
	var mismatches []TruthMismatch
	for _, frameTruth := range truth.Frames {
		frame, ok := p.FrameByIndex(frameTruth.FrameIndex)
		if !ok {
			return mismatches, schemaError(p.SourceVersion, "$.frames", "truth frame %d absent", frameTruth.FrameIndex)
		}
		if frame.AtNS != frameTruth.AtNS {
			mismatches = append(mismatches, TruthMismatch{
				FrameIndex: frameTruth.FrameIndex, ExpectedWeight: -1, ObservedWeight: -2,
			})
		}
		for _, sample := range frameTruth.SampleWeights {
			layer, ok := p.LayerByID(sample.LayerID)
			if !ok {
				return mismatches, schemaError(p.SourceVersion, "$.layers", "truth layer %q absent", sample.LayerID)
			}
			var source *RippleSource
			sources := EffectiveRipples(p, layer, frame)
			for i := range sources {
				if sources[i].ID == sample.SourceID {
					source = &sources[i]
					break
				}
			}
			if source == nil {
				return mismatches, schemaError(p.SourceVersion, "$.layers[].ripples", "truth source %q absent", sample.SourceID)
			}
			distance, weight := RippleWeightBP(*source, sample.X, sample.Y)
			if distance != sample.DistancePX || weight != sample.ExpectedWeightBP {
				mismatches = append(mismatches, TruthMismatch{
					FrameIndex: frameTruth.FrameIndex, SourceID: sample.SourceID,
					X: sample.X, Y: sample.Y,
					ExpectedDistance: sample.DistancePX, ObservedDistance: distance,
					ExpectedWeight: sample.ExpectedWeightBP, ObservedWeight: weight,
				})
			}
		}
	}
	return mismatches, nil
}
