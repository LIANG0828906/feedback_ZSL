package pipeline

import (
	"fmt"
	"sort"

	"volumekernel/internal/camera"
	"volumekernel/internal/kernel"
	"volumekernel/internal/model"
)

type NodeKind string

const (
	NodeDensity NodeKind = "density-voxel"
	NodeCoeff   NodeKind = "coefficient"
	NodeCamera  NodeKind = "camera"
	NodeRecipe  NodeKind = "recipe"
	NodeRay     NodeKind = "ray-segment"
	NodeBlock   NodeKind = "screen-block"
	NodePixel   NodeKind = "pixel"
)

type Node struct {
	ID       string   `json:"id"`
	Kind     NodeKind `json:"kind"`
	MediumID string   `json:"medium_id,omitempty"`
	BlockX   int      `json:"block_x,omitempty"`
	BlockY   int      `json:"block_y,omitempty"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type DependencyGraph struct {
	Nodes       map[string]Node  `json:"-"`
	Edges       []Edge           `json:"edges"`
	BlockDeps   map[int][]string `json:"block_deps"`
	BlockPixels map[int][]string `json:"block_pixels"`
	Plans       []BlockPlan      `json:"-"`
}

func BuildDependencyGraph(s model.SceneSnapshot, cameraIndex int) (DependencyGraph, error) {
	if cameraIndex < 0 || cameraIndex >= len(s.Scene.CameraTrack.Samples) {
		return DependencyGraph{}, model.ParamError("", "camera_index", "camera index %d out of range", cameraIndex)
	}
	plans := PlanBlocks(s.Scene.Viewport, s.Scene.Recipe)
	g := DependencyGraph{
		Nodes:       make(map[string]Node),
		BlockDeps:   make(map[int][]string),
		BlockPixels: make(map[int][]string),
		Plans:       plans,
	}
	sample := s.Scene.CameraTrack.Samples[cameraIndex]
	edgeSet := make(map[Edge]struct{})
	addEdge := func(from, to string) {
		e := Edge{From: from, To: to}
		if _, ok := edgeSet[e]; !ok {
			edgeSet[e] = struct{}{}
			g.Edges = append(g.Edges, e)
		}
	}
	for _, plan := range plans {
		blockID := blockNodeID(plan.X, plan.Y)
		g.Nodes[blockID] = Node{ID: blockID, Kind: NodeBlock, BlockX: plan.X, BlockY: plan.Y}
		depSet := make(map[string]struct{})
		for _, p := range plan.Pixels {
			ray, err := camera.RayForPixel(sample, s.Scene.Viewport, p.X, p.Y)
			if err != nil {
				return DependencyGraph{}, err
			}
			deps := kernel.Dependencies(s.Scene, ray)
			deps = append(deps, "camera:"+s.Scene.CameraTrack.ID+":"+sample.ID, "recipe:"+s.Scene.Recipe.Version)
			sort.Strings(deps)
			pixelID := fmt.Sprintf("pixel:%d,%d", p.X, p.Y)
			g.Nodes[pixelID] = Node{ID: pixelID, Kind: NodePixel}
			addEdge(blockID, pixelID)
			g.BlockPixels[plan.Index] = append(g.BlockPixels[plan.Index], pixelID)
			for _, dep := range deps {
				depSet[dep] = struct{}{}
				node, ok := g.Nodes[dep]
				if !ok {
					node = Node{ID: dep, Kind: depKind(dep), MediumID: depMedium(dep)}
					g.Nodes[dep] = node
				}
				addEdge(dep, ray.ID)
			}
			g.Nodes[ray.ID] = Node{ID: ray.ID, Kind: NodeRay, BlockX: plan.X, BlockY: plan.Y}
			addEdge(ray.ID, blockID)
		}
		deps := make([]string, 0, len(depSet))
		for d := range depSet {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		g.BlockDeps[plan.Index] = deps
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		return g.Edges[i].To < g.Edges[j].To
	})
	return g, nil
}

func blockNodeID(x, y int) string { return fmt.Sprintf("block:%d,%d", x, y) }

func depKind(dep string) NodeKind {
	switch {
	case len(dep) >= 6 && dep[:6] == "voxel:":
		return NodeDensity
	case len(dep) >= 6 && dep[:6] == "coeff:":
		return NodeCoeff
	case len(dep) >= 7 && dep[:7] == "camera:":
		return NodeCamera
	case len(dep) >= 7 && dep[:7] == "recipe:":
		return NodeRecipe
	default:
		return NodeCoeff
	}
}

func depMedium(dep string) string {
	parts := splitN(dep, ':', 3)
	if len(parts) >= 2 {
		switch parts[0] {
		case "voxel", "coeff", "medium":
			return parts[1]
		}
	}
	return ""
}

func splitN(s string, sep byte, n int) []string {
	var out []string
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
