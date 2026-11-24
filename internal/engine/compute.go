package engine

import (
	"fmt"

	"fixedsampler/internal/fixedpoint"
	"fixedsampler/internal/model"
)

type ComputeInput struct {
	Project    *model.Project
	Stage      model.StageConfig
	ChunkIndex int
	ChunkX     int
	ChunkY     int
	Rects      map[string]model.Rect
	PrePlanes  map[string]*model.Plane
}

func ComputeNode(in ComputeInput) (*model.LevelArtifact, error) {
	art := &model.LevelArtifact{Seq: in.Stage.Seq, Type: in.Stage.Type, Config: in.Stage}
	switch in.Stage.Type {
	case "resample":
		if err := computeResample(in, art); err != nil {
			return nil, err
		}
	case "color":
		if err := computeColor(in, art); err != nil {
			return nil, err
		}
	case "quantize":
		if err := computeQuantize(in, art); err != nil {
			return nil, err
		}
	default:
		return nil, model.SchemaError("unsupported stage type %q", in.Stage.Type)
	}
	return art, nil
}

func addTrunc(art *model.LevelArtifact, in ComputeInput, rec model.TruncRecord) {
	rec.StageSeq = in.Stage.Seq
	rec.ChunkX = in.ChunkX
	rec.ChunkY = in.ChunkY
	art.Truncations = append(art.Truncations, rec)
}

func computeResample(in ComputeInput, art *model.LevelArtifact) error {
	stage := in.Stage
	scale := stage.FixedScale
	fx, fy := stage.FactorX, stage.FactorY
	taps, normResidual := kernelWeights(stage.Kernel, fx, fy, scale)
	if taps == nil {
		return model.KernelError(stage.Seq, "unknown kernel %q", stage.Kernel)
	}
	for _, name := range stage.Planes {
		plane := in.PrePlanes[name]
		if plane == nil {
			return model.SchemaError("stage %d missing pre-plane %q", stage.Seq, name)
		}
		rect := in.Rects[name]
		values := make([]int64, 0, rect.Width()*rect.Height())
		addTrunc(art, in, model.TruncRecord{
			Plane: name, Kind: "weight_normalization", Numerator: scale,
			Denominator: int64(fx * fy), Remainder: normResidual,
		})
		for oy := rect.Y0; oy < rect.Y1; oy++ {
			for ox := rect.X0; ox < rect.X1; ox++ {
				var acc int64
				for ti, tap := range taps {
					sx := ox*fx + tap.OffsetX
					sy := oy*fy + tap.OffsetY
					if sx < 0 || sy < 0 || sx >= plane.Width || sy >= plane.Height {
						return model.DimError(name, stage.Factor, "kernel sample (%d,%d) outside plane %dx%d for output (%d,%d)",
							sx, sy, plane.Width, plane.Height, ox, oy)
					}
					v := plane.Values[sy*plane.Width+sx]
					acc += v * tap.Weight
					_ = ti
				}
				out, rem := fixedpoint.DivModFloor(acc, scale)
				values = append(values, out)
				addTrunc(art, in, model.TruncRecord{
					Plane: name, Kind: "downsample_accum", OutputX: ox, OutputY: oy,
					Numerator: acc, Denominator: scale, Remainder: rem,
				})
			}
		}
		art.Tiles = append(art.Tiles, model.Tile{Plane: name, Rect: rect, Values: values})
	}
	return nil
}

func computeColor(in ComputeInput, art *model.LevelArtifact) error {
	stage := in.Stage
	m := stage.Matrix
	if m == nil {
		return model.SchemaError("stage %d missing fixed matrix", stage.Seq)
	}
	rect := in.Rects[stage.OutputNames[0]]
	inputs := make([]*model.Plane, 3)
	for i, name := range stage.InputNames {
		inputs[i] = in.PrePlanes[name]
		if inputs[i] == nil {
			return model.SchemaError("stage %d missing input plane %q", stage.Seq, name)
		}
	}
	outputs := make([][]int64, 3)
	for i := range outputs {
		outputs[i] = make([]int64, 0, rect.Width()*rect.Height())
	}
	for y := rect.Y0; y < rect.Y1; y++ {
		for x := rect.X0; x < rect.X1; x++ {
			var v [3]int64
			for i, plane := range inputs {
				if x >= plane.Width || y >= plane.Height {
					return model.DimError(stage.InputNames[i], 0, "color sample (%d,%d) outside %dx%d", x, y, plane.Width, plane.Height)
				}
				v[i] = plane.Values[y*plane.Width+x]
			}
			for row := 0; row < 3; row++ {
				num := m.Offsets[row] * m.Scale
				for col := 0; col < 3; col++ {
					num += m.Rows[row][col] * v[col]
				}
				out, rem := fixedpoint.DivModFloor(num, m.Scale)
				outputs[row] = append(outputs[row], out)
				addTrunc(art, in, model.TruncRecord{
					Plane: stage.OutputNames[row], Kind: "color_quant", OutputX: x, OutputY: y,
					TapIndex: row, Numerator: num, Denominator: m.Scale, Remainder: rem,
				})
			}
		}
	}
	for i, name := range stage.OutputNames {
		art.Tiles = append(art.Tiles, model.Tile{Plane: name, Rect: rect, Values: outputs[i]})
	}
	return nil
}

func computeQuantize(in ComputeInput, art *model.LevelArtifact) error {
	stage := in.Stage
	q := in.Project.ValueScale
	outMax := fixedpoint.MaxCode(stage.BitDepth)
	for _, name := range stage.Planes {
		plane := in.PrePlanes[name]
		if plane == nil {
			return model.SchemaError("stage %d missing quant plane %q", stage.Seq, name)
		}
		inMax := fixedpoint.MaxCode(plane.BitDepth)
		den := q * inMax
		if den <= 0 {
			return fmt.Errorf("stage %d invalid quant denominator q=%d in_max=%d", stage.Seq, q, inMax)
		}
		rect := in.Rects[name]
		values := make([]int64, 0, rect.Width()*rect.Height())
		for y := rect.Y0; y < rect.Y1; y++ {
			for x := rect.X0; x < rect.X1; x++ {
				v := plane.Values[y*plane.Width+x]
				num := v * outMax
				floored, rem := fixedpoint.DivModFloor(num, den)
				code := floored
				if 2*rem >= den {
					code++
				}
				if code < 0 {
					code = 0
				}
				if code > outMax {
					code = outMax
				}
				out := code * q
				values = append(values, out)
				addTrunc(art, in, model.TruncRecord{
					Plane: name, Kind: "bitdepth_round", OutputX: x, OutputY: y,
					Numerator: num, Denominator: den, Remainder: rem,
				})
			}
		}
		art.Tiles = append(art.Tiles, model.Tile{Plane: name, Rect: rect, Values: values})
	}
	return nil
}
