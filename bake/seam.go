package bake

import (
	"fmt"
	"math"
	"sort"

	"terrainbake/geometry"
	"terrainbake/model"
)

// SeamTransfer records a cross-chunk shadow handoff used by an affected vertex.
type SeamTransfer struct {
	EdgeID      string            `json:"edge_id"`
	FromChunk   string            `json:"from_chunk"`
	ToChunk     string            `json:"to_chunk"`
	SourceChunk string            `json:"source_chunk"`
	Handoff     model.Vec2        `json:"handoff"`
	Affected    model.VertexPoint `json:"affected"`
	Shadow      bool              `json:"shadow"`
}

// SeamInterval is one shadowed run along a shared edge.
type SeamInterval struct {
	Side     string  `json:"side"`
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	Vertices int     `json:"vertices"`
}

// SeamReport inventories cross-chunk propagation and both-side intervals.
type SeamReport struct {
	Edges     []string                  `json:"edges"`
	Transfers []SeamTransfer            `json:"transfers"`
	Intervals map[string][]SeamInterval `json:"intervals"`
}

// AnalyzeSeams verifies that both chunks agree at every shared vertex and
// collects shadow handoffs for diagnostics.
func AnalyzeSeams(result *BakeResult, terrain *geometry.Terrain) (*SeamReport, error) {
	if result == nil || terrain == nil {
		return nil, model.NewError(model.CodeValue, "seam analysis requires result and terrain")
	}
	report := &SeamReport{Intervals: make(map[string][]SeamInterval)}
	for _, edge := range terrain.Edges() {
		report.Edges = append(report.Edges, edge.ID)
		times := make(map[int]struct{})
		for _, unit := range result.Units {
			if unit.ChunkID == edge.ChunkA || unit.ChunkID == edge.ChunkB {
				times[unit.Time] = struct{}{}
			}
		}
		timeList := make([]int, 0, len(times))
		for t := range times {
			timeList = append(timeList, t)
		}
		sort.Ints(timeList)
		for _, timeValue := range timeList {
			sideA, okA := result.Unit(edge.ChunkA, timeValue)
			sideB, okB := result.Unit(edge.ChunkB, timeValue)
			if !okA || !okB {
				return nil, model.NewError(model.CodeSeam, "shared edge %s is missing one side at time %d", edge.ID, timeValue).
					WithEdge(edge.ID).WithTime(timeValue).WithChunk(edge.ChunkB)
			}
			pointsA := edgePoints(sideA, edge)
			pointsB := edgePoints(sideB, edge)
			keys := make([]string, 0, len(pointsA))
			for key := range pointsA {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				a := pointsA[key]
				b, ok := pointsB[key]
				if !ok {
					return nil, model.NewError(model.CodeSeam,
						"cross-chunk shadow was not propagated at %s time %d", edge.ID, timeValue).
						WithEdge(edge.ID).WithTime(timeValue).WithChunk(edge.ChunkB).
						WithPoint(a.Vertex).WithWorld(model.Vec2{X: a.World.X, Z: a.World.Z})
				}
				if a.DirectShadow != b.DirectShadow {
					return nil, model.NewError(model.CodeSeam,
						"cross-chunk shadow mismatch at %s time %d (%t vs %t)", edge.ID, timeValue, a.DirectShadow, b.DirectShadow).
						WithEdge(edge.ID).WithTime(timeValue).WithChunk(edge.ChunkB).
						WithPoint(a.Vertex).WithWorld(model.Vec2{X: a.World.X, Z: a.World.Z}).WithField("direct_shadow")
				}
				if a.SkyVisibilityFixed != b.SkyVisibilityFixed {
					return nil, model.NewError(model.CodeSeam,
						"cross-chunk sky visibility mismatch at %s time %d (%d vs %d)",
						edge.ID, timeValue, a.SkyVisibilityFixed, b.SkyVisibilityFixed).
						WithEdge(edge.ID).WithTime(timeValue).WithChunk(edge.ChunkB).
						WithPoint(a.Vertex).WithWorld(model.Vec2{X: a.World.X, Z: a.World.Z}).
						WithField("sky_visibility").WithChannel("visibility")
				}
				for _, point := range []*model.PointResult{a, b} {
					for _, handoff := range point.Shadow.Handoffs {
						report.Transfers = append(report.Transfers, SeamTransfer{
							EdgeID: handoff.EdgeID, FromChunk: handoff.FromChunk, ToChunk: handoff.ToChunk,
							SourceChunk: handoff.SourceChunk, Handoff: handoff.Position,
							Affected: point.Vertex, Shadow: point.DirectShadow,
						})
					}
				}
			}
			intervals := append(shadowIntervals("a", pointsA), shadowIntervals("b", pointsB)...)
			report.Intervals[fmt.Sprintf("%s@%d", edge.ID, timeValue)] = intervals
		}
	}
	sort.Strings(report.Edges)
	sort.Slice(report.Transfers, func(i, j int) bool {
		a, b := report.Transfers[i], report.Transfers[j]
		if a.EdgeID != b.EdgeID {
			return a.EdgeID < b.EdgeID
		}
		if a.Affected.Z != b.Affected.Z {
			return a.Affected.Z < b.Affected.Z
		}
		return a.Affected.X < b.Affected.X
	})
	return report, nil
}

func edgePoints(unit *BakedUnit, edge model.SharedEdge) map[string]*model.PointResult {
	out := make(map[string]*model.PointResult)
	for i := range unit.Points {
		p := &unit.Points[i]
		if !onEdge(p.World, edge) {
			continue
		}
		along := p.World.Z
		if edge.Axis == "z" {
			along = p.World.X
		}
		key := fmt.Sprintf("%.6f", along)
		out[key] = p
	}
	return out
}

func onEdge(world model.Vec3, edge model.SharedEdge) bool {
	if edge.Axis == "x" {
		if math.Abs(world.X-edge.Position) > 1e-7 {
			return false
		}
		return world.Z >= edge.Start-1e-7 && world.Z <= edge.End+1e-7
	}
	if math.Abs(world.Z-edge.Position) > 1e-7 {
		return false
	}
	return world.X >= edge.Start-1e-7 && world.X <= edge.End+1e-7
}

func shadowIntervals(side string, points map[string]*model.PointResult) []SeamInterval {
	keys := make([]string, 0, len(points))
	for key := range points {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []SeamInterval
	var start, end float64
	count := 0
	flush := func() {
		if count == 0 {
			return
		}
		out = append(out, SeamInterval{Side: side, Start: start, End: end, Vertices: count})
		count = 0
	}
	for _, key := range keys {
		point := points[key]
		if !point.DirectShadow {
			flush()
			continue
		}
		value, _ := parseEdgeKey(key)
		if count == 0 {
			start = value
		}
		end = value
		count++
	}
	flush()
	return out
}

func parseEdgeKey(key string) (float64, error) {
	var value float64
	_, err := fmt.Sscanf(key, "%f", &value)
	return value, err
}
