package bake

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"sort"

	"terrainkernel/internal/fixed"
	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
	"terrainkernel/internal/schema"
	"terrainkernel/internal/terrain"
)

func EvalRawWeights(p *model.Project, px, py int) (int64, int64, []int64, error) {
	if p == nil {
		return 0, 0, nil, kerr.New(kerr.CodeSchema, "project", "nil project")
	}
	height := terrain.HeightMM(p.Seed, px, py)
	slope := terrain.SlopeURad(p.Seed, px, py)
	raw := make([]int64, len(p.Layers))
	for i := range p.Layers {
		layer := &p.Layers[i]
		driverValue := height
		if layer.Curve != nil && layer.Curve.Driver == "slope" {
			driverValue = slope
		}
		var value int64
		if layer.Curve != nil {
			value = schema.EvalCurve(layer.Curve, driverValue)
		} else {
			value = layer.Constant
		}
		if layer.Noise != nil {
			value += terrain.NoiseOffset(layer.Noise.Seed, layer.Noise.Amplitude, layer.Noise.ScaleMM, px, py)
		}
		if value < 0 {
			value = 0
		}
		if layer.SlopeCapURad > 0 && slope > layer.SlopeCapURad {
			value = 0
		}
		raw[i] = value
	}
	return height, slope, raw, nil
}

func LayerAnchors(p *model.Project) []int64 {
	anchors := make([]int64, len(p.Layers))
	if p.NormMode == "global" {
		for i := range anchors {
			anchors[i] = model.WeightScale
		}
		return anchors
	}
	for i := range p.Layers {
		layer := &p.Layers[i]
		var maxValue int64
		if layer.Curve != nil {
			for _, pt := range layer.Curve.Points {
				if pt.Y > maxValue {
					maxValue = pt.Y
				}
			}
		} else if layer.Constant > maxValue {
			maxValue = layer.Constant
		}
		if layer.Noise != nil && layer.Noise.Amplitude > maxValue {
			maxValue = layer.Noise.Amplitude
		}
		if maxValue <= 0 {
			maxValue = model.WeightScale
		}
		anchors[i] = maxValue
	}
	return anchors
}

func Normalize(mode string, raw, anchors []int64) ([]int64, error) {
	out := make([]int64, len(raw))
	if mode == "per_layer" {
		if len(anchors) != len(raw) {
			return nil, kerr.New(kerr.CodeSchema, "anchors", "anchor count does not match layers")
		}
		for i, w := range raw {
			if anchors[i] <= 0 {
				out[i] = 0
				continue
			}
			v, err := fixed.MulDiv(w, model.WeightScale, anchors[i], "normalization")
			if err != nil {
				return nil, err
			}
			out[i] = fixed.Clamp(v, 0, model.WeightScale)
		}
	} else {
		copy(out, raw)
	}
	return largestRemainder(out)
}

type remEntry struct {
	index     int
	remainder int64
}

func largestRemainder(weights []int64) ([]int64, error) {
	var sum int64
	for i, w := range weights {
		if w < 0 {
			return nil, kerr.New(kerr.CodeSchema, "normalization", "weight must be non-negative")
		}
		v, err := fixed.Add(sum, w, "normalization")
		if err != nil {
			return nil, err
		}
		sum = v
		_ = i
	}
	out := make([]int64, len(weights))
	if sum <= 0 {
		if len(out) > 0 {
			out[0] = model.WeightScale
		}
		return out, nil
	}
	rems := make([]remEntry, len(weights))
	var assigned int64
	for i, w := range weights {
		q, err := fixed.MulDiv(w, model.WeightScale, sum, "normalization")
		if err != nil {
			return nil, err
		}
		out[i] = q
		assigned += q
		scaled, err := fixed.Mul(w, model.WeightScale, "normalization")
		if err != nil {
			return nil, err
		}
		rems[i] = remEntry{index: i, remainder: scaled - q*sum}
	}
	left := model.WeightScale - assigned
	sort.SliceStable(rems, func(i, j int) bool {
		if rems[i].remainder == rems[j].remainder {
			return rems[i].index < rems[j].index
		}
		return rems[i].remainder > rems[j].remainder
	})
	for i := int64(0); i < left && i < int64(len(rems)); i++ {
		out[rems[i].index]++
	}
	return out, nil
}

func compositeBlend(layers []model.Layer, weights []int64) (int64, error) {
	order := make([]int, len(layers))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		if layers[order[i]].Order == layers[order[j]].Order {
			return order[i] < order[j]
		}
		return layers[order[i]].Order < layers[order[j]].Order
	})
	var sum int64
	for _, index := range order {
		term, err := fixed.Mul(layers[index].Tint, weights[index], "blend")
		if err != nil {
			return 0, err
		}
		sum, err = fixed.Add(sum, term, "blend")
		if err != nil {
			return 0, err
		}
	}
	return fixed.MulDivRounded(sum, 1, model.WeightScale, "blend", fixed.Nearest)
}

func BakePixel(p *model.Project, px, py int) (*Pixel, error) {
	return bakePixelWithAnchors(p, LayerAnchors(p), px, py)
}

func bakePixelWithAnchors(p *model.Project, anchors []int64, px, py int) (*Pixel, error) {
	height, slope, raw, err := EvalRawWeights(p, px, py)
	if err != nil {
		return nil, err
	}
	normalized, err := Normalize(p.NormMode, raw, anchors)
	if err != nil {
		return nil, err
	}
	dominant := 0
	for i := 1; i < len(normalized); i++ {
		if normalized[i] > normalized[dominant] {
			dominant = i
		}
	}
	blend, err := compositeBlend(p.Layers, normalized)
	if err != nil {
		return nil, err
	}
	return &Pixel{
		PX:        px,
		PY:        py,
		HeightMM:  height,
		SlopeURad: slope,
		Weights:   normalized,
		Dominant:  dominant,
		Blend:     blend,
	}, nil
}

func bakeChunk(p *model.Project, chunkID int) (*ChunkResult, error) {
	originX, originY, ok := p.ChunkOrigin(chunkID)
	if !ok {
		return nil, kerr.New(kerr.CodeSchema, "chunk_id", "chunk id out of range")
	}
	size := p.ChunkSizePx
	anchors := LayerAnchors(p)
	result := &ChunkResult{
		ChunkID:    chunkID,
		Size:       size,
		Dominants:  make([]int, 0, size*size),
		Blends:     make([]int64, 0, size*size),
		WeightRows: make([][]int64, 0, size*size),
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			pixel, err := bakePixelWithAnchors(p, anchors, originX+x, originY+y)
			if err != nil {
				return nil, err
			}
			result.WeightRows = append(result.WeightRows, append([]int64(nil), pixel.Weights...))
			result.Dominants = append(result.Dominants, pixel.Dominant)
			result.Blends = append(result.Blends, pixel.Blend)
		}
	}
	if err := result.ComputeHashes(); err != nil {
		return nil, err
	}
	return result, nil
}

// FullBake is the deterministic serial reference path.
func FullBake(p *model.Project) (map[int]*ChunkResult, []int, error) {
	results := make(map[int]*ChunkResult, p.ChunkCount())
	sequence := make([]int, 0, p.ChunkCount())
	for id := 0; id < p.ChunkCount(); id++ {
		result, err := bakeChunk(p, id)
		if err != nil {
			return nil, nil, err
		}
		results[id] = result
		sequence = append(sequence, id)
	}
	return results, sequence, nil
}

func ProjectHash(p *model.Project) string {
	if p == nil {
		return ""
	}
	data, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ChunkParamVersion hashes only the layer parameters that can affect one chunk.
// This lets an incremental edit preserve unaffected checkpoint rows.
func ChunkParamVersion(p *model.Project, chunkID int, deps []string) string {
	if p == nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(GlobalParamHash(p)))
	h.Write([]byte{0})
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(chunkID))
	h.Write(buf[:])
	anchors := LayerAnchors(p)
	for i, layer := range p.Layers {
		h.Write([]byte(layer.ID))
		h.Write([]byte{0})
		binary.BigEndian.PutUint64(buf[:], uint64(layer.Order))
		h.Write(buf[:])
		binary.BigEndian.PutUint64(buf[:], uint64(layer.Tint))
		h.Write(buf[:])
		binary.BigEndian.PutUint64(buf[:], uint64(anchors[i]))
		h.Write(buf[:])
	}
	if originX, originY, ok := p.ChunkOrigin(chunkID); ok {
		for y := 0; y < p.ChunkSizePx; y++ {
			for x := 0; x < p.ChunkSizePx; x++ {
				height, slope, raw, err := EvalRawWeights(p, originX+x, originY+y)
				if err != nil {
					continue
				}
				binary.BigEndian.PutUint64(buf[:], uint64(height))
				h.Write(buf[:])
				binary.BigEndian.PutUint64(buf[:], uint64(slope))
				h.Write(buf[:])
				for _, value := range raw {
					binary.BigEndian.PutUint64(buf[:], uint64(value))
					h.Write(buf[:])
				}
			}
		}
	}
	depCopy := append([]string(nil), deps...)
	sort.Strings(depCopy)
	for _, dep := range depCopy {
		h.Write([]byte(dep))
		h.Write([]byte{0})
	}
	return "param-" + hex.EncodeToString(h.Sum(nil)[:16])
}

// GlobalParamHash deliberately excludes editable curve/noise/cap/tint values.
// Those are versioned per chunk so unrelated chunks can remain checkpointed.
func GlobalParamHash(p *model.Project) string {
	if p == nil {
		return ""
	}
	type layerIdentity struct {
		ID    string
		Order int
	}
	payload := struct {
		ProjectID   string
		Seed        int64
		ChunkCols   int
		ChunkRows   int
		ChunkSizePx int
		Norm        string
		Layers      []layerIdentity
	}{
		ProjectID:   p.ProjectID,
		Seed:        p.Seed,
		ChunkCols:   p.ChunkCols,
		ChunkRows:   p.ChunkRows,
		ChunkSizePx: p.ChunkSizePx,
		Norm:        p.NormMode,
	}
	for _, layer := range p.Layers {
		payload.Layers = append(payload.Layers, layerIdentity{ID: layer.ID, Order: layer.Order})
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ChunkParamVersions(p *model.Project, graph *Graph) map[int]string {
	out := make(map[int]string, p.ChunkCount())
	for id := 0; id < p.ChunkCount(); id++ {
		deps := graph.ChunkDependencies(id)
		out[id] = ChunkParamVersion(p, id, deps)
	}
	return out
}
