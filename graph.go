package fog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type DependencyNode struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`
	MediumID string     `json:"medium_id,omitempty"`
	Tile     *TileCoord `json:"tile,omitempty"`
	Voxel    *[3]int    `json:"voxel,omitempty"`
	Hash     string     `json:"hash,omitempty"`
}

type DependencyEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type DependencyGraph struct {
	Nodes map[string]DependencyNode `json:"nodes"`
	Edges []DependencyEdge          `json:"edges"`
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{Nodes: map[string]DependencyNode{}}
}

func (g *DependencyGraph) addNode(node DependencyNode) {
	if _, exists := g.Nodes[node.ID]; !exists {
		g.Nodes[node.ID] = node
	}
}

func (g *DependencyGraph) addEdge(from, to, reason string) {
	g.addNode(DependencyNode{ID: from})
	g.addNode(DependencyNode{ID: to})
	g.Edges = append(g.Edges, DependencyEdge{From: from, To: to, Reason: reason})
}

func VoxelWorldBounds(medium *Medium, x, y, z int) AABB {
	local := medium.Density.VoxelBounds(x, y, z)
	minBase := medium.Bounds.Min
	return AABB{
		Min: FixedVec3{X: minBase.X + local.Min.X, Y: minBase.Y + local.Min.Y, Z: minBase.Z + local.Min.Z},
		Max: FixedVec3{X: minBase.X + local.Max.X, Y: minBase.Y + local.Max.Y, Z: minBase.Z + local.Max.Z},
	}
}

func BuildDependencyGraph(scene *Scene) *DependencyGraph {
	graph := NewDependencyGraph()
	cameraNode := "camera:" + scene.CameraTrack.ID + ":v" + fmt.Sprint(scene.CameraTrack.Version)
	recipeNode := "recipe:" + scene.Recipe.ID + ":v" + fmt.Sprint(scene.Recipe.Version)
	graph.addNode(DependencyNode{ID: cameraNode, Kind: "camera", Hash: scene.CameraTrack.ContentHash()})
	graph.addNode(DependencyNode{ID: recipeNode, Kind: "recipe", Hash: scene.Recipe.ContentHash()})

	for _, medium := range scene.Media {
		densityNode := "density:" + medium.ID + ":v" + fmt.Sprint(medium.Version)
		absorptionNode := "absorption:" + medium.ID + ":v" + fmt.Sprint(medium.Version)
		scatteringNode := "scattering:" + medium.ID + ":v" + fmt.Sprint(medium.Version)
		graph.addNode(DependencyNode{ID: densityNode, Kind: "density", MediumID: medium.ID, Hash: medium.ContentHash()})
		graph.addNode(DependencyNode{ID: absorptionNode, Kind: "absorption", MediumID: medium.ID, Hash: medium.ContentHash()})
		graph.addNode(DependencyNode{ID: scatteringNode, Kind: "scattering", MediumID: medium.ID, Hash: medium.ContentHash()})
	}

	for _, tile := range TileGrid(scene.Viewport, scene.Recipe) {
		tileID := tileNodeID(tile)
		tileCopy := tile
		graph.addNode(DependencyNode{ID: tileID, Kind: "tile", Tile: &tileCopy})
		for _, ray := range RaysForTile(*scene, tile) {
			hits, err := IntersectRayMedia(ray, scene.Media)
			if err != nil {
				continue
			}
			segments, err := BuildRaySegments(ray, hits)
			if err != nil {
				continue
			}
			for _, segment := range segments {
				segmentID := fmt.Sprintf("segment:%s:%d", ray.ID, segment.Index)
				graph.addNode(DependencyNode{ID: segmentID, Kind: "ray_segment"})
				graph.addEdge(cameraNode, segmentID, "ray_geometry")
				graph.addEdge(recipeNode, segmentID, "integration_budget")
				graph.addEdge(segmentID, tileID, "segment_to_tile")
				for _, mediumID := range segment.MediumIDs {
					medium := scene.Medium(mediumID)
					if medium == nil {
						continue
					}
					densityNode := "density:" + medium.ID + ":v" + fmt.Sprint(medium.Version)
					absorptionNode := "absorption:" + medium.ID + ":v" + fmt.Sprint(medium.Version)
					scatteringNode := "scattering:" + medium.ID + ":v" + fmt.Sprint(medium.Version)
					graph.addEdge(densityNode, segmentID, "density_field")
					graph.addEdge(absorptionNode, segmentID, "absorption_coefficient")
					graph.addEdge(scatteringNode, segmentID, "scattering_coefficient")
					for _, voxel := range intersectedVoxels(ray, medium, segment) {
						voxelID := voxelNodeID(medium.ID, voxel)
						voxelCopy := voxel
						graph.addNode(DependencyNode{ID: voxelID, Kind: "voxel", MediumID: medium.ID, Voxel: &voxelCopy})
						graph.addEdge(voxelID, segmentID, "voxel_density")
					}
				}
			}
		}
	}
	return graph
}

func intersectedVoxels(ray Ray, medium *Medium, segment RaySegment) [][3]int {
	res := medium.Density.Resolution
	out := make([][3]int, 0, 8)
	for z := 0; z < res[2]; z++ {
		for y := 0; y < res[1]; y++ {
			for x := 0; x < res[0]; x++ {
				bounds := VoxelWorldBounds(medium, x, y, z)
				near, far, ok := SlabIntersection(ray, bounds)
				if !ok {
					continue
				}
				if far < segment.Near.Fixed.Float64()-1e-9 || near > segment.Far.Fixed.Float64()+1e-9 {
					continue
				}
				out = append(out, [3]int{x, y, z})
			}
		}
	}
	return out
}

func tileNodeID(tile TileCoord) string {
	return fmt.Sprintf("tile:%d:%d", tile.X, tile.Y)
}

func voxelNodeID(mediumID string, voxel [3]int) string {
	return fmt.Sprintf("voxel:%s:%d:%d:%d", mediumID, voxel[0], voxel[1], voxel[2])
}

func (g *DependencyGraph) Descendants(nodeID string) []string {
	adjacency := map[string][]string{}
	for _, edge := range g.Edges {
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
	}
	seen := map[string]bool{}
	queue := []string{nodeID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacency[current] {
			if seen[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, next)
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (g *DependencyGraph) AffectedTiles(nodeID string) []TileCoord {
	tiles := map[TileCoord]bool{}
	if node, ok := g.Nodes[nodeID]; ok && node.Kind == "tile" && node.Tile != nil {
		tiles[*node.Tile] = true
	}
	for _, id := range g.Descendants(nodeID) {
		node := g.Nodes[id]
		if node.Kind == "tile" && node.Tile != nil {
			tiles[*node.Tile] = true
		}
	}
	out := make([]TileCoord, 0, len(tiles))
	for tile := range tiles {
		out = append(out, tile)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Y < out[j].Y || (out[i].Y == out[j].Y && out[i].X < out[j].X)
	})
	return out
}

type InvalidationItem struct {
	MediumID string    `json:"medium_id,omitempty"`
	Tile     TileCoord `json:"tile"`
	NodeID   string    `json:"node_id"`
	Reason   string    `json:"reason"`
}

type InvalidationReport struct {
	EditKind        string             `json:"edit_kind"`
	MediumID        string             `json:"medium_id"`
	Affected        []InvalidationItem `json:"affected"`
	AffectedTiles   []TileCoord        `json:"affected_tiles"`
	ReusedTiles     []TileCoord        `json:"reused_tiles"`
	InvalidatedAtMS int64              `json:"invalidated_at_ms,omitempty"`
}

func (r InvalidationReport) ContainsTile(tile TileCoord) bool {
	for _, candidate := range r.AffectedTiles {
		if candidate == tile {
			return true
		}
	}
	return false
}

func PlanVoxelEdit(scene *Scene, graph *DependencyGraph, mediumID string, voxel [3]int, value int) (*InvalidationReport, error) {
	medium := scene.Medium(mediumID)
	if medium == nil {
		e := schemaError("medium_id", mediumID, "medium not found")
		return nil, e
	}
	d := medium.Density
	if voxel[0] < 0 || voxel[0] >= d.Resolution[0] || voxel[1] < 0 || voxel[1] >= d.Resolution[1] || voxel[2] < 0 || voxel[2] >= d.Resolution[2] {
		e := newError(CodeDensity, "voxel coordinate %v is outside resolution %v", voxel, d.Resolution)
		e.Field = "voxel"
		e.Value = voxel
		e.MediumID = mediumID
		return nil, e
	}
	if value < 0 || value > MaxDensityVoxel {
		e := newError(CodeDensity, "density value %d is outside [0,%d]", value, MaxDensityVoxel)
		e.Field = "value"
		e.Value = value
		e.MediumID = mediumID
		return nil, e
	}
	affected := graph.AffectedTiles(voxelNodeID(mediumID, voxel))
	return newInvalidationReport("density_voxel", mediumID, "voxel_density", voxelNodeID(mediumID, voxel), affected, scene), nil
}

func ApplyVoxelEdit(scene *Scene, graph *DependencyGraph, mediumID string, voxel [3]int, value int) (*InvalidationReport, error) {
	report, err := PlanVoxelEdit(scene, graph, mediumID, voxel, value)
	if err != nil {
		return nil, err
	}
	medium := scene.Medium(mediumID)
	medium.Density.Values[medium.Density.Index(voxel[0], voxel[1], voxel[2])] = value
	medium.Version++
	return report, nil
}

func ApplyScatteringEdit(scene *Scene, graph *DependencyGraph, mediumID string, value Fixed) (*InvalidationReport, error) {
	medium := scene.Medium(mediumID)
	if medium == nil {
		return nil, schemaError("medium_id", mediumID, "medium not found")
	}
	if value < 0 || value > FixedFromFloat(MaxCoefficient) {
		e := schemaError("scattering", value.Float64(), "scattering must be in [0,%v]", MaxCoefficient)
		e.MediumID = mediumID
		return nil, e
	}
	oldVersion := medium.Version
	nodeID := "scattering:" + mediumID + ":v" + fmt.Sprint(oldVersion)
	affected := graph.AffectedTiles(nodeID)
	medium.Scattering = value
	medium.Version++
	return newInvalidationReport("scattering", mediumID, "scattering_coefficient", nodeID, affected, scene), nil
}

func newInvalidationReport(kind, mediumID, reason, nodeID string, affectedTiles []TileCoord, scene *Scene) *InvalidationReport {
	all := TileGrid(scene.Viewport, scene.Recipe)
	affectedSet := map[TileCoord]bool{}
	items := make([]InvalidationItem, 0, len(affectedTiles))
	for _, tile := range affectedTiles {
		affectedSet[tile] = true
		items = append(items, InvalidationItem{MediumID: mediumID, Tile: tile, NodeID: nodeID, Reason: reason})
	}
	reused := make([]TileCoord, 0)
	for _, tile := range all {
		if !affectedSet[tile] {
			reused = append(reused, tile)
		}
	}
	return &InvalidationReport{EditKind: kind, MediumID: mediumID, Affected: items, AffectedTiles: affectedTiles, ReusedTiles: reused}
}

func RayCacheKey(scene *Scene, ray Ray, options IntegrationOptions, segments []RaySegment) string {
	payload, _ := json.Marshal(struct {
		RayID         string             `json:"ray_id"`
		Pixel         PixelCoord         `json:"pixel"`
		CameraVersion int64              `json:"camera_version"`
		RecipeVersion int64              `json:"recipe_version"`
		Media         []map[string]any   `json:"media"`
		UpstreamKeys  []string           `json:"upstream_keys"`
		Options       IntegrationOptions `json:"options"`
		AlgoVersion   string             `json:"algo_version"`
	}{
		RayID: ray.ID, Pixel: ray.Pixel, CameraVersion: scene.CameraTrack.Version,
		RecipeVersion: scene.Recipe.Version, Media: dependencyMedia(scene, segments),
		UpstreamKeys: []string{scene.CameraTrack.ContentHash(), scene.Recipe.ContentHash()},
		Options:      options, AlgoVersion: AlgoVersion,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func dependencyMedia(scene *Scene, segments []RaySegment) []map[string]any {
	ids := map[string]bool{}
	for _, segment := range segments {
		for _, id := range segment.MediumIDs {
			ids[id] = true
		}
	}
	out := make([]map[string]any, 0, len(ids))
	for id := range ids {
		medium := scene.Medium(id)
		if medium == nil {
			continue
		}
		out = append(out, map[string]any{"id": id, "version": medium.Version, "hash": medium.ContentHash()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return out
}

func TileCacheKey(scene *Scene, tile TileCoord, rayResults []RayResult, options IntegrationOptions) string {
	keys := make([]string, 0, len(rayResults))
	for _, result := range rayResults {
		keys = append(keys, result.CacheKey)
	}
	sort.Strings(keys)
	payload, _ := json.Marshal(struct {
		Tile          TileCoord          `json:"tile"`
		Options       IntegrationOptions `json:"options"`
		UpstreamKeys  []string           `json:"upstream_ray_cache_keys"`
		RecipeVersion int64              `json:"recipe_version"`
		AlgoVersion   string             `json:"algo_version"`
	}{tile, options, keys, scene.Recipe.Version, AlgoVersion})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
