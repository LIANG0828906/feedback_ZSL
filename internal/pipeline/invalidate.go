package pipeline

import (
	"fmt"
	"sort"

	"volumekernel/internal/checkpoint"
	"volumekernel/internal/model"
)

type EditKind string

const (
	EditVoxel      EditKind = "density-voxel"
	EditScattering EditKind = "scattering"
	EditAbsorption EditKind = "absorption"
	EditCamera     EditKind = "camera"
	EditRecipe     EditKind = "recipe"
)

type Edit struct {
	Kind     EditKind `json:"kind"`
	MediumID string   `json:"medium_id"`
	IX       int      `json:"ix"`
	IY       int      `json:"iy"`
	IZ       int      `json:"iz"`
	Reason   string   `json:"reason"`
}

type Invalidation struct {
	NodeID   string   `json:"node_id"`
	NodeKind NodeKind `json:"node_kind"`
	MediumID string   `json:"medium_id,omitempty"`
	BlockX   int      `json:"block_x"`
	BlockY   int      `json:"block_y"`
	Pixels   []string `json:"pixels"`
	Reason   string   `json:"reason"`
}

type InvalidationReport struct {
	Edit                 Edit           `json:"edit"`
	Entries              []Invalidation `json:"entries"`
	AffectedBlocks       []string       `json:"affected_blocks"`
	AffectedPixels       int            `json:"affected_pixels"`
	Unaffected           int            `json:"unaffected_blocks"`
	CacheRowsDeleted     int64          `json:"cache_rows_deleted"`
	CacheHitsAfterEdit   int            `json:"cache_hits_after_edit"`
	CacheMissesAfterEdit int            `json:"cache_misses_after_edit"`
}

func InvalidateForEdit(graph DependencyGraph, edit Edit, store *checkpoint.Store) (InvalidationReport, error) {
	report := InvalidationReport{Edit: edit}
	starts, reason := changedNodes(graph, edit)
	if len(starts) == 0 {
		return report, nil
	}
	adj := make(map[string][]string)
	for _, e := range graph.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	seen := make(map[string]bool)
	queue := append([]string(nil), starts...)
	for _, s := range queue {
		seen[s] = true
	}
	affectedBlocks := make(map[string]struct{})
	affectedPixels := make(map[string]struct{})
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		node := graph.Nodes[id]
		switch node.Kind {
		case NodeBlock:
			affectedBlocks[id] = struct{}{}
		case NodePixel:
			affectedPixels[id] = struct{}{}
		}
		for _, to := range adj[id] {
			if !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	var blockCoords [][2]int
	blockIDs := make([]string, 0, len(affectedBlocks))
	for id := range affectedBlocks {
		blockIDs = append(blockIDs, id)
		n := graph.Nodes[id]
		blockCoords = append(blockCoords, [2]int{n.BlockX, n.BlockY})
	}
	sort.Strings(blockIDs)
	for _, id := range blockIDs {
		n := graph.Nodes[id]
		pixels := append([]string(nil), graph.BlockPixels[blockIndex(graph, n.BlockX, n.BlockY)]...)
		sort.Strings(pixels)
		entry := Invalidation{
			NodeID:   id,
			NodeKind: NodeBlock,
			MediumID: edit.MediumID,
			BlockX:   n.BlockX,
			BlockY:   n.BlockY,
			Pixels:   pixels,
			Reason:   reason,
		}
		if edit.Reason != "" {
			entry.Reason += ": " + edit.Reason
		}
		report.Entries = append(report.Entries, entry)
	}
	report.AffectedBlocks = blockIDs
	report.AffectedPixels = len(affectedPixels)
	report.Unaffected = len(graph.Plans) - len(affectedBlocks)
	if store != nil && len(blockCoords) > 0 {
		n, err := store.DeleteCacheForBlocks(blockCoords)
		if err != nil {
			return report, err
		}
		report.CacheRowsDeleted = n
	}
	return report, nil
}

func changedNodes(graph DependencyGraph, edit Edit) ([]string, string) {
	switch edit.Kind {
	case EditVoxel:
		id := fmt.Sprintf("voxel:%s:%d,%d,%d", edit.MediumID, edit.IX, edit.IY, edit.IZ)
		return []string{id}, fmt.Sprintf("density voxel %s[%d,%d,%d] changed", edit.MediumID, edit.IX, edit.IY, edit.IZ)
	case EditScattering:
		return []string{"coeff:" + edit.MediumID + ":scattering"}, "scattering coefficient of " + edit.MediumID + " changed"
	case EditAbsorption:
		return []string{"coeff:" + edit.MediumID + ":absorption"}, "absorption coefficient of " + edit.MediumID + " changed"
	case EditCamera:
		var ids []string
		for id, n := range graph.Nodes {
			if n.Kind == NodeCamera {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		return ids, "camera parameters changed"
	case EditRecipe:
		var ids []string
		for id, n := range graph.Nodes {
			if n.Kind == NodeRecipe {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		return ids, "recipe parameters changed"
	default:
		return nil, "unknown edit"
	}
}

func blockIndex(g DependencyGraph, x, y int) int {
	for _, p := range g.Plans {
		if p.X == x && p.Y == y {
			return p.Index
		}
	}
	return -1
}

func ApplyEdit(s model.Scene, edit Edit) model.Scene {
	cp := s
	cp.Media = append([]model.Medium(nil), s.Media...)
	for i := range cp.Media {
		cp.Media[i].Density.Values = append([]uint8(nil), s.Media[i].Density.Values...)
	}
	cp.CameraTrack.Samples = append([]model.CameraSample(nil), s.CameraTrack.Samples...)
	for i := range cp.Media {
		m := &cp.Media[i]
		if m.ID != edit.MediumID {
			continue
		}
		switch edit.Kind {
		case EditVoxel:
			if edit.IX >= 0 && edit.IY >= 0 && edit.IZ >= 0 &&
				edit.IX < m.Density.NX && edit.IY < m.Density.NY && edit.IZ < m.Density.NZ {
				idx := (edit.IZ*m.Density.NY+edit.IY)*m.Density.NX + edit.IX
				old := m.Density.Values[idx]
				if old > 0 {
					m.Density.Values[idx] = old - 1
				} else {
					m.Density.Values[idx] = 1
				}
			}
		case EditScattering:
			m.Scattering += 0.01
		case EditAbsorption:
			m.Absorption += 0.01
		}
	}
	if edit.Kind == EditCamera && len(cp.CameraTrack.Samples) > 0 {
		cp.CameraTrack.Samples[0].Position.X += 0.01
	}
	if edit.Kind == EditRecipe {
		cp.Recipe.Version += "-edited"
	}
	return cp
}
