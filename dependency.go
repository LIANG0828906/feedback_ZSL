package iconmask

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
)

type CacheKeyInput struct {
	BlockContentHash string `json:"blockContentHash"`
	SegmentID        string `json:"segmentId"`
	Stage            string `json:"stage,omitempty"`
	Radius           int    `json:"radius"`
	Round            int    `json:"round"`
	UpstreamKey      string `json:"upstreamKey"`
}

func BuildCacheKey(input CacheKeyInput) string {
	h := sha256.New()
	_, _ = h.Write([]byte("iconmask/cache/v1\x00"))
	_, _ = h.Write([]byte(input.BlockContentHash))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(input.SegmentID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(input.Stage))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.Itoa(input.Radius)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.Itoa(input.Round)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(input.UpstreamKey))
	return hex.EncodeToString(h.Sum(nil))
}

type BlockDependency struct {
	Block       Coord    `json:"block"`
	Rect        Rect     `json:"rect"`
	ContentHash string   `json:"contentHash"`
	SegmentIDs  []string `json:"segmentIds"`
}

type DependencyEdge struct {
	From           string  `json:"from"`
	To             string  `json:"to"`
	Kind           string  `json:"kind"`
	SegmentID      string  `json:"segmentId,omitempty"`
	Curvature      float64 `json:"curvature,omitempty"`
	Radius         int     `json:"radius,omitempty"`
	RadiusBandMin  int     `json:"radiusBandMin,omitempty"`
	RadiusBandMax  int     `json:"radiusBandMax,omitempty"`
	CurvatureBands int     `json:"curvatureBands,omitempty"`
	Round          int     `json:"round"`
	CacheKey       string  `json:"cacheKey,omitempty"`
}

type DependencyGraph struct {
	ImageID    string            `json:"imageId"`
	BlockSize  int               `json:"blockSize"`
	Segments   []BoundarySegment `json:"segments"`
	Blocks     []BlockDependency `json:"blocks"`
	Edges      []DependencyEdge  `json:"edges"`
	invalid    map[string]bool
	blockByKey map[string]BlockDependency
}

type Invalidation struct {
	Block     Coord          `json:"block"`
	SegmentID string         `json:"segmentId"`
	Reason    string         `json:"reason"`
	CacheKey  string         `json:"cacheKey,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

func NewDependencyGraph(
	imageID string,
	mask Mask,
	segments []BoundarySegment,
	blockSize int,
	radiusMax int,
	maxRounds int,
	radiusTable *RadiusTable,
) *DependencyGraph {
	if blockSize <= 0 {
		blockSize = 32
	}
	graph := &DependencyGraph{
		ImageID:    imageID,
		BlockSize:  blockSize,
		Segments:   append([]BoundarySegment(nil), segments...),
		invalid:    make(map[string]bool),
		blockByKey: make(map[string]BlockDependency),
	}
	rounds := maxRounds
	if rounds <= 0 {
		rounds = 1
	}
	segmentByID := make(map[string]BoundarySegment, len(segments))
	for _, segment := range segments {
		segmentByID[segment.ID] = segment
	}
	for y := 0; y < mask.Height; y += blockSize {
		for x := 0; x < mask.Width; x += blockSize {
			block := Coord{X: x / blockSize, Y: y / blockSize}
			rect := Rect{
				X: x, Y: y,
				Width:  minInt(blockSize, mask.Width-x),
				Height: minInt(blockSize, mask.Height-y),
			}
			expanded := Rect{
				X:      maxInt(0, rect.X-radiusMax-2),
				Y:      maxInt(0, rect.Y-radiusMax-2),
				Width:  minInt(mask.Width, rect.X+rect.Width+radiusMax+2) - maxInt(0, rect.X-radiusMax-2),
				Height: minInt(mask.Height, rect.Y+rect.Height+radiusMax+2) - maxInt(0, rect.Y-radiusMax-2),
			}
			dependency := BlockDependency{
				Block:       block,
				Rect:        rect,
				ContentHash: blockContentHash(mask, expanded),
			}
			for _, segment := range segments {
				for _, point := range segment.Points {
					if point.X >= float64(expanded.X)-1 && point.X <= float64(expanded.X+expanded.Width)+1 &&
						point.Y >= float64(expanded.Y)-1 && point.Y <= float64(expanded.Y+expanded.Height)+1 {
						dependency.SegmentIDs = append(dependency.SegmentIDs, segment.ID)
						break
					}
				}
			}
			sort.Strings(dependency.SegmentIDs)
			graph.Blocks = append(graph.Blocks, dependency)
			graph.blockByKey[blockKey(block)] = dependency
			upstream := dependency.ContentHash
			for _, segmentID := range dependency.SegmentIDs {
				segment := segmentByID[segmentID]
				radius := 0
				if radiusTable != nil {
					if entry, ok := radiusTable.Lookup(segmentID); ok {
						radius = entry.ErosionRadius
						upstream = BuildCacheKey(CacheKeyInput{
							BlockContentHash: dependency.ContentHash,
							SegmentID:        segmentID,
							Radius:           radius,
							Round:            0,
							UpstreamKey:      upstream,
						})
					}
				}
				segmentNode := "segment:" + segmentID
				bandMin, bandMax, bands := 0, 0, 0
				if radiusTable != nil {
					bandMin = radiusTable.Config.Min
					bandMax = radiusTable.Config.Max
					bands = radiusTable.Config.CurvatureBands
				}
				graph.Edges = append(graph.Edges,
					DependencyEdge{
						From: "param:radiusBand", To: segmentNode + ":radius",
						Kind: "radius_band_to_segment_radius", SegmentID: segmentID,
						Curvature: segment.Curvature, RadiusBandMin: bandMin,
						RadiusBandMax: bandMax, CurvatureBands: bands,
					},
					DependencyEdge{
						From: "param:curvatureBands", To: segmentNode + ":radius",
						Kind: "curvature_bands_to_segment_radius", SegmentID: segmentID,
						Curvature: segment.Curvature, RadiusBandMin: bandMin,
						RadiusBandMax: bandMax, CurvatureBands: bands,
					},
					DependencyEdge{
						From:      segmentNode + ":curvature",
						To:        segmentNode + ":radius",
						Kind:      "curvature_to_radius",
						SegmentID: segmentID, Curvature: segment.Curvature,
					},
					DependencyEdge{
						From:      segmentNode + ":radius",
						To:        "block:" + blockKey(block) + ":mask",
						Kind:      "radius_to_block_mask",
						SegmentID: segmentID, Curvature: segment.Curvature,
						Radius: radius, CacheKey: upstream,
					},
				)
				for round := 0; round < rounds; round++ {
					graph.Edges = append(graph.Edges,
						DependencyEdge{
							From:      "round:" + strconv.Itoa(round),
							To:        segmentNode + ":operation",
							Kind:      "round_to_operation",
							SegmentID: segmentID, Radius: radius, Round: round,
						},
						DependencyEdge{
							From:      segmentNode + ":operation",
							To:        "block:" + blockKey(block) + ":mask",
							Kind:      "operation_to_block_mask",
							SegmentID: segmentID, Radius: radius, Round: round,
							CacheKey: BuildCacheKey(CacheKeyInput{
								BlockContentHash: dependency.ContentHash,
								SegmentID:        segmentID,
								Radius:           radius,
								Round:            round,
								UpstreamKey:      upstream,
							}),
						},
					)
				}
			}
		}
	}
	sort.SliceStable(graph.Blocks, func(i, j int) bool {
		if graph.Blocks[i].Block.Y != graph.Blocks[j].Block.Y {
			return graph.Blocks[i].Block.Y < graph.Blocks[j].Block.Y
		}
		return graph.Blocks[i].Block.X < graph.Blocks[j].Block.X
	})
	return graph
}

func blockContentHash(mask Mask, rect Rect) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d:%d:%d:%d:", rect.X, rect.Y, rect.Width, rect.Height)
	for y := rect.Y; y < rect.Y+rect.Height; y++ {
		for x := rect.X; x < rect.X+rect.Width; x++ {
			h.Write([]byte{mask.At(x, y)})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func blockKey(block Coord) string {
	return strconv.Itoa(block.X) + "," + strconv.Itoa(block.Y)
}

func (graph *DependencyGraph) InvalidateParams(imageID string, oldCfg, newCfg RadiusConfig) ([]Invalidation, *Error) {
	oldTable, err := ResolveRadii(imageID, segmentsFromGraph(graph), oldCfg)
	if err != nil {
		return nil, err
	}
	newTable, err := ResolveRadii(imageID, segmentsFromGraph(graph), newCfg)
	if err != nil {
		return nil, err
	}
	affected := make(map[string]string)
	for _, oldEntry := range oldTable.Segments {
		newEntry, ok := newTable.Lookup(oldEntry.SegmentID)
		if !ok || oldEntry.ErosionRadius != newEntry.ErosionRadius || oldEntry.DilationRadius != newEntry.DilationRadius ||
			oldEntry.Bucket != newEntry.Bucket {
			affected[oldEntry.SegmentID] = "radius_mapping_changed"
		}
	}
	invalidations := make([]Invalidation, 0)
	for _, dependency := range graph.Blocks {
		for _, segmentID := range dependency.SegmentIDs {
			reason, ok := affected[segmentID]
			if !ok {
				continue
			}
			entry, _ := newTable.Lookup(segmentID)
			invalidations = append(invalidations, Invalidation{
				Block:     dependency.Block,
				SegmentID: segmentID,
				Reason:    reason,
				CacheKey: BuildCacheKey(CacheKeyInput{
					BlockContentHash: dependency.ContentHash,
					SegmentID:        segmentID,
					Radius:           entry.ErosionRadius,
					Round:            0,
					UpstreamKey:      dependency.ContentHash,
				}),
				Details: map[string]any{
					"oldRadius": radiusForSegment(oldTable, segmentID),
					"newRadius": entry.ErosionRadius,
				},
			})
			graph.invalid[blockKey(dependency.Block)] = true
		}
	}
	sortInvalidations(invalidations)
	return invalidations, nil
}

func segmentsFromGraph(graph *DependencyGraph) []BoundarySegment {
	if len(graph.Segments) > 0 {
		return graph.Segments
	}
	seen := make(map[string]bool)
	segments := make([]BoundarySegment, 0)
	for _, edge := range graph.Edges {
		if edge.Kind != "curvature_to_radius" || seen[edge.SegmentID] {
			continue
		}
		seen[edge.SegmentID] = true
		segments = append(segments, BoundarySegment{
			ID:        edge.SegmentID,
			Curvature: edge.Curvature,
		})
	}
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].ID < segments[j].ID })
	return segments
}

func radiusForSegment(table *RadiusTable, segmentID string) int {
	if entry, ok := table.Lookup(segmentID); ok {
		return entry.ErosionRadius
	}
	return 0
}

func (graph *DependencyGraph) InvalidateInitialMask(imageID string, oldMask, newMask Mask, radiusMax int) ([]Invalidation, *Error) {
	if oldMask.Width != newMask.Width || oldMask.Height != newMask.Height {
		return nil, schemaError(imageID, "initialMask", "edited initial mask dimensions conflict")
	}
	changedBlocks := make(map[string]Coord)
	affectedSegments := make(map[string]bool)
	changedPixels := 0
	for i := range oldMask.Data {
		if oldMask.Data[i] == newMask.Data[i] {
			continue
		}
		changedPixels++
		x, y := i%oldMask.Width, i/oldMask.Width
		block := Coord{X: x / graph.BlockSize, Y: y / graph.BlockSize}
		changedBlocks[blockKey(block)] = block
		px, py := float64(x)+0.5, float64(y)+0.5
		bestDistance := math.Inf(1)
		bestSegment := ""
		for _, segment := range graph.Segments {
			for _, point := range segment.Points {
				d := math.Hypot(px-point.X, py-point.Y)
				if d < bestDistance {
					bestDistance = d
					bestSegment = segment.ID
				}
			}
		}
		if bestSegment != "" && bestDistance <= float64(radiusMax+3) {
			affectedSegments[bestSegment] = true
		}
	}
	if len(changedBlocks) == 0 {
		return nil, nil
	}
	invalidations := make([]Invalidation, 0)
	for _, dependency := range graph.Blocks {
		_, changed := changedBlocks[blockKey(dependency.Block)]
		emitted := 0
		for _, segmentID := range dependency.SegmentIDs {
			if !affectedSegments[segmentID] {
				continue
			}
			reason := "downstream_of_initial_mask_edit"
			if changed {
				reason = "initial_mask_edited"
			}
			invalidations = append(invalidations, Invalidation{
				Block:     dependency.Block,
				SegmentID: segmentID,
				Reason:    reason,
				Details:   map[string]any{"changedPixels": changedPixels},
			})
			graph.invalid[blockKey(dependency.Block)] = true
			emitted++
		}
		if changed && emitted == 0 {
			segmentID := ""
			if len(dependency.SegmentIDs) > 0 {
				segmentID = dependency.SegmentIDs[0]
			}
			invalidations = append(invalidations, Invalidation{
				Block:     dependency.Block,
				SegmentID: segmentID,
				Reason:    "initial_mask_edited",
				Details:   map[string]any{"changedPixels": changedPixels},
			})
			graph.invalid[blockKey(dependency.Block)] = true
		}
	}
	sortInvalidations(invalidations)
	return invalidations, nil
}

func sortInvalidations(invalidations []Invalidation) {
	sort.SliceStable(invalidations, func(i, j int) bool {
		if invalidations[i].Block.Y != invalidations[j].Block.Y {
			return invalidations[i].Block.Y < invalidations[j].Block.Y
		}
		if invalidations[i].Block.X != invalidations[j].Block.X {
			return invalidations[i].Block.X < invalidations[j].Block.X
		}
		if invalidations[i].SegmentID != invalidations[j].SegmentID {
			return invalidations[i].SegmentID < invalidations[j].SegmentID
		}
		return invalidations[i].Reason < invalidations[j].Reason
	})
}

func (graph *DependencyGraph) IsValid(block Coord) bool {
	return !graph.invalid[blockKey(block)]
}

func (graph *DependencyGraph) MarkValid(block Coord) {
	delete(graph.invalid, blockKey(block))
}

func (graph *DependencyGraph) InvalidBlocks() []Coord {
	blocks := make([]Coord, 0, len(graph.invalid))
	for _, dependency := range graph.Blocks {
		if graph.invalid[blockKey(dependency.Block)] {
			blocks = append(blocks, dependency.Block)
		}
	}
	return blocks
}

func (graph *DependencyGraph) DependentsOf(segmentID string) []Coord {
	blocks := make([]Coord, 0)
	for _, dependency := range graph.Blocks {
		for _, id := range dependency.SegmentIDs {
			if id == segmentID {
				blocks = append(blocks, dependency.Block)
				break
			}
		}
	}
	return blocks
}

// ReusableCheckpoints returns persisted stage products whose blocks remain
// valid after invalidation. Invalid blocks must be recomputed.
func (graph *DependencyGraph) ReusableCheckpoints(checkpoints []Checkpoint) (reusable []Checkpoint, invalid []Coord) {
	seenInvalid := make(map[string]bool)
	for _, checkpoint := range checkpoints {
		if graph.IsValid(checkpoint.Block) {
			reusable = append(reusable, checkpoint)
			continue
		}
		key := blockKey(checkpoint.Block)
		if !seenInvalid[key] {
			seenInvalid[key] = true
			invalid = append(invalid, checkpoint.Block)
		}
	}
	sort.SliceStable(invalid, func(i, j int) bool {
		if invalid[i].Y != invalid[j].Y {
			return invalid[i].Y < invalid[j].Y
		}
		return invalid[i].X < invalid[j].X
	})
	return reusable, invalid
}
