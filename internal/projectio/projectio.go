package projectio

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"terrainnormalverify/internal/model"
)

func Load(path string) (*model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		e := model.NewError(model.ErrIO, -1, "path", err.Error())
		e.Value = path
		return nil, e
	}
	var p model.Project
	if err := json.Unmarshal(data, &p); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && strings.Contains(typeErr.Field, "weights") {
			blockID, field, ok := firstInvalidWeight(data)
			if !ok {
				blockID = -1
				field = typeErr.Field
			}
			e := model.NewError(model.ErrWeight, blockID, field,
				"quantized weights must be integers in [0,1000]")
			e.Value = typeErr.Value
			return nil, e
		}
		e := model.NewError(model.ErrSchema, -1, "$", "malformed project JSON: "+err.Error())
		return nil, e
	}
	if err := Validate(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func firstInvalidWeight(data []byte) (int, string, bool) {
	var envelope struct {
		Blocks []struct {
			ID      int     `json:"id"`
			Weights [][]any `json:"weights"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return -1, "", false
	}
	for _, block := range envelope.Blocks {
		for y, row := range block.Weights {
			for x, value := range row {
				number, ok := value.(float64)
				if !ok || math.Trunc(number) != number || number < math.MinInt32 || number > math.MaxInt32 {
					return block.ID, fmt.Sprintf("weights[%d][%d]", y, x), true
				}
			}
		}
	}
	return -1, "", false
}

func Save(path string, p *model.Project) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return model.NewError(model.ErrIO, -1, "project", err.Error())
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return model.NewError(model.ErrIO, -1, "path", err.Error())
	}
	return nil
}

func Validate(p *model.Project) error {
	if p == nil {
		return model.NewError(model.ErrSchema, -1, "project", "project is nil")
	}
	if p.SchemaVersion == "" {
		return model.NewError(model.ErrSchema, -1, "schemaVersion", "field is required")
	}
	if p.SchemaVersion != model.SchemaVersion {
		e := model.NewError(model.ErrSchema, -1, "schemaVersion", "unsupported schema version")
		e.Value = p.SchemaVersion
		e.Want = model.SchemaVersion
		return e
	}
	if p.World.Unit == "" {
		return model.NewError(model.ErrSchema, -1, "world.unit", "field is required")
	}
	if p.World.HeightScale <= 0 || math.IsNaN(p.World.HeightScale) || math.IsInf(p.World.HeightScale, 0) {
		e := model.NewError(model.ErrPara, -1, "world.heightScale", "must be finite and positive")
		e.Value = p.World.HeightScale
		return e
	}
	if err := validateParameters(p.Parameters); err != nil {
		return err
	}
	if len(p.Layers) == 0 {
		return model.NewError(model.ErrSchema, -1, "layers", "at least one material layer is required")
	}
	layerIDs := make(map[string]bool, len(p.Layers))
	for i, layer := range p.Layers {
		if layer.ID == "" {
			return model.NewError(model.ErrSchema, -1, fmt.Sprintf("layers[%d].id", i), "field is required")
		}
		if layer.Name == "" {
			return model.NewError(model.ErrSchema, -1, fmt.Sprintf("layers[%d].name", i), "field is required")
		}
		if layerIDs[layer.ID] {
			e := model.NewError(model.ErrSchema, -1, fmt.Sprintf("layers[%d].id", i), "duplicate layer id")
			e.Value = layer.ID
			return e
		}
		layerIDs[layer.ID] = true
		for _, value := range []float64{layer.NormalBiasX, layer.NormalBiasY, layer.NormalBiasZ, layer.Strength} {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return model.NewError(model.ErrSchema, -1, fmt.Sprintf("layers[%d]", i), "numeric values must be finite")
			}
		}
		if layer.Strength <= 0 || layer.Strength > 4 {
			e := model.NewError(model.ErrPara, -1, fmt.Sprintf("layers[%d].strength", i), "must be in (0, 4]")
			e.Value = layer.Strength
			return e
		}
	}
	if len(p.Blocks) == 0 {
		return model.NewError(model.ErrSchema, -1, "blocks", "at least one terrain block is required")
	}
	blocks := make(map[int]*model.Block, len(p.Blocks))
	for i := range p.Blocks {
		b := &p.Blocks[i]
		if b.ID <= 0 {
			return model.NewError(model.ErrSchema, b.ID, fmt.Sprintf("blocks[%d].id", i), "must be positive")
		}
		if _, exists := blocks[b.ID]; exists {
			return model.NewError(model.ErrSchema, b.ID, "blocks.id", "duplicate block id")
		}
		if b.Name == "" {
			return model.NewError(model.ErrSchema, b.ID, "name", "field is required")
		}
		if b.Width <= 0 {
			return model.NewError(model.ErrSize, b.ID, "width", "must be positive")
		}
		if b.Height <= 0 {
			return model.NewError(model.ErrSize, b.ID, "height", "must be positive")
		}
		if b.Heights == nil {
			return model.NewError(model.ErrSchema, b.ID, "heights", "field is required")
		}
		if b.Weights == nil {
			return model.NewError(model.ErrSchema, b.ID, "weights", "field is required")
		}
		px := b.Width * b.Height
		if len(b.Heights) != px {
			e := model.NewError(model.ErrSize, b.ID, "heights", "height sample count must equal width*height")
			e.Value = len(b.Heights)
			e.Want = px
			return e
		}
		for j, h := range b.Heights {
			if math.IsNaN(h) || math.IsInf(h, 0) {
				e := model.NewError(model.ErrSchema, b.ID, fmt.Sprintf("heights[%d]", j), "height must be finite")
				return e
			}
		}
		if len(b.Weights) != px {
			e := model.NewError(model.ErrSize, b.ID, "weights", "weight row count must equal width*height")
			e.Value = len(b.Weights)
			e.Want = px
			return e
		}
		for j, row := range b.Weights {
			if len(row) != len(p.Layers) {
				e := model.NewError(model.ErrSize, b.ID, fmt.Sprintf("weights[%d]", j), "weight row must match layer count")
				e.Value = len(row)
				e.Want = len(p.Layers)
				return e
			}
			sum := 0
			for k, weight := range row {
				if weight < 0 || weight > model.WeightScale {
					e := model.NewError(model.ErrWeight, b.ID, fmt.Sprintf("weights[%d][%d]", j, k), "weight is outside [0,1000]")
					e.Value = weight
					return e
				}
				sum += weight
			}
			if sum == 0 {
				e := model.NewError(model.ErrWeight, b.ID, fmt.Sprintf("weights[%d]", j), "quantized weights must have a positive row sum")
				e.Value = sum
				return e
			}
		}
		blocks[b.ID] = b
	}
	if len(p.Seams) == 0 {
		return model.NewError(model.ErrSchema, -1, "seams", "at least one shared-edge constraint is required")
	}
	seamIDs := make(map[string]bool, len(p.Seams))
	for i, seam := range p.Seams {
		field := fmt.Sprintf("seams[%d]", i)
		if seam.ID == "" {
			return model.NewError(model.ErrSchema, -1, field+".id", "field is required")
		}
		if seamIDs[seam.ID] {
			return model.NewError(model.ErrSchema, -1, field+".id", "duplicate seam id")
		}
		seamIDs[seam.ID] = true
		a, okA := blocks[seam.BlockA]
		b, okB := blocks[seam.BlockB]
		if !okA {
			e := model.NewError(model.ErrSchema, seam.BlockA, field+".blockA", "referenced adjacent block does not exist")
			e.Value = seam.BlockA
			return e
		}
		if !okB {
			e := model.NewError(model.ErrSchema, seam.BlockB, field+".blockB", "referenced adjacent block does not exist")
			e.Value = seam.BlockB
			return e
		}
		if seam.BlockA == seam.BlockB {
			return model.NewError(model.ErrSchema, seam.BlockA, field+".blockB", "a seam must reference two different blocks")
		}
		if seam.ThresholdDeg <= 0 || seam.ThresholdDeg >= 180 {
			e := model.NewError(model.ErrSchema, seam.BlockA, field+".thresholdDeg", "must be in (0,180)")
			e.Value = seam.ThresholdDeg
			return e
		}
		if seam.MaxIterations < 0 || seam.MaxIterations > 10000 {
			e := model.NewError(model.ErrSchema, seam.BlockA, field+".maxIterations", "must be in [0,10000]")
			e.Value = seam.MaxIterations
			return e
		}
		if seam.GeometrySlopeThresholdDeg <= 0 || seam.GeometrySlopeThresholdDeg >= 90 {
			e := model.NewError(model.ErrSchema, seam.BlockA, field+".geometrySlopeThresholdDeg", "must be in (0,90)")
			e.Value = seam.GeometrySlopeThresholdDeg
			return e
		}
		switch seam.Axis {
		case "vertical":
			if seam.EdgeA != "east" || seam.EdgeB != "west" {
				return model.NewError(model.ErrSchema, seam.BlockA, field+".edgeA", "vertical seam requires east/west edges")
			}
			if a.Height != b.Height {
				e := model.NewError(model.ErrSize, seam.BlockA, field+".edgeSamples", "shared-edge height sample column lengths differ")
				e.Value = a.Height
				e.Want = b.Height
				return e
			}
			if a.OriginX+a.Width-1 != b.OriginX || a.OriginY != b.OriginY {
				return model.NewError(model.ErrSchema, seam.BlockA, field+".origin", "vertical seam edges are not coincident")
			}
		case "horizontal":
			if seam.EdgeA != "south" || seam.EdgeB != "north" {
				return model.NewError(model.ErrSchema, seam.BlockA, field+".edgeA", "horizontal seam requires south/north edges")
			}
			if a.Width != b.Width {
				e := model.NewError(model.ErrSize, seam.BlockA, field+".edgeSamples", "shared-edge height sample row lengths differ")
				e.Value = a.Width
				e.Want = b.Width
				return e
			}
			if a.OriginY+a.Height-1 != b.OriginY || a.OriginX != b.OriginX {
				return model.NewError(model.ErrSchema, seam.BlockA, field+".origin", "horizontal seam edges are not coincident")
			}
		default:
			e := model.NewError(model.ErrSchema, seam.BlockA, field+".axis", "must be vertical or horizontal")
			e.Value = seam.Axis
			return e
		}
	}
	return validateTruth(p, blocks, seamIDs)
}

func validateParameters(p model.BakeParameters) error {
	switch p.DifferenceOperator {
	case "central", "forward", "sobel":
	default:
		e := model.NewError(model.ErrPara, -1, "parameters.differenceOperator", "must be central, forward, or sobel")
		e.Value = p.DifferenceOperator
		return e
	}
	if p.Scale < 1e-6 || p.Scale > 1e4 || math.IsNaN(p.Scale) || math.IsInf(p.Scale, 0) {
		e := model.NewError(model.ErrPara, -1, "parameters.scale", "must be finite and in [1e-6,1e4]")
		e.Value = p.Scale
		return e
	}
	if p.MaterialMix != "weighted" && p.MaterialMix != "dominant" {
		e := model.NewError(model.ErrPara, -1, "parameters.materialMix", "must be weighted or dominant")
		e.Value = p.MaterialMix
		return e
	}
	if p.SeamRelaxation <= 0 || p.SeamRelaxation > 0.5 || math.IsNaN(p.SeamRelaxation) {
		e := model.NewError(model.ErrPara, -1, "parameters.seamRelaxation", "must be in (0,0.5]")
		e.Value = p.SeamRelaxation
		return e
	}
	return nil
}

func validateTruth(p *model.Project, blocks map[int]*model.Block, seams map[string]bool) error {
	if len(p.Truth.Blocks) == 0 {
		return model.NewError(model.ErrSchema, -1, "truth.blocks", "analytic truth samples are required")
	}
	seen := make(map[int]bool)
	for i, bt := range p.Truth.Blocks {
		b, ok := blocks[bt.BlockID]
		if !ok {
			e := model.NewError(model.ErrSchema, bt.BlockID, fmt.Sprintf("truth.blocks[%d].blockId", i), "truth references unknown block")
			return e
		}
		if seen[bt.BlockID] {
			return model.NewError(model.ErrSchema, bt.BlockID, fmt.Sprintf("truth.blocks[%d].blockId", i), "duplicate truth block")
		}
		seen[bt.BlockID] = true
		if len(bt.Pixels) == 0 {
			return model.NewError(model.ErrSchema, bt.BlockID, "truth.blocks.pixels", "at least one truth pixel is required")
		}
		for j, px := range bt.Pixels {
			field := fmt.Sprintf("truth.blocks[%d].pixels[%d]", i, j)
			if !b.ValidXY(px.X, px.Y) {
				e := model.NewError(model.ErrSize, bt.BlockID, field, "truth pixel is outside block bounds")
				e.Value = [2]int{px.X, px.Y}
				return e
			}
			if math.Abs(px.Normal.Length()-1) > 1e-6 {
				e := model.NewError(model.ErrSchema, bt.BlockID, field+".normal", "truth normal must be unit length")
				e.Value = px.Normal.Length()
				return e
			}
			if px.SlopeDeg < 0 || px.SlopeDeg >= 90 {
				e := model.NewError(model.ErrSchema, bt.BlockID, field+".slopeDeg", "must be in [0,90)")
				e.Value = px.SlopeDeg
				return e
			}
			if px.Tolerance <= 0 || px.Tolerance > 0.5 {
				e := model.NewError(model.ErrSchema, bt.BlockID, field+".tolerance", "must be in (0,0.5]")
				e.Value = px.Tolerance
				return e
			}
		}
	}
	order := make([]int, 0, len(blocks))
	for id := range blocks {
		order = append(order, id)
	}
	sort.Ints(order)
	for _, id := range order {
		if !seen[id] {
			return model.NewError(model.ErrSchema, id, "truth.blocks", "block has no analytic truth samples")
		}
	}
	if len(p.Truth.Seams) == 0 {
		return model.NewError(model.ErrSchema, -1, "truth.seams", "seam truth entries are required")
	}
	seenSeam := make(map[string]bool)
	for i, st := range p.Truth.Seams {
		if !seams[st.SeamID] {
			e := model.NewError(model.ErrSchema, -1, fmt.Sprintf("truth.seams[%d].seamId", i), "truth references unknown seam")
			e.Value = st.SeamID
			return e
		}
		if seenSeam[st.SeamID] {
			return model.NewError(model.ErrSchema, -1, fmt.Sprintf("truth.seams[%d].seamId", i), "duplicate seam truth")
		}
		seenSeam[st.SeamID] = true
		if st.ExpectedAdjacentAngleDeg < 0 || st.ExpectedAdjacentAngleDeg >= 180 || math.IsNaN(st.ExpectedAdjacentAngleDeg) {
			e := model.NewError(model.ErrSchema, -1, fmt.Sprintf("truth.seams[%d].expectedAdjacentAngleDeg", i), "must be in [0,180)")
			e.Value = st.ExpectedAdjacentAngleDeg
			return e
		}
		if st.ToleranceDeg <= 0 || st.ToleranceDeg > 10 || math.IsNaN(st.ToleranceDeg) {
			e := model.NewError(model.ErrSchema, -1, fmt.Sprintf("truth.seams[%d].toleranceDeg", i), "must be in (0,10]")
			e.Value = st.ToleranceDeg
			return e
		}
		for _, category := range st.ExpectedCategories {
			switch category {
			case "geometric_fold", "material_mix", "difference_operator":
			default:
				e := model.NewError(model.ErrSchema, -1, fmt.Sprintf("truth.seams[%d].expectedCategories", i), "unknown attribution category")
				e.Value = category
				return e
			}
		}
	}
	for _, seam := range p.Seams {
		if !seenSeam[seam.ID] {
			return model.NewError(model.ErrSchema, seam.BlockA, "truth.seams", "seam has no truth entry")
		}
	}
	return nil
}

func AsError(err error) *model.Error {
	var e *model.Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
