package maskqc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type CacheKeyParts struct {
	TileID            string   `json:"tile_id"`
	TileContentHash   string   `json:"tile_content_hash"`
	StrokeID          string   `json:"stroke_id,omitempty"`
	StrokeContentHash string   `json:"stroke_content_hash,omitempty"`
	SynthesisOrder    int      `json:"synthesis_order"`
	OrderVersion      string   `json:"order_version"`
	ConversionVersion string   `json:"conversion_version"`
	AlgorithmVersion  string   `json:"algorithm_version"`
	UpstreamKeys      []string `json:"upstream_keys,omitempty"`
}

func BuildCacheKey(parts CacheKeyParts) string {
	if parts.OrderVersion == "" {
		parts.OrderVersion = OrderRulesVersion
	}
	if parts.ConversionVersion == "" {
		parts.ConversionVersion = ConversionParamsVersion
	}
	if parts.AlgorithmVersion == "" {
		parts.AlgorithmVersion = AlgorithmVersion
	}
	sort.Strings(parts.UpstreamKeys)
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type GraphNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

type GraphEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type DependencyGraph struct {
	IconID string      `json:"icon_id"`
	Nodes  []GraphNode `json:"nodes"`
	Edges  []GraphEdge `json:"edges"`
}

func BuildDependencyGraph(icon *Icon, order []*Stroke) *DependencyGraph {
	g := &DependencyGraph{IconID: icon.ID}
	orderNode := "order:" + icon.ID
	topoNode := "topology:" + icon.ID
	dirNode := "direction:" + icon.ID
	g.Nodes = append(g.Nodes,
		GraphNode{orderNode, "order", "synthesis order / priority+seq"},
		GraphNode{topoNode, "topology", "connected components and holes"},
		GraphNode{dirNode, "direction", "boundary-normal direction"},
	)
	for _, id := range icon.TileIDs() {
		g.Nodes = append(g.Nodes, GraphNode{id, "tile", id})
		g.Edges = append(g.Edges, GraphEdge{orderNode, id, "order_version+dictionary_position"})
		g.Edges = append(g.Edges, GraphEdge{id, topoNode, "tile_pixels"})
		g.Edges = append(g.Edges, GraphEdge{id, dirNode, "boundary_pixels"})
	}
	g.Edges = append(g.Edges, GraphEdge{topoNode, dirNode, "component_labels"})
	for _, s := range order {
		g.Nodes = append(g.Nodes, GraphNode{s.ID, "stroke", s.ID})
		g.Edges = append(g.Edges, GraphEdge{orderNode, s.ID, "adjudicates_priority_then_seq"})
		for _, tileID := range AffectedTileIDs(icon, s) {
			g.Edges = append(g.Edges, GraphEdge{s.ID, tileID, "raster_bounds_intersect_tile"})
		}
	}
	return g
}

type EditType string

const (
	EditUpdate EditType = "update"
	EditInsert EditType = "insert"
)

type Edit struct {
	Type      EditType `json:"type"`
	StrokeID  string   `json:"stroke_id"`
	NewStart  *Point   `json:"new_start,omitempty"`
	NewEnd    *Point   `json:"new_end,omitempty"`
	NewWeight *int     `json:"new_weight,omitempty"`
	NewSeq    *int     `json:"new_seq,omitempty"`
	Insert    *Stroke  `json:"insert,omitempty"`
}

type InvalidationReport struct {
	IconID         string            `json:"icon_id"`
	EditType       EditType          `json:"edit_type"`
	StrokeID       string            `json:"stroke_id"`
	Reasons        []string          `json:"reasons"`
	OldContentHash string            `json:"old_content_hash,omitempty"`
	NewContentHash string            `json:"new_content_hash,omitempty"`
	AffectedTiles  []string          `json:"affected_tiles"`
	ReusableTiles  []string          `json:"reusable_tiles"`
	TileReasons    map[string]string `json:"tile_reasons"`
	InvalidNodes   []string          `json:"invalid_nodes"`
	Downstream     []string          `json:"downstream"`
	ImpactScope    string            `json:"impact_scope"`
}

func ApplyEdit(p *Project, iconID string, edit Edit) (*Stroke, *Stroke, error) {
	icon := p.Icon(iconID)
	if icon == nil {
		return nil, nil, newError(ESchema, "icon_id", "icon %q not found", iconID)
	}
	switch edit.Type {
	case EditUpdate:
		var current *Stroke
		for _, s := range icon.Strokes {
			if s.ID == edit.StrokeID {
				current = s
				break
			}
		}
		if current == nil {
			return nil, nil, newError(ESchema, "stroke_id", "stroke %q not found", edit.StrokeID)
		}
		old := *current
		updated := *current
		if edit.NewStart != nil {
			updated.Start = *edit.NewStart
		}
		if edit.NewEnd != nil {
			updated.End = *edit.NewEnd
		}
		if edit.NewWeight != nil {
			updated.WeightPermyriad = *edit.NewWeight
		}
		if edit.NewSeq != nil {
			updated.Seq = *edit.NewSeq
		}
		if err := ValidateStrokeParameters(&updated); err != nil {
			return &old, current, err
		}
		*current = updated
		return &old, current, nil
	case EditInsert:
		if edit.Insert == nil {
			return nil, nil, newError(ESchema, "insert", "missing stroke body")
		}
		for _, s := range icon.Strokes {
			if s.ID == edit.Insert.ID {
				return nil, nil, newError(ESchema, "insert.id", "stroke %q already exists", edit.Insert.ID)
			}
		}
		if err := ValidateStrokeParameters(edit.Insert); err != nil {
			return nil, edit.Insert, err
		}
		edit.Insert.SourceSchema = p.Schema
		icon.Strokes = append(icon.Strokes, edit.Insert)
		return nil, edit.Insert, nil
	default:
		return nil, nil, newError(ESchema, "edit.type", "unknown edit type %q", edit.Type)
	}
}

func AffectedTileIDs(icon *Icon, s *Stroke) []string {
	if s == nil {
		return nil
	}
	bounds := StrokeBounds(*s, icon.Width, icon.Height)
	var out []string
	for ty := 0; ty*icon.TileHeight < icon.Height; ty++ {
		for tx := 0; tx*icon.TileWidth < icon.Width; tx++ {
			id := TileID(icon.ID, tx, ty)
			if bounds.Intersect(TileRect(icon, tx, ty)).Empty() {
				continue
			}
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// PlanInvalidation computes exact geometric tile impact. For coordinate edits
// both old and new bounds are included; weight-only edits stay on the same tiles.
func PlanInvalidation(icon *Icon, edit Edit, oldStroke, newStroke *Stroke) *InvalidationReport {
	rep := &InvalidationReport{
		IconID: icon.ID, EditType: edit.Type, StrokeID: edit.StrokeID,
		TileReasons: map[string]string{},
		Downstream:  []string{"topology:" + icon.ID, "direction:" + icon.ID},
	}
	affected := map[string]string{}
	if oldStroke != nil {
		rep.OldContentHash = oldStroke.ContentHash()
		for _, id := range AffectedTileIDs(icon, oldStroke) {
			affected[id] = "old_raster_bounds"
		}
	}
	if newStroke != nil {
		rep.NewContentHash = newStroke.ContentHash()
		reason := "new_raster_bounds"
		if edit.Type == EditUpdate && edit.NewStart == nil && edit.NewEnd == nil && edit.NewSeq == nil {
			reason = "stroke_parameter_changed"
		}
		for _, id := range AffectedTileIDs(icon, newStroke) {
			if existing, ok := affected[id]; ok {
				affected[id] = existing + "+new_bounds"
			} else {
				affected[id] = reason
			}
		}
	}
	if edit.NewWeight != nil {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("weight changed to %d/10000", *edit.NewWeight))
	}
	if edit.NewStart != nil || edit.NewEnd != nil {
		rep.Reasons = append(rep.Reasons, "stroke coordinates changed; old and new bounds invalidated")
	}
	if edit.Type == EditInsert {
		rep.Reasons = append(rep.Reasons, "new stroke inserted; intersecting tiles invalidated")
	}
	if edit.NewSeq != nil {
		rep.Reasons = append(rep.Reasons, "pen sequence changed; synthesis order must be adjudicated again")
		rep.InvalidNodes = append(rep.InvalidNodes, "order:"+icon.ID)
	}

	all := icon.TileIDs()
	for _, id := range all {
		if reason, ok := affected[id]; ok {
			rep.AffectedTiles = append(rep.AffectedTiles, id)
			rep.TileReasons[id] = reason
		} else {
			rep.ReusableTiles = append(rep.ReusableTiles, id)
		}
	}
	sort.Strings(rep.AffectedTiles)
	sort.Strings(rep.ReusableTiles)
	rep.InvalidNodes = append(rep.InvalidNodes, rep.AffectedTiles...)
	rep.InvalidNodes = append(rep.InvalidNodes, rep.Downstream...)
	if len(rep.AffectedTiles) == 0 {
		rep.ImpactScope = "none"
	} else if len(rep.ReusableTiles) == 0 {
		rep.ImpactScope = fmt.Sprintf("all %d tiles + topology/direction", len(rep.AffectedTiles))
	} else {
		rep.ImpactScope = fmt.Sprintf("%d/%d tiles + topology/direction; %d tiles reusable", len(rep.AffectedTiles), len(all), len(rep.ReusableTiles))
	}
	return rep
}
