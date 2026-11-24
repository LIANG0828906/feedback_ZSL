package engine

import (
	"sort"

	"cadlab/internal/deps"
	"cadlab/internal/model"
	"cadlab/internal/render"
)

type DiffView struct {
	Equal      bool              `json:"equal"`
	PixelCount int               `json:"pixelCount"`
	TileCount  int               `json:"tileCount"`
	Message    string            `json:"message"`
	Diagnostic *model.Diagnostic `json:"diagnostic,omitempty"`
}

type Comparison struct {
	Version             int64             `json:"version"`
	FullVsIncremental   DiffView          `json:"fullVsIncremental"`
	FullVsResume        *DiffView         `json:"fullVsResume,omitempty"`
	ResumeVsIncremental *DiffView         `json:"resumeVsIncremental,omitempty"`
	CoverageEqual       bool              `json:"coverageEqual"`
	ShadingEqual        bool              `json:"shadingEqual"`
	FirstDiffPixel      *model.PixelPoint `json:"firstDiffPixel,omitempty"`
	FirstDiffTile       int               `json:"firstDiffTile"`
	FirstDiffField      string            `json:"firstDiffField"`
}

func diffView(a, b *render.Frame) DiffView {
	if a == nil || b == nil {
		return DiffView{Equal: false, Message: "missing path frame"}
	}
	diag := render.CompareFrames(a, b)
	v := DiffView{Equal: diag == nil, PixelCount: a.Width * a.Height, TileCount: a.Grid.Count()}
	if diag == nil {
		v.Message = "pixel-identical; coverage and luminance histograms identical"
		return v
	}
	v.Message = diag.Error()
	v.Diagnostic = diag
	return v
}

func (e *Engine) compareIncrementalToReference(inc, ref *render.Frame, invalidated map[int]bool) *Comparison {
	cmp := &Comparison{Version: ref.Version, FullVsIncremental: diffView(ref, inc)}
	changedTiles := changedTileIDs(ref, inc)
	var mislabeled []int
	for _, id := range changedTiles {
		if !invalidated[id] {
			mislabeled = append(mislabeled, id)
		}
	}
	if len(mislabeled) > 0 {
		sort.Ints(mislabeled)
		cmp.FullVsIncremental.Diagnostic = &model.Diagnostic{
			Code: model.CodeScope, Message: "reused tiles differ from full reference",
			PatchID: -1, TileID: mislabeled[0], Field: "invalidatedTiles",
			MislabeledTiles: mislabeled,
			Reasons:         []string{"screen tile changed without being placed in the invalidation set"},
		}
		cmp.FullVsIncremental.Message = cmp.FullVsIncremental.Diagnostic.Error()
	}
	if cmp.FullVsIncremental.Diagnostic != nil {
		d := cmp.FullVsIncremental.Diagnostic
		cmp.FirstDiffPixel = d.FirstPixel
		cmp.FirstDiffTile = d.TileID
		cmp.FirstDiffField = d.Field
		return cmp
	}
	cmp.CoverageEqual = coverageEqual(ref, inc)
	cmp.ShadingEqual = shadingEqual(ref, inc)
	return cmp
}

func changedTileIDs(a, b *render.Frame) []int {
	var changed []int
	for id := 0; id < a.Grid.Count(); id++ {
		ta, oka := a.TileByID(id)
		tb, okb := b.TileByID(id)
		if !oka || !okb {
			changed = append(changed, id)
			continue
		}
		if ta.Stats.Key() != tb.Stats.Key() || !tileBytesEqual(ta, tb) {
			changed = append(changed, id)
		}
	}
	return changed
}

func tileBytesEqual(a, b render.TileResult) bool {
	if len(a.Color) != len(b.Color) || len(a.Depth) != len(b.Depth) {
		return false
	}
	for i := range a.Color {
		if a.Color[i] != b.Color[i] {
			return false
		}
	}
	for i := range a.Depth {
		if a.Depth[i] != b.Depth[i] {
			return false
		}
	}
	return true
}

func coverageEqual(a, b *render.Frame) bool {
	for i := range a.Stats {
		if a.Stats[i].SurfacePixels != b.Stats[i].SurfacePixels || a.Stats[i].Coverage != b.Stats[i].Coverage {
			return false
		}
	}
	return true
}

func shadingEqual(a, b *render.Frame) bool {
	for i := range a.Stats {
		if a.Stats[i].LuminanceBins != b.Stats[i].LuminanceBins {
			return false
		}
	}
	return true
}

func (e *Engine) ComparePaths() (*Comparison, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ref := e.reference
	if ref == nil {
		ref = e.latestExact
	}
	if ref == nil {
		return nil, model.Diag(model.CodeDiff, "no full reference frame available")
	}
	cmp := &Comparison{Version: ref.Version}
	if e.latestIncremental != nil {
		cmp.FullVsIncremental = diffView(ref, e.latestIncremental)
		cmp.CoverageEqual = coverageEqual(ref, e.latestIncremental)
		cmp.ShadingEqual = shadingEqual(ref, e.latestIncremental)
	} else {
		cmp.FullVsIncremental = DiffView{Equal: false, Message: "incremental frame not rendered"}
	}
	if e.latestResume != nil {
		v := diffView(ref, e.latestResume)
		cmp.FullVsResume = &v
		if e.latestIncremental != nil {
			iv := diffView(e.latestIncremental, e.latestResume)
			cmp.ResumeVsIncremental = &iv
		}
	}
	if cmp.FullVsIncremental.Diagnostic != nil {
		d := cmp.FullVsIncremental.Diagnostic
		cmp.FirstDiffPixel, cmp.FirstDiffTile, cmp.FirstDiffField = d.FirstPixel, d.TileID, d.Field
	}
	e.lastCompare = cmp
	return cmp, nil
}

func (e *Engine) AuditScope(report deps.Report, expected []int) *model.Diagnostic {
	return deps.AuditScope(report, expected)
}

func (e *Engine) ValidatePathSnapshot(a, b *render.Frame) *model.Diagnostic {
	if a == nil || b == nil {
		return model.Diag(model.CodeVersion, "missing path frame")
	}
	if a.Version != b.Version {
		return &model.Diagnostic{
			Code: model.CodeVersion, Message: "preview/exact path snapshot versions differ",
			PatchID: -1, TileID: -1, Field: "snapshotVersion",
			VersionA: a.Version, VersionB: b.Version,
		}
	}
	if a.Camera != b.Camera {
		return &model.Diagnostic{
			Code: model.CodeVersion, Message: "path cameras/intrinsics differ",
			PatchID: -1, TileID: -1, Field: "camera",
			VersionA: a.Version, VersionB: b.Version,
		}
	}
	if a.Clip != b.Clip {
		return &model.Diagnostic{
			Code: model.CodeVersion, Message: "path clipping planes differ",
			PatchID: -1, TileID: -1, Field: "clipPlane",
			VersionA: a.Version, VersionB: b.Version,
		}
	}
	return nil
}
