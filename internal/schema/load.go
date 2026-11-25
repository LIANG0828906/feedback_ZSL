package schema

import (
	"encoding/json"
	"fmt"

	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
)

func loadV1(data []byte) (*model.Project, error) {
	var wire V1Wire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, kerr.New(kerr.CodeSchema, "$", "invalid v1 JSON: "+err.Error())
	}
	if err := requireString(wire.HeightUnit, "height_unit"); err != nil {
		return nil, err
	}
	if wire.HeightUnit != "cm" {
		return nil, kerr.New(kerr.CodeUnit, "height_unit", fmt.Sprintf("v1 expects cm, got %q", wire.HeightUnit))
	}
	if err := requireString(wire.SlopeUnit, "slope_unit"); err != nil {
		return nil, err
	}
	if wire.SlopeUnit != "rad" {
		return nil, kerr.New(kerr.CodeUnit, "slope_unit", fmt.Sprintf("v1 expects rad, got %q", wire.SlopeUnit))
	}
	if err := validateProjectHeader(wire.ProjectID, wire.Normalization, wire.ChunkCols, wire.ChunkRows, wire.ChunkSizePx, styleV1); err != nil {
		return nil, err
	}
	p := &model.Project{
		SourceVersion: 1,
		ProjectID:     wire.ProjectID,
		NormMode:      wire.Normalization,
		Seed:          wire.Seed,
		ChunkCols:     wire.ChunkCols,
		ChunkRows:     wire.ChunkRows,
		ChunkSizePx:   wire.ChunkSizePx,
		Layers:        make([]model.Layer, 0, len(wire.Layers)),
	}
	for i, lw := range wire.Layers {
		layer := model.Layer{
			ID:    lw.LayerID,
			Order: lw.Order,
			Tint:  lw.Tint,
		}
		switch {
		case len(lw.WeightTable) > 0:
			if lw.BinHeightCM <= 0 {
				return nil, kerr.New(kerr.CodeSchema, layerPath(i, "bin_height_cm"), "required positive integer")
			}
			originMM, err := HeightToMM(lw.HeightOriginCM, "cm", layerPath(i, "height_origin_cm"))
			if err != nil {
				return nil, err
			}
			stepMM, err := HeightToMM(lw.BinHeightCM, "cm", layerPath(i, "bin_height_cm"))
			if err != nil {
				return nil, err
			}
			points := make([]model.CurvePoint, len(lw.WeightTable))
			for j, weight := range lw.WeightTable {
				if weight < 0 {
					return nil, kerr.New(kerr.CodeSchema, fmt.Sprintf("%s[%d]", layerPath(i, "weight_table"), j), "weight must be non-negative")
				}
				points[j] = model.CurvePoint{X: originMM + int64(j)*stepMM, Y: weight}
			}
			layer.Curve = &model.Curve{Driver: "height", Points: points}
		case len(lw.SlopeTable) > 0:
			if lw.BinSlopeRad <= 0 {
				return nil, kerr.New(kerr.CodeSchema, layerPath(i, "bin_slope_rad"), "required positive integer")
			}
			origin, err := SlopeToURad(lw.SlopeOriginRad, wire.SlopeUnit, layerPath(i, "slope_origin_rad"))
			if err != nil {
				return nil, err
			}
			step, err := SlopeToURad(lw.BinSlopeRad, wire.SlopeUnit, layerPath(i, "bin_slope_rad"))
			if err != nil {
				return nil, err
			}
			points := make([]model.CurvePoint, len(lw.SlopeTable))
			for j, weight := range lw.SlopeTable {
				if weight < 0 {
					return nil, kerr.New(kerr.CodeSchema, fmt.Sprintf("%s[%d]", layerPath(i, "slope_table"), j), "weight must be non-negative")
				}
				points[j] = model.CurvePoint{X: origin + int64(j)*step, Y: weight}
			}
			layer.Curve = &model.Curve{Driver: "slope", Points: points}
		default:
			return nil, kerr.New(kerr.CodeSchema, layerPath(i, "weight_table"), "missing weight table")
		}
		p.Layers = append(p.Layers, layer)
	}
	if err := validateLayers(p.Layers, styleV1); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2(data []byte) (*model.Project, error) {
	var wire V2Wire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, kerr.New(kerr.CodeSchema, "$", "invalid v2 JSON: "+err.Error())
	}
	if err := requireString(wire.HeightUnit, "heightUnit"); err != nil {
		return nil, err
	}
	if wire.HeightUnit != "mm" {
		return nil, kerr.New(kerr.CodeUnit, "heightUnit", fmt.Sprintf("v2 expects mm, got %q", wire.HeightUnit))
	}
	if err := requireString(wire.SlopeUnit, "slopeUnit"); err != nil {
		return nil, err
	}
	if wire.SlopeUnit != "deg" {
		return nil, kerr.New(kerr.CodeUnit, "slopeUnit", fmt.Sprintf("v2 expects deg, got %q", wire.SlopeUnit))
	}
	if err := validateProjectHeader(wire.ProjectId, wire.Normalization, wire.ChunkCols, wire.ChunkRows, wire.ChunkSizePx, styleV2); err != nil {
		return nil, err
	}
	var capURad int64
	if wire.SlopeCapDeg != nil {
		var err error
		capURad, err = SlopeToURad(*wire.SlopeCapDeg, wire.SlopeUnit, "slopeCapDeg")
		if err != nil {
			return nil, err
		}
		if capURad < 0 {
			return nil, kerr.New(kerr.CodeSchema, "slopeCapDeg", "slope cap must be non-negative")
		}
	}
	p := &model.Project{
		SourceVersion: 2,
		ProjectID:     wire.ProjectId,
		NormMode:      wire.Normalization,
		Seed:          wire.Seed,
		ChunkCols:     wire.ChunkCols,
		ChunkRows:     wire.ChunkRows,
		ChunkSizePx:   wire.ChunkSizePx,
		Layers:        make([]model.Layer, 0, len(wire.Layers)),
	}
	for i, lw := range wire.Layers {
		curve, err := loadCurve(i, lw.Driver, len(lw.CurvePoints), func(j int) (int64, int64) {
			pt := lw.CurvePoints[j]
			return pt.X, pt.Y
		}, wire.HeightUnit, wire.SlopeUnit, "curvePoints")
		if err != nil {
			return nil, err
		}
		p.Layers = append(p.Layers, model.Layer{
			ID:           lw.LayerId,
			Order:        lw.Order,
			Curve:        curve,
			Tint:         lw.Tint,
			SlopeCapURad: capURad,
		})
	}
	if err := validateLayers(p.Layers, styleV2); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV3(data []byte) (*model.Project, error) {
	var wire V3Wire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, kerr.New(kerr.CodeSchema, "$", "invalid v3 JSON: "+err.Error())
	}
	if err := requireString(wire.HeightUnit, "HeightUnit"); err != nil {
		return nil, err
	}
	if wire.HeightUnit != "m" {
		return nil, kerr.New(kerr.CodeUnit, "HeightUnit", fmt.Sprintf("v3 expects m, got %q", wire.HeightUnit))
	}
	if err := requireString(wire.SlopeUnit, "SlopeUnit"); err != nil {
		return nil, err
	}
	if wire.SlopeUnit != "deg" {
		return nil, kerr.New(kerr.CodeUnit, "SlopeUnit", fmt.Sprintf("v3 expects deg, got %q", wire.SlopeUnit))
	}
	if err := validateProjectHeader(wire.ProjectID, wire.Normalization, wire.ChunkCols, wire.ChunkRows, wire.ChunkSizePx, styleV3); err != nil {
		return nil, err
	}
	p := &model.Project{
		SourceVersion: 3,
		ProjectID:     wire.ProjectID,
		NormMode:      wire.Normalization,
		Seed:          wire.Seed,
		ChunkCols:     wire.ChunkCols,
		ChunkRows:     wire.ChunkRows,
		ChunkSizePx:   wire.ChunkSizePx,
		Layers:        make([]model.Layer, 0, len(wire.Layers)),
	}
	for i, lw := range wire.Layers {
		curve, err := loadCurve(i, lw.Driver, len(lw.CurvePoints), func(j int) (int64, int64) {
			pt := lw.CurvePoints[j]
			return pt.X, pt.Y
		}, wire.HeightUnit, wire.SlopeUnit, "CurvePoints")
		if err != nil {
			return nil, err
		}
		layer := model.Layer{
			ID:    lw.LayerID,
			Order: lw.Order,
			Curve: curve,
			Tint:  lw.Tint,
		}
		if lw.Noise != nil {
			scaleMM, err := HeightToMM(lw.Noise.ScaleM, wire.HeightUnit, layerPath(i, "Noise.ScaleM"))
			if err != nil {
				return nil, err
			}
			layer.Noise = &model.Noise{
				Seed:      lw.Noise.Seed,
				Amplitude: lw.Noise.Amplitude,
				ScaleMM:   scaleMM,
			}
		}
		if lw.SlopeCapDeg != nil {
			capURad, err := SlopeToURad(*lw.SlopeCapDeg, wire.SlopeUnit, layerPath(i, "SlopeCapDeg"))
			if err != nil {
				return nil, err
			}
			if capURad < 0 {
				return nil, kerr.New(kerr.CodeSchema, layerPath(i, "SlopeCapDeg"), "slope cap must be non-negative")
			}
			layer.SlopeCapURad = capURad
		}
		p.Layers = append(p.Layers, layer)
	}
	if err := validateLayers(p.Layers, styleV3); err != nil {
		return nil, err
	}
	return p, nil
}

func loadCurve(index int, driver string, n int, point func(int) (int64, int64), heightUnit, slopeUnit, curveField string) (*model.Curve, error) {
	switch driver {
	case "height", "slope":
	case "":
		return nil, kerr.New(kerr.CodeSchema, layerPath(index, "driver"), "required field is empty")
	default:
		return nil, kerr.New(kerr.CodeSchema, layerPath(index, "driver"), "unrecognized driver "+quote(driver))
	}
	if n == 0 {
		return nil, kerr.New(kerr.CodeSchema, layerPath(index, curveField), "curve has no points")
	}
	points := make([]model.CurvePoint, n)
	for j := 0; j < n; j++ {
		x, y := point(j)
		path := fmt.Sprintf("%s[%d].X", layerPath(index, curveField), j)
		if curveField == "curvePoints" {
			path = fmt.Sprintf("%s[%d].x", layerPath(index, curveField), j)
		}
		var err error
		if driver == "height" {
			x, err = HeightToMM(x, heightUnit, path)
		} else {
			x, err = SlopeToURad(x, slopeUnit, path)
		}
		if err != nil {
			return nil, err
		}
		if y < 0 {
			ypath := fmt.Sprintf("%s[%d].y", layerPath(index, curveField), j)
			if curveField == "CurvePoints" {
				ypath = fmt.Sprintf("%s[%d].Y", layerPath(index, curveField), j)
			}
			return nil, kerr.New(kerr.CodeSchema, ypath, "weight must be non-negative")
		}
		points[j] = model.CurvePoint{X: x, Y: y}
	}
	return &model.Curve{Driver: driver, Points: points}, nil
}
