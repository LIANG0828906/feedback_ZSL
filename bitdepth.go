package quantforensics

import "fmt"

type DiscrepancyLevel string

const (
	LevelNone       DiscrepancyLevel = "none"
	LevelCoordinate DiscrepancyLevel = "block_coordinate_conversion"
	LevelPalette    DiscrepancyLevel = "palette_quantization"
	LevelBitDepth   DiscrepancyLevel = "bit_depth_quantization"
)

type BitDepthSpec struct {
	BitsR int `json:"bitsR"`
	BitsG int `json:"bitsG"`
	BitsB int `json:"bitsB"`
}

func RGB565Spec() BitDepthSpec { return BitDepthSpec{BitsR: 5, BitsG: 6, BitsB: 5} }

func (s BitDepthSpec) valid() error {
	for _, v := range []int{s.BitsR, s.BitsG, s.BitsB} {
		if v < 1 || v > 8 {
			return schemaError("$.bitDepth", "channel bit depth must be between 1 and 8")
		}
	}
	return nil
}

// QuantizeChannelBitDepth reduces an 8-bit channel to the declared bit depth
// and expands it back to 8-bit using integer-only nearest level scaling.
func QuantizeChannelBitDepth(v uint8, bits int) (reconstructed uint8, quantized int, residual int, err error) {
	if bits < 1 || bits > 8 {
		return 0, 0, 0, schemaError("$.bitDepth", "bit depth must be 1..8")
	}
	levels := int64(1) << uint(bits)
	last := levels - 1
	input := int64(v)
	q := (input*last + 127) / 255
	if q < 0 {
		q = 0
	}
	if q > last {
		q = last
	}
	out := q * 255 / last
	if out < 0 || out > 255 {
		return 0, 0, 0, overflowError("$.bitDepth.reconstruct", q, last, "bit-depth expansion out of uint8 range")
	}
	return uint8(out), int(q), int(out - input), nil
}

func QuantizePixelBitDepth(c RGB, spec BitDepthSpec) (RGB, [3]int, error) {
	if err := spec.valid(); err != nil {
		return RGB{}, [3]int{}, err
	}
	bits := [3]int{spec.BitsR, spec.BitsG, spec.BitsB}
	channels := [3]uint8{c.R, c.G, c.B}
	out := [3]uint8{}
	res := [3]int{}
	for i := 0; i < 3; i++ {
		r, _, residual, err := QuantizeChannelBitDepth(channels[i], bits[i])
		if err != nil {
			return RGB{}, [3]int{}, err
		}
		out[i] = r
		res[i] = residual
	}
	return RGB{R: out[0], G: out[1], B: out[2]}, res, nil
}

func MaxBitDepthDeviation(spec BitDepthSpec) ([3]int, error) {
	if err := spec.valid(); err != nil {
		return [3]int{}, err
	}
	bits := [3]int{spec.BitsR, spec.BitsG, spec.BitsB}
	max := [3]int{}
	for channel := 0; channel < 3; channel++ {
		for v := 0; v < 256; v++ {
			_, _, residual, err := QuantizeChannelBitDepth(uint8(v), bits[channel])
			if err != nil {
				return [3]int{}, err
			}
			d := residual
			if d < 0 {
				d = -d
			}
			if d > max[channel] {
				max[channel] = d
			}
		}
	}
	return max, nil
}

type DiscrepancyClassification struct {
	Level              DiscrepancyLevel `json:"level"`
	BlockID            string           `json:"blockId,omitempty"`
	Coordinate         *BlockCoordinate `json:"coordinate,omitempty"`
	IndexMismatches    int              `json:"indexMismatches"`
	MaxDeviation       [3]int           `json:"maxDeviation"`
	BitDepthBound      [3]int           `json:"bitDepthBound"`
	CoordinateResidual [2]int           `json:"coordinateResidual"`
	Rule               string           `json:"rule"`
	Evidence           []string         `json:"evidence,omitempty"`
}

// ClassifyDiscrepancy fixes the triage order: coordinate evidence first, then
// palette index evidence, then the declared bit-depth error envelope.
func ClassifyDiscrepancy(block Block, expectedIndices, actualIndices []int, maxDeviation [3]int, coordResidual [2]int, spec *BitDepthSpec) DiscrepancyClassification {
	c := DiscrepancyClassification{BlockID: block.ID, CoordinateResidual: coordResidual, MaxDeviation: maxDeviation}
	coord := block.Coordinate()
	c.Coordinate = &coord
	if coordResidual[0] != 0 || coordResidual[1] != 0 || block.ResidualX != 0 || block.ResidualY != 0 {
		c.Level = LevelCoordinate
		c.Rule = "non_zero_floor_remainder_from_absolute_to_block_index"
		c.Evidence = append(c.Evidence, fmt.Sprintf("coordinate residual x/y=%d/%d", coordResidual[0], coordResidual[1]))
		return c
	}

	n := len(expectedIndices)
	if len(actualIndices) < n {
		n = len(actualIndices)
	}
	for i := 0; i < n; i++ {
		if expectedIndices[i] != actualIndices[i] {
			c.IndexMismatches++
		}
	}
	if c.IndexMismatches > 0 {
		c.Level = LevelPalette
		c.Rule = "truth_palette_index_differs_from_selected_index"
		c.Evidence = append(c.Evidence, fmt.Sprintf("%d/%d pixels select a different palette entry", c.IndexMismatches, n))
		return c
	}

	if spec != nil {
		bound, err := MaxBitDepthDeviation(*spec)
		if err == nil {
			c.BitDepthBound = bound
			within := true
			for channel := 0; channel < 3; channel++ {
				if maxDeviation[channel] > bound[channel] {
					within = false
				}
			}
			if within {
				c.Level = LevelBitDepth
				c.Rule = "indices_match_and_component_errors_fit_integer_bit_depth_envelope"
				c.Evidence = append(c.Evidence, fmt.Sprintf("max deviation %v within RGB bit-depth bound %v", maxDeviation, bound))
				return c
			}
		}
	}

	if maxDeviation != [3]int{} {
		c.Level = LevelPalette
		c.Rule = "indices_match_but_reconstructed_palette_value_or_gain_differs"
		c.Evidence = append(c.Evidence, "component deviation remains after index match")
		return c
	}
	c.Level = LevelNone
	c.Rule = "no_measured_discrepancy"
	return c
}

type ForensicTriage struct {
	Coordinate DiscrepancyClassification `json:"coordinate"`
	Palette    DiscrepancyClassification `json:"palette"`
	BitDepth   DiscrepancyClassification `json:"bitDepth"`
}

func RunForensicTriage(p *Project, rep *RunReport) ForensicTriage {
	byID := map[string]BlockOutcome{}
	for _, o := range rep.Outcomes {
		byID[o.Block.ID] = o
	}
	block := p.Blocks[3]
	out := byID[block.ID]

	coordBlock := block
	coordBlock.ResidualX = 3
	coord := ClassifyDiscrepancy(coordBlock, out.Quant.Indices, out.Quant.Indices, out.Evaluation.MaxDeviation, [2]int{3, 0}, nil)

	wrongIndices := append([]int(nil), out.Quant.Indices...)
	if len(wrongIndices) > 0 {
		wrongIndices[0] = (wrongIndices[0] + 1) % len(p.Palette.Entries)
	}
	palette := ClassifyDiscrepancy(block, out.Quant.Indices, wrongIndices, out.Evaluation.MaxDeviation, [2]int{}, nil)

	spec := RGB565Spec()
	im, _ := func() (Image, bool) {
		for _, im := range p.Images {
			if im.ID == block.ImageID {
				return im, true
			}
		}
		return Image{}, false
	}()
	maxBit := [3]int{}
	for y := 0; y < block.H; y++ {
		for x := 0; x < block.W; x++ {
			src := im.At(block.X+x, block.Y+y)
			_, res, err := QuantizePixelBitDepth(src, spec)
			if err == nil {
				for c := 0; c < 3; c++ {
					d := res[c]
					if d < 0 {
						d = -d
					}
					if d > maxBit[c] {
						maxBit[c] = d
					}
				}
			}
		}
	}
	bitDepth := ClassifyDiscrepancy(block, []int{1}, []int{1}, maxBit, [2]int{}, &spec)
	return ForensicTriage{Coordinate: coord, Palette: palette, BitDepth: bitDepth}
}
