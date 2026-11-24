package deps

import (
	"fmt"
	"sort"

	"cadlab/internal/geom"
	"cadlab/internal/model"
	"cadlab/internal/render"
)

const (
	KindControl  = "controlPoint"
	KindPatch    = "surfacePatch"
	KindMaterial = "material"
	KindDensity  = "tessDensity"
	KindTile     = "screenTile"
	KindShade    = "shadeStage"
)

type Node struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

type Edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Stage string `json:"stage"`
}

type Graph struct {
	Version int64  `json:"version"`
	Nodes   []Node `json:"nodes"`
	Edges   []Edge `json:"edges"`
}

type EditKind string

const (
	EditControl  EditKind = "controlPoint"
	EditMaterial EditKind = "material"
	EditDensity  EditKind = "density"
)

type Edit struct {
	Kind     EditKind       `json:"kind"`
	PatchID  int            `json:"patchId"`
	Row      int            `json:"row"`
	Col      int            `json:"col"`
	Position model.Vec3     `json:"position"`
	Material model.Material `json:"material"`
	Density  int            `json:"density"`
}

type TileInvalidation struct {
	TileID  int      `json:"tileId"`
	Patches []int    `json:"patchIds"`
	Reasons []string `json:"reasons"`
	Stages  []string `json:"stages"`
}

type Report struct {
	Version          int64              `json:"version"`
	Edit             Edit               `json:"edit"`
	InvalidatedTiles []int              `json:"invalidatedTiles"`
	ReusedTiles      []int              `json:"reusedTiles"`
	AffectedPatches  []int              `json:"affectedPatches"`
	DownstreamStages []string           `json:"downstreamStages"`
	Items            []TileInvalidation `json:"items"`
	Graph            Graph              `json:"graph"`
}

func Build(scene *render.Scene) Graph {
	g := Graph{Version: scene.Version}
	seen := map[string]bool{}
	addNode := func(id, kind, label string) {
		if seen[id] {
			return
		}
		seen[id] = true
		g.Nodes = append(g.Nodes, Node{ID: id, Kind: kind, Label: label})
	}
	addEdge := func(from, to, stage string) {
		g.Edges = append(g.Edges, Edge{From: from, To: to, Stage: stage})
	}
	for _, p := range scene.Patches {
		patchNode := patchID(p.PatchID)
		addNode(patchNode, KindPatch, fmt.Sprintf("Surface patch %d", p.PatchID))
		matNode := materialID(p.PatchID)
		denNode := densityID(p.PatchID)
		addNode(matNode, KindMaterial, fmt.Sprintf("Material P%d", p.PatchID))
		addNode(denNode, KindDensity, fmt.Sprintf("Tessellation P%d", p.PatchID))
		addEdge(matNode, patchNode, "material")
		addEdge(denNode, patchNode, "tessellation")
		for row := 0; row < p.Source.VCount; row++ {
			for col := 0; col < p.Source.UCount; col++ {
				cpNode := controlID(p.PatchID, row, col)
				addNode(cpNode, KindControl, fmt.Sprintf("P%d control[%d,%d]", p.PatchID, row, col))
				addEdge(cpNode, patchNode, "geometry")
			}
		}
		covered := render.PatchCoverage(scene, p.PatchID, nil)
		for _, tile := range covered {
			tileNode := tileID(tile)
			shadeNode := shadeID(tile)
			addNode(tileNode, KindTile, fmt.Sprintf("Screen tile %d", tile))
			addNode(shadeNode, KindShade, fmt.Sprintf("Shade tile %d", tile))
			addEdge(patchNode, tileNode, "geometry")
			addEdge(tileNode, shadeNode, "shading")
			addEdge(matNode, shadeNode, "material")
			addEdge(denNode, tileNode, "tessellation")
			for row := 0; row < p.Source.VCount; row++ {
				for col := 0; col < p.Source.UCount; col++ {
					addEdge(controlID(p.PatchID, row, col), tileNode, "geometry")
				}
			}
		}
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		return g.Edges[i].To < g.Edges[j].To
	})
	return g
}

func Compute(oldScene, newScene *render.Scene, edit Edit) Report {
	rep := Report{Version: newScene.Version, Edit: edit}
	tileReasons := map[int][]string{}
	tilePatches := map[int][]int{}
	tileStages := map[int][]string{}
	oldCov, newCov := map[int]bool{}, map[int]bool{}
	var rangeOld, rangeNew *[4]float64

	switch edit.Kind {
	case EditControl:
		if ps, ok := patchState(oldScene, edit.PatchID); ok {
			u0, u1, v0, v1 := geom.ControlSupport(ps.Source, edit.Row, edit.Col)
			r := [4]float64{u0, u1, v0, v1}
			rangeOld = &r
		}
		if ps, ok := patchState(newScene, edit.PatchID); ok {
			u0, u1, v0, v1 := geom.ControlSupport(ps.Source, edit.Row, edit.Col)
			r := [4]float64{u0, u1, v0, v1}
			rangeNew = &r
		}
	case EditMaterial, EditDensity:
	default:
		return rep
	}
	for _, id := range render.PatchCoverage(oldScene, edit.PatchID, rangeOld) {
		oldCov[id] = true
	}
	for _, id := range render.PatchCoverage(newScene, edit.PatchID, rangeNew) {
		newCov[id] = true
	}
	for id := range oldCov {
		tileReasons[id] = append(tileReasons[id], reasonFor(edit, "old"))
	}
	for id := range newCov {
		if !oldCov[id] {
			tileReasons[id] = append(tileReasons[id], reasonFor(edit, "new"))
		}
	}
	for id := range tileReasons {
		tilePatches[id] = []int{edit.PatchID}
		tileStages[id] = []string{"geometry", "shading"}
		if edit.Kind == EditMaterial {
			tileStages[id] = []string{"shading"}
		}
		if edit.Kind == EditDensity {
			tileStages[id] = []string{"geometry", "shading"}
		}
	}
	for id, reasons := range tileReasons {
		rep.InvalidatedTiles = append(rep.InvalidatedTiles, id)
		rep.Items = append(rep.Items, TileInvalidation{
			TileID: id, Patches: tilePatches[id], Reasons: dedupe(reasons), Stages: dedupe(tileStages[id]),
		})
	}
	rep.InvalidatedTiles = model.SortTiles(rep.InvalidatedTiles)
	sort.Slice(rep.Items, func(i, j int) bool { return rep.Items[i].TileID < rep.Items[j].TileID })
	invalid := map[int]bool{}
	for _, id := range rep.InvalidatedTiles {
		invalid[id] = true
		rep.DownstreamStages = append(rep.DownstreamStages, fmt.Sprintf("shade:tile:%d", id))
	}
	for id := 0; id < newScene.Tiles.Count(); id++ {
		if !invalid[id] {
			rep.ReusedTiles = append(rep.ReusedTiles, id)
		}
	}
	rep.AffectedPatches = []int{edit.PatchID}
	rep.Graph = Build(newScene)
	return rep
}

func reasonFor(edit Edit, side string) string {
	switch edit.Kind {
	case EditControl:
		return fmt.Sprintf("control-point P%d[%d,%d] %s geometry", edit.PatchID, edit.Row, edit.Col, side)
	case EditMaterial:
		return fmt.Sprintf("material parameter P%d %s shading", edit.PatchID, side)
	case EditDensity:
		return fmt.Sprintf("tessellation density P%d %s geometry", edit.PatchID, side)
	default:
		return string(edit.Kind)
	}
}

func patchState(scene *render.Scene, id int) (model.PatchState, bool) {
	for _, p := range scene.Patches {
		if p.PatchID == id {
			return model.PatchState{Source: p.Source, Material: p.Material}, true
		}
	}
	return model.PatchState{}, false
}

// AuditScope reports both under- and over-invalidation. The expected set is
// computed from the actual changes and can be supplied by a verifier.
func AuditScope(report Report, expectedTiles []int) *model.Diagnostic {
	want := map[int]bool{}
	got := map[int]bool{}
	for _, id := range expectedTiles {
		want[id] = true
	}
	for _, id := range report.InvalidatedTiles {
		got[id] = true
	}
	var mismatch []int
	var reasons []string
	for id := range want {
		if !got[id] {
			mismatch = append(mismatch, id)
			reasons = append(reasons, fmt.Sprintf("tile %d requires invalidation but was reused", id))
		}
	}
	for id := range got {
		if !want[id] {
			mismatch = append(mismatch, id)
			reasons = append(reasons, fmt.Sprintf("tile %d was invalidated without coverage change", id))
		}
	}
	if len(mismatch) == 0 {
		return nil
	}
	sort.Ints(mismatch)
	return &model.Diagnostic{
		Code: model.CodeScope, Message: "invalidation scope mismatch",
		PatchID: -1, TileID: -1, Field: "invalidatedTiles",
		MislabeledTiles: mismatch, Reasons: reasons,
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func patchID(id int) string { return fmt.Sprintf("patch:%d", id) }
func controlID(p, r, c int) string {
	return fmt.Sprintf("control:%d:%d:%d", p, r, c)
}
func materialID(id int) string { return fmt.Sprintf("material:%d", id) }
func densityID(id int) string  { return fmt.Sprintf("density:%d", id) }
func tileID(id int) string     { return fmt.Sprintf("tile:%d", id) }
func shadeID(id int) string    { return fmt.Sprintf("shade:tile:%d", id) }
