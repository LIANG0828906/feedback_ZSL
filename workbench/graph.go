package workbench

import (
	"fmt"
	"strings"

	"yuvench/filters"
	"yuvench/yuv"
)

func BuildGraph(image *yuv.Image, params filters.Params) (*Graph, error) {
	if image == nil {
		return nil, fmt.Errorf("missing image")
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	specs := []struct {
		id    string
		plane string
		stage string
		deps  []string
	}{
		{filters.NodeYDenoise, "Y", "denoise", nil},
		{filters.NodeYSharpen, "Y", "sharpen", []string{filters.NodeYDenoise}},
		{filters.NodeYContrast, "Y", "contrast", []string{filters.NodeYSharpen}},
		{filters.NodeUSmooth, "U", "smooth", nil},
		{filters.NodeUGain, "U", "gain", []string{filters.NodeUSmooth}},
		{filters.NodeVSmooth, "V", "smooth", nil},
		{filters.NodeVGain, "V", "gain", []string{filters.NodeVSmooth}},
		{filters.NodeComposite, "YUV", "interleave", []string{filters.NodeYContrast, filters.NodeUGain, filters.NodeVGain}},
	}
	graph := &Graph{
		ByID:          make(map[string]*Node, len(specs)),
		FinalID:       filters.NodeComposite,
		InputHash:     image.InputHash,
		ParamVersion:  params.ImageParamsHash(),
		ChromaVersion: image.ChromaVersion,
	}
	for seq, spec := range specs {
		paramsJSON, err := params.StageParamsJSON(spec.id)
		if err != nil {
			return nil, err
		}
		paramsHash, err := params.StageParamsHash(spec.id)
		if err != nil {
			return nil, err
		}
		inputHash := image.PlaneHashes[spec.plane]
		if spec.plane == "YUV" {
			inputHash = yuv.HashBytes(
				[]byte("composite-input-v1"),
				[]byte(image.PlaneHashes["Y"]),
				[]byte(image.PlaneHashes["U"]),
				[]byte(image.PlaneHashes["V"]),
				[]byte(image.Meta.ChromaAlignment),
				[]byte(image.Meta.Upsample),
			)
		}
		graph.Nodes = append(graph.Nodes, &Node{
			Seq:           seq,
			ID:            spec.id,
			Plane:         spec.plane,
			Stage:         spec.stage,
			Deps:          append([]string(nil), spec.deps...),
			InputHash:     inputHash,
			ParamsHash:    paramsHash,
			ParamsJSON:    paramsJSON,
			ChromaVersion: image.ChromaVersion,
		})
	}
	for _, node := range graph.Nodes {
		graph.ByID[node.ID] = node
	}
	for _, node := range graph.Nodes {
		upstream := make([]string, 0, len(node.Deps))
		for _, dep := range node.Deps {
			upstream = append(upstream, graph.ByID[dep].CacheKey)
		}
		node.CacheKey = computeCacheKey(image, node, upstream)
	}
	return graph, nil
}

func computeCacheKey(image *yuv.Image, node *Node, upstream []string) string {
	parts := [][]byte{
		[]byte("stage-cache-v1"),
		[]byte(AlgorithmVersion),
		[]byte(node.Plane),
		[]byte(node.Stage),
		[]byte(node.InputHash),
		node.ParamsJSON,
		[]byte(image.Meta.ChromaAlignment),
		[]byte(image.Meta.Upsample),
		[]byte(node.ChromaVersion),
		[]byte(strings.Join(upstream, ",")),
	}
	return yuv.HashBytes(parts...)
}

func ComputeRunID(meta SnapshotMeta) string {
	return yuv.HashBytes(
		[]byte(meta.SchemaVersion),
		[]byte(meta.AlgorithmVersion),
		[]byte(meta.ParamVersion),
		[]byte(meta.ChromaVersion),
		[]byte(meta.InputHash),
		[]byte(meta.ManifestHash),
	)
}

func EvaluateInvalidations(graph *Graph, previous map[string]*CacheEntry, hardReason string) ([]Invalidation, map[string]bool) {
	invalid := make(map[string]bool, len(graph.Nodes))
	out := make([]Invalidation, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		reason := ""
		if hardReason != "" {
			reason = hardReason
		} else {
			entry := previous[node.ID]
			if entry == nil {
				reason = "missing"
			} else if entry.InputHash != node.InputHash {
				reason = "input-changed"
			} else if entry.ChromaVersion != node.ChromaVersion {
				reason = "chroma-alignment-changed"
			} else if entry.ParamsHash != node.ParamsHash {
				reason = "parameter-changed"
			} else if entry.CacheKey != node.CacheKey {
				reason = "cache-key-changed"
				for _, dep := range node.Deps {
					if invalid[dep] && previous[dep] != nil && graph.ByID[dep].CacheKey != previous[dep].CacheKey {
						reason = "upstream-cache-key-changed:" + dep
						break
					}
				}
			}
		}
		if reason != "" {
			invalid[node.ID] = true
			out = append(out, Invalidation{
				Plane:  node.Plane,
				Stage:  node.Stage,
				Node:   node.ID,
				Reason: reason,
			})
			continue
		}
		for _, dep := range node.Deps {
			if invalid[dep] {
				reason = "upstream-cache-key-changed:" + dep
				invalid[node.ID] = true
				out = append(out, Invalidation{
					Plane:  node.Plane,
					Stage:  node.Stage,
					Node:   node.ID,
					Reason: reason,
				})
				break
			}
		}
	}
	return out, invalid
}
