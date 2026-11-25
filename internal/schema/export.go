package schema

import (
	"encoding/json"
	"fmt"

	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
)

const (
	v1BinHeightMM  int64 = 100
	v1BinSlopeURad int64 = 5_000
)

// Marshal exports the internal model to one concrete wire version. When the
// target cannot express a feature, the returned bytes are still a valid lossy
// export and the error is a *kerr.LossError.
func Marshal(p *model.Project, version int) ([]byte, error) {
	if p == nil {
		return nil, kerr.New(kerr.CodeSchema, "project", "nil project")
	}
	if err := validateLayers(p.Layers, styleInternal); err != nil {
		return nil, err
	}
	var (
		wire interface{}
		loss []kerr.LossItem
		err  error
	)
	switch version {
	case 1:
		wire, loss, err = marshalV1(p)
	case 2:
		wire, loss, err = marshalV2(p)
	case 3:
		wire, loss, err = marshalV3(p)
	default:
		return nil, kerr.New(kerr.CodeVersion, "format_version", fmt.Sprintf("unknown target version %d", version))
	}
	if err != nil {
		return nil, err
	}
	data, err := canonicalJSON(wire)
	if err != nil {
		return nil, err
	}
	if len(loss) > 0 {
		return data, &kerr.LossError{Items: loss}
	}
	return data, nil
}

func canonicalJSON(wire interface{}) ([]byte, error) {
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, kerr.New(kerr.CodeSchema, "$", "JSON export failed: "+err.Error())
	}
	return append(data, '\n'), nil
}

func marshalV1(p *model.Project) (V1Wire, []kerr.LossItem, error) {
	wire := V1Wire{
		FormatVersion: 1,
		ProjectID:     p.ProjectID,
		HeightUnit:    "cm",
		SlopeUnit:     "rad",
		Normalization: p.NormMode,
		Seed:          p.Seed,
		ChunkCols:     p.ChunkCols,
		ChunkRows:     p.ChunkRows,
		ChunkSizePx:   p.ChunkSizePx,
		Layers:        make([]V1Layer, 0, len(p.Layers)),
	}
	var loss []kerr.LossItem
	for i := range p.Layers {
		layer := &p.Layers[i]
		out := V1Layer{LayerID: layer.ID, Order: layer.Order, Tint: layer.Tint}
		if layer.Curve == nil || len(layer.Curve.Points) == 0 {
			if layer.Expr == "" {
				return wire, nil, kerr.New(kerr.CodeSchema, layerPath(i, "curvePoints"), "missing curve")
			}
			out.WeightTable = []int64{layer.Constant}
			out.BinHeightCM = 1
		} else if layer.Curve.Driver == "slope" || layer.Curve.Driver == "" && slopeDominates(layer.Curve) {
			table, origin, bin, err := flattenSlope(layer.Curve)
			if err != nil {
				return wire, nil, err
			}
			out.WeightTable = nil
			out.BinHeightCM = 0
			out.SlopeTable = table
			out.BinSlopeRad = bin
			out.SlopeOriginRad = origin
			loss = append(loss, kerr.LossItem{
				Path:   layerPath(i, "curve_points"),
				Kind:   "piecewise_linear_curve",
				Action: "quantized",
				Detail: "piecewise-linear curve quantized to v1 constant bins",
			})
		} else {
			table, originCM, err := flattenHeight(layer.Curve)
			if err != nil {
				return wire, nil, err
			}
			out.WeightTable = table
			out.BinHeightCM = 10
			out.HeightOriginCM = originCM
			loss = append(loss, kerr.LossItem{
				Path:   layerPath(i, "curve_points"),
				Kind:   "piecewise_linear_curve",
				Action: "quantized",
				Detail: "piecewise-linear curve quantized to v1 constant bins",
			})
		}
		if layer.Noise != nil {
			loss = append(loss, kerr.LossItem{
				Path:   layerPath(i, "noise"),
				Kind:   "noise",
				Action: "dropped",
				Detail: "v1 has no noise perturbation layer",
			})
		}
		if layer.SlopeCapURad != 0 {
			loss = append(loss, kerr.LossItem{
				Path:   layerPath(i, "slopeCapDeg"),
				Kind:   "slope_cap",
				Action: "dropped",
				Detail: "v1 has no slope cap; mask disabled",
			})
		}
		wire.Layers = append(wire.Layers, out)
	}
	return wire, loss, nil
}

// slopeDominates exists only for malformed internal models with no driver. The
// generator and loaders always set a driver, so normal exports are unambiguous.
func slopeDominates(curve *model.Curve) bool {
	if curve == nil || len(curve.Points) == 0 {
		return false
	}
	return curve.Points[len(curve.Points)-1].X > 1_000_000
}

func flattenHeight(curve *model.Curve) ([]int64, int64, error) {
	first := curve.Points[0].X
	last := curve.Points[len(curve.Points)-1].X
	originCM := first / 10
	originMM := originCM * 10
	span := last - originMM
	if span < 0 {
		span = 0
	}
	n := span/v1BinHeightMM + 1
	if n <= 0 || n > 10_000_000 {
		return nil, 0, kerr.New(kerr.CodeOverflow, "weight_table", "flattened table length overflow")
	}
	table := make([]int64, n)
	for i := int64(0); i < n; i++ {
		table[i] = EvalCurve(curve, originMM+i*v1BinHeightMM)
	}
	return table, originCM, nil
}

func flattenSlope(curve *model.Curve) ([]int64, int64, int64, error) {
	first := curve.Points[0].X
	last := curve.Points[len(curve.Points)-1].X
	span := last - first
	if span < 0 {
		span = 0
	}
	n := span/v1BinSlopeURad + 1
	if n <= 0 || n > 10_000_000 {
		return nil, 0, 0, kerr.New(kerr.CodeOverflow, "slope_table", "flattened table length overflow")
	}
	table := make([]int64, n)
	for i := int64(0); i < n; i++ {
		table[i] = EvalCurve(curve, first+i*v1BinSlopeURad)
	}
	return table, first, v1BinSlopeURad, nil
}

func marshalV2(p *model.Project) (V2Wire, []kerr.LossItem, error) {
	wire := V2Wire{
		FormatVer:     2,
		ProjectId:     p.ProjectID,
		HeightUnit:    "mm",
		SlopeUnit:     "deg",
		Normalization: p.NormMode,
		Seed:          p.Seed,
		ChunkCols:     p.ChunkCols,
		ChunkRows:     p.ChunkRows,
		ChunkSizePx:   p.ChunkSizePx,
		Layers:        make([]V2Layer, 0, len(p.Layers)),
	}
	var loss []kerr.LossItem
	capURad := p.Layers[0].SlopeCapURad
	capsDiffer := false
	for i := range p.Layers {
		if p.Layers[i].SlopeCapURad != capURad {
			capsDiffer = true
		}
	}
	if capsDiffer {
		loss = append(loss, kerr.LossItem{
			Path:   "slopeCapDeg",
			Kind:   "per_layer_slope_cap",
			Action: "collapsed",
			Detail: "v2 supports a single project-level cap; differing per-layer caps collapse",
		})
		capURad = 0
	}
	if capURad > 0 {
		deg, err := URadToSlope(capURad, "deg", "slopeCapDeg")
		if err != nil {
			return wire, nil, err
		}
		wire.SlopeCapDeg = &deg
	}
	for i := range p.Layers {
		layer := &p.Layers[i]
		driver := "height"
		if layer.Curve != nil && layer.Curve.Driver != "" {
			driver = layer.Curve.Driver
		}
		out := V2Layer{
			LayerId:     layer.ID,
			Order:       layer.Order,
			Driver:      driver,
			CurvePoints: make([]V2CurvePt, 0, curveLen(layer.Curve)),
			Tint:        layer.Tint,
		}
		if layer.Curve != nil {
			for _, pt := range layer.Curve.Points {
				x := pt.X
				if driver == "slope" {
					var err error
					x, err = URadToSlope(x, "deg", fmt.Sprintf("%s[%d].x", layerPath(i, "curvePoints"), len(out.CurvePoints)))
					if err != nil {
						return wire, nil, err
					}
				}
				out.CurvePoints = append(out.CurvePoints, V2CurvePt{X: x, Y: pt.Y})
			}
		}
		if layer.Noise != nil {
			loss = append(loss, kerr.LossItem{
				Path:   layerPath(i, "noise"),
				Kind:   "noise",
				Action: "dropped",
				Detail: "v2 has no per-layer noise; perturbation removed",
			})
		}
		wire.Layers = append(wire.Layers, out)
	}
	return wire, loss, nil
}

func marshalV3(p *model.Project) (V3Wire, []kerr.LossItem, error) {
	wire := V3Wire{
		FormatVersion: 3,
		ProjectID:     p.ProjectID,
		HeightUnit:    "m",
		SlopeUnit:     "deg",
		Normalization: p.NormMode,
		Seed:          p.Seed,
		ChunkCols:     p.ChunkCols,
		ChunkRows:     p.ChunkRows,
		ChunkSizePx:   p.ChunkSizePx,
		Layers:        make([]V3Layer, 0, len(p.Layers)),
	}
	for i := range p.Layers {
		layer := &p.Layers[i]
		driver := "height"
		if layer.Curve != nil && layer.Curve.Driver != "" {
			driver = layer.Curve.Driver
		}
		out := V3Layer{
			LayerID:     layer.ID,
			Order:       layer.Order,
			Driver:      driver,
			CurvePoints: make([]V3CurvePt, 0, curveLen(layer.Curve)),
			Tint:        layer.Tint,
		}
		if layer.Curve != nil {
			for _, pt := range layer.Curve.Points {
				x := pt.X
				var err error
				if driver == "slope" {
					x, err = URadToSlope(x, "deg", fmt.Sprintf("%s[%d].x", layerPath(i, "curvePoints"), len(out.CurvePoints)))
				} else {
					x, err = MMToHeight(x, "m", fmt.Sprintf("%s[%d].x", layerPath(i, "curvePoints"), len(out.CurvePoints)))
				}
				if err != nil {
					return wire, nil, err
				}
				out.CurvePoints = append(out.CurvePoints, V3CurvePt{X: x, Y: pt.Y})
			}
		}
		if layer.Noise != nil {
			scale, err := MMToHeight(layer.Noise.ScaleMM, "m", layerPath(i, "Noise.ScaleM"))
			if err != nil {
				return wire, nil, err
			}
			out.Noise = &V3Noise{
				Seed:      layer.Noise.Seed,
				Amplitude: layer.Noise.Amplitude,
				ScaleM:    scale,
			}
		}
		if layer.SlopeCapURad > 0 {
			deg, err := URadToSlope(layer.SlopeCapURad, "deg", layerPath(i, "SlopeCapDeg"))
			if err != nil {
				return wire, nil, err
			}
			out.SlopeCapDeg = &deg
		}
		wire.Layers = append(wire.Layers, out)
	}
	return wire, nil, nil
}

func curveLen(curve *model.Curve) int {
	if curve == nil {
		return 0
	}
	return len(curve.Points)
}
