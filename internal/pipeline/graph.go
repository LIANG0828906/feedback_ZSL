package pipeline

import (
	"fmt"
	"sort"

	"glyphbrush/internal/loader"
)

type SegmentDependency struct {
	SegmentID    int      `json:"segmentId"`
	GlyphID      string   `json:"glyphId"`
	ContourIndex int      `json:"contourIndex"`
	CurveIndex   int      `json:"curveIndex"`
	ArcStart     float64  `json:"arcStart"`
	ArcEnd       float64  `json:"arcEnd"`
	ParamDeps    []string `json:"paramDeps"`
	ControlDeps  []string `json:"controlDeps"`
	LayerID      string   `json:"layerId"`
}

type LayerDependency struct {
	ID         string   `json:"id"`
	SegmentIDs []int    `json:"segmentIds"`
	Upstream   []string `json:"upstream"`
}

type DependencyGraph struct {
	GlyphID  string              `json:"glyphId"`
	Segments []SegmentDependency `json:"segments"`
	Layers   []LayerDependency   `json:"layers"`
}

type InvalidationEntry struct {
	SegmentID    int     `json:"segmentId"`
	ContourIndex int     `json:"contourIndex"`
	CurveIndex   int     `json:"curveIndex"`
	ArcStart     float64 `json:"arcStart"`
	ArcEnd       float64 `json:"arcEnd"`
	Param        string  `json:"param"`
	Reason       string  `json:"reason"`
}

type InvalidationReport struct {
	GlyphID   string              `json:"glyphId"`
	Params    []string            `json:"params,omitempty"`
	Entries   []InvalidationEntry `json:"entries"`
	Layers    []string            `json:"layers"`
	TopoOrder []string            `json:"topoOrder"`
	Empty     bool                `json:"empty"`
}

const (
	paramWidth           = "width"
	paramSpacing         = "spacing"
	paramJoin            = "join"
	paramTip             = "tip"
	paramCurvatureFactor = "curvatureFactor"
)

func buildGraph(g loader.Glyph, plans []SegmentPlan) *DependencyGraph {
	graph := &DependencyGraph{GlyphID: g.Glyph.ID}
	byContour := make(map[int][]int)
	for _, plan := range plans {
		deps := &SegmentDependency{
			SegmentID:    plan.SegmentID,
			GlyphID:      g.Glyph.ID,
			ContourIndex: plan.ContourIndex,
			CurveIndex:   plan.CurveIndex,
			ArcStart:     plan.ArcStart,
			ArcEnd:       plan.ArcEnd,
			ParamDeps:    []string{paramWidth, paramSpacing, paramJoin, paramTip, paramCurvatureFactor},
			LayerID:      contourLayerID(g.Glyph.ID, plan.ContourIndex),
		}
		// A contour uses a cumulative arc-length parameter. Editing a control
		// point changes the length of its own curve and shifts all later arc
		// parameters in that contour, so those later segments are genuine
		// downstream dependants. Earlier segments and other contours are not.
		for curve := 0; curve <= plan.CurveIndex; curve++ {
			deps.ControlDeps = append(deps.ControlDeps, controlDependency(plan.ContourIndex, curve))
		}
		graph.Segments = append(graph.Segments, *deps)
		byContour[plan.ContourIndex] = append(byContour[plan.ContourIndex], plan.SegmentID)
	}
	contourIDs := make([]int, 0, len(byContour))
	for contour := range byContour {
		contourIDs = append(contourIDs, contour)
	}
	sort.Ints(contourIDs)
	var allSegments []int
	for _, contour := range contourIDs {
		ids := append([]int(nil), byContour[contour]...)
		sort.Ints(ids)
		graph.Layers = append(graph.Layers, LayerDependency{
			ID:         contourLayerID(g.Glyph.ID, contour),
			SegmentIDs: ids,
		})
		allSegments = append(allSegments, ids...)
	}
	graph.Layers = append(graph.Layers, LayerDependency{
		ID:         compositeLayerID(g.Glyph.ID),
		SegmentIDs: allSegments,
	})
	return graph
}

func contourLayerID(glyphID string, contour int) string {
	return fmt.Sprintf("layer:%s:contour:%d", glyphID, contour)
}

func compositeLayerID(glyphID string) string {
	return fmt.Sprintf("layer:%s:composite", glyphID)
}

func controlDependency(contour, curve int) string {
	return fmt.Sprintf("control:%d:%d", contour, curve)
}

func (g *DependencyGraph) InvalidateParams(params []string) InvalidationReport {
	report := InvalidationReport{GlyphID: g.GlyphID, Params: append([]string(nil), params...)}
	paramSet := make(map[string]bool, len(params))
	for _, p := range params {
		paramSet[p] = true
	}
	invalid := make(map[int]bool)
	for _, dep := range g.Segments {
		for _, text := range dep.ParamDeps {
			if paramSet[text] {
				report.Entries = append(report.Entries, InvalidationEntry{
					SegmentID:    dep.SegmentID,
					ContourIndex: dep.ContourIndex,
					CurveIndex:   dep.CurveIndex,
					ArcStart:     dep.ArcStart,
					ArcEnd:       dep.ArcEnd,
					Param:        text,
					Reason:       fmt.Sprintf("parameter %s changed", text),
				})
				invalid[dep.SegmentID] = true
			}
		}
	}
	g.finishReport(&report, invalid)
	return report
}

func (g *DependencyGraph) InvalidateControl(contour, curve int) InvalidationReport {
	control := controlDependency(contour, curve)
	report := InvalidationReport{GlyphID: g.GlyphID, Params: []string{control}}
	invalid := make(map[int]bool)
	for _, dep := range g.Segments {
		for _, c := range dep.ControlDeps {
			if c == control {
				report.Entries = append(report.Entries, InvalidationEntry{
					SegmentID:    dep.SegmentID,
					ContourIndex: dep.ContourIndex,
					CurveIndex:   dep.CurveIndex,
					ArcStart:     dep.ArcStart,
					ArcEnd:       dep.ArcEnd,
					Param:        control,
					Reason:       "control point edited; curve and later arc-length parameters depend on it",
				})
				invalid[dep.SegmentID] = true
				break
			}
		}
	}
	g.finishReport(&report, invalid)
	return report
}

func (g *DependencyGraph) finishReport(report *InvalidationReport, invalid map[int]bool) {
	report.Empty = len(invalid) == 0
	ids := make([]int, 0, len(invalid))
	for id := range invalid {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	layerSet := make(map[string]bool)
	for _, id := range ids {
		report.TopoOrder = append(report.TopoOrder, fmt.Sprintf("segment:%d", id))
		for _, layer := range g.Layers {
			for _, sid := range layer.SegmentIDs {
				if sid == id {
					layerSet[layer.ID] = true
				}
			}
		}
	}
	for _, layer := range g.Layers {
		if layerSet[layer.ID] {
			report.Layers = append(report.Layers, layer.ID)
			report.TopoOrder = append(report.TopoOrder, layer.ID)
		}
	}
}

func (g *DependencyGraph) Segment(segmentID int) (SegmentDependency, bool) {
	for _, dep := range g.Segments {
		if dep.SegmentID == segmentID {
			return dep, true
		}
	}
	return SegmentDependency{}, false
}
