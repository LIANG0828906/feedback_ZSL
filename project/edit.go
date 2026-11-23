package project

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"terrain-bake/internal/atomicfs"
)

func loadEdits(root string, p *Project) ([]EditRecord, *Error) {
	path := filepath.Join(root, "edits.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, NewError(CodeDecode, "", "editsFile", "read edits file: %v", err)
	}
	var wf wireEditFile
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, NewError(CodeDecode, "", "editsFile", "decode edits file: %v", err)
	}
	if wf.SchemaVersion == nil {
		return nil, missing("", "edits.schemaVersion")
	}
	if *wf.SchemaVersion != 1 {
		return nil, NewError(CodeSchema, "", "edits.schemaVersion", "unsupported edits schema version %d", *wf.SchemaVersion)
	}
	if wf.Edits == nil {
		return nil, missing("", "edits.edits")
	}
	edits := make([]EditRecord, 0, len(*wf.Edits))
	for i := range *wf.Edits {
		w := (*wf.Edits)[i]
		if w.Seq == nil {
			return nil, missing("", fmt.Sprintf("edits[%d].seq", i))
		}
		if w.Block == nil {
			return nil, missing("", fmt.Sprintf("edits[%d].block", i))
		}
		if w.Kind == nil {
			return nil, missing(*w.Block, fmt.Sprintf("edits[%d].kind", i))
		}
		if w.Col == nil {
			return nil, missing(*w.Block, fmt.Sprintf("edits[%d].col", i))
		}
		if w.Row == nil {
			return nil, missing(*w.Block, fmt.Sprintf("edits[%d].row", i))
		}
		if w.Value == nil {
			return nil, missing(*w.Block, fmt.Sprintf("edits[%d].value", i))
		}
		if *w.Kind == "material" && w.Layer == nil {
			return nil, missing(*w.Block, fmt.Sprintf("edits[%d].layer", i))
		}
		rec := EditRecord{Seq: *w.Seq, Block: *w.Block, Kind: *w.Kind, Col: *w.Col, Row: *w.Row, Value: *w.Value}
		if w.Layer != nil {
			rec.Layer = *w.Layer
		}
		bd, ok := p.Blocks[rec.Block]
		if !ok {
			return nil, NewError(CodeSchema, rec.Block, fmt.Sprintf("edits[%d].block", i), "unknown block")
		}
		switch rec.Kind {
		case "height":
			if rec.Col < 0 || rec.Col >= bd.Def.Grid.Cols || rec.Row < 0 || rec.Row >= bd.Def.Grid.Rows {
				return nil, NewError(CodeSize, rec.Block, fmt.Sprintf("edits[%d]", i), "height edit coordinate out of range")
			}
			if !finite(rec.Value) {
				return nil, NewError(CodeSchema, rec.Block, fmt.Sprintf("edits[%d].value", i), "height value must be finite")
			}
			bd.Height[rec.Row*bd.Def.Grid.Cols+rec.Col] = rec.Value
		case "material":
			if rec.Layer < 0 || rec.Layer >= bd.Def.Material.Layers {
				return nil, NewError(CodeSchema, rec.Block, fmt.Sprintf("edits[%d].layer", i), "material layer out of range")
			}
			if rec.Col < 0 || rec.Col >= bd.Def.Grid.Cols || rec.Row < 0 || rec.Row >= bd.Def.Grid.Rows {
				return nil, NewError(CodeSize, rec.Block, fmt.Sprintf("edits[%d]", i), "material edit coordinate out of range")
			}
			if rec.Value != math.Trunc(rec.Value) {
				return nil, NewError(CodeWeight, rec.Block, fmt.Sprintf("edits[%d].value", i), "material weight must be an integer")
			}
			target := int(rec.Value)
			if target < 0 || target > bd.Def.Material.QuantMax {
				return nil, NewError(CodeWeight, rec.Block, fmt.Sprintf("edits[%d].value", i), "material weight %d outside [0,%d]", target, bd.Def.Material.QuantMax)
			}
			if e := rebalanceMaterialCell(bd, rec.Col, rec.Row, rec.Layer, target); e != nil {
				return nil, e
			}
		default:
			return nil, NewError(CodeSchema, rec.Block, fmt.Sprintf("edits[%d].kind", i), "kind must be height or material")
		}
		edits = append(edits, rec)
	}
	return edits, nil
}

func (p *Project) ApplyHeightEdit(block string, col, row int, value float64) *Error {
	bd, ok := p.Blocks[block]
	if !ok {
		return NewError(CodeSchema, block, "block", "unknown block")
	}
	if col < 0 || col >= bd.Def.Grid.Cols || row < 0 || row >= bd.Def.Grid.Rows {
		return NewError(CodeSize, block, "col,row", "height edit coordinate (%d,%d) is outside %dx%d", col, row, bd.Def.Grid.Cols, bd.Def.Grid.Rows)
	}
	if !finite(value) {
		return NewError(CodeSchema, block, "height", "height value must be finite")
	}
	bd.Height[row*bd.Def.Grid.Cols+col] = value
	rec := EditRecord{Seq: len(p.Edits) + 1, Block: block, Kind: "height", Col: col, Row: row, Value: value}
	p.Edits = append(p.Edits, rec)
	p.TruthStale = true
	p.RecomputeContentHash()
	return nil
}

func (p *Project) ApplyMaterialEdit(block string, col, row, layer, weight int) *Error {
	bd, ok := p.Blocks[block]
	if !ok {
		return NewError(CodeSchema, block, "block", "unknown block")
	}
	if layer < 0 || layer >= bd.Def.Material.Layers {
		return NewError(CodeSchema, block, "layer", "material layer %d is outside [0,%d)", layer, bd.Def.Material.Layers)
	}
	if col < 0 || col >= bd.Def.Grid.Cols || row < 0 || row >= bd.Def.Grid.Rows {
		return NewError(CodeSize, block, "col,row", "material edit coordinate (%d,%d) is outside %dx%d", col, row, bd.Def.Grid.Cols, bd.Def.Grid.Rows)
	}
	if weight < 0 || weight > bd.Def.Material.QuantMax {
		return NewError(CodeWeight, block, "material.weight", "material weight %d outside [0,%d]", weight, bd.Def.Material.QuantMax)
	}
	if e := rebalanceMaterialCell(bd, col, row, layer, weight); e != nil {
		return e
	}
	rec := EditRecord{Seq: len(p.Edits) + 1, Block: block, Kind: "material", Col: col, Row: row, Layer: layer, Value: float64(weight)}
	p.Edits = append(p.Edits, rec)
	p.TruthStale = true
	p.RecomputeContentHash()
	return nil
}

func rebalanceMaterialCell(bd *BlockData, col, row, layer, target int) *Error {
	base := (row*bd.Def.Material.Cols + col) * bd.Def.Material.Layers
	layers := bd.Def.Material.Layers
	quant := bd.Def.Material.QuantMax
	others := make([]int, layers-1)
	otherSum := 0
	k := 0
	for i := 0; i < layers; i++ {
		if i == layer {
			continue
		}
		others[k] = int(bd.Weights[base+i])
		otherSum += others[k]
		k++
	}
	remaining := quant - target
	if remaining < 0 {
		return NewError(CodeWeight, "", "material.weight", "target weight exceeds quantization maximum")
	}
	if len(others) == 0 {
		if target != quant {
			return NewError(CodeWeight, bd.Def.ID, "material.weight", "single-layer material must sum to quantMax")
		}
		bd.Weights[base+layer] = uint16(target)
		return nil
	}
	weights := make([]int, len(others))
	if otherSum == 0 {
		for i := range weights {
			weights[i] = remaining / len(others)
		}
		weights[0] += remaining - (remaining/len(others))*len(others)
	} else {
		fractions := make([]float64, len(others))
		allocated := 0
		for i, w := range others {
			exact := float64(remaining) * float64(w) / float64(otherSum)
			weights[i] = int(math.Floor(exact))
			fractions[i] = exact - float64(weights[i])
			allocated += weights[i]
		}
		left := remaining - allocated
		for left > 0 {
			best := -1
			for i := range weights {
				if weights[i] >= quant {
					continue
				}
				if best == -1 || fractions[i] > fractions[best] || (fractions[i] == fractions[best] && i < best) {
					best = i
				}
			}
			if best < 0 {
				return NewError(CodeWeight, bd.Def.ID, "material.weights", "unable to rebalance material weights")
			}
			weights[best]++
			fractions[best] = -1
			left--
		}
	}
	bd.Weights[base+layer] = uint16(target)
	k = 0
	for i := 0; i < layers; i++ {
		if i == layer {
			continue
		}
		if weights[k] < 0 || weights[k] > quant {
			return NewError(CodeWeight, bd.Def.ID, "material.weights", "rebalanced weight %d outside [0,%d]", weights[k], quant)
		}
		bd.Weights[base+i] = uint16(weights[k])
		k++
	}
	return nil
}

func WriteEdits(root string, edits []EditRecord) *Error {
	payload := EditFile{SchemaVersion: 1, Edits: edits}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return NewError(CodeCache, "", "editsFile", "encode edits: %v", err)
	}
	data = append(data, '\n')
	if err := atomicfs.WriteFile(filepath.Join(root, "edits.json"), data, 0o644); err != nil {
		return NewError(CodeCache, "", "editsFile", "write edits atomically: %v", err)
	}
	return nil
}

func (p *Project) Clone() *Project {
	clone := &Project{
		Root:        p.Root,
		Manifest:    p.Manifest,
		Truth:       p.Truth,
		Blocks:      make(map[string]*BlockData, len(p.Blocks)),
		BlockOrder:  append([]string(nil), p.BlockOrder...),
		Edits:       append([]EditRecord(nil), p.Edits...),
		ContentHash: p.ContentHash,
		TruthStale:  p.TruthStale,
	}
	clone.Manifest.Materials = append([]Material(nil), p.Manifest.Materials...)
	clone.Manifest.Blocks = append([]Block(nil), p.Manifest.Blocks...)
	clone.Manifest.Seams = append([]Seam(nil), p.Manifest.Seams...)
	clone.Truth.HeightSamples = append([]HeightTruth(nil), p.Truth.HeightSamples...)
	clone.Truth.MaterialRatios = append([]RatioTruth(nil), p.Truth.MaterialRatios...)
	clone.Truth.InstanceCounts = append([]CountTruth(nil), p.Truth.InstanceCounts...)
	clone.Truth.Seams = append([]SeamAngleTruth(nil), p.Truth.Seams...)
	for id, bd := range p.Blocks {
		clone.Blocks[id] = &BlockData{
			Def:     bd.Def,
			Height:  append([]float64(nil), bd.Height...),
			Weights: append([]uint16(nil), bd.Weights...),
			Mask:    append([]byte(nil), bd.Mask...),
		}
	}
	return clone
}
