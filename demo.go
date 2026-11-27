package terrainbake

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type BlockView struct {
	ID              string   `json:"id"`
	Rect            Rect     `json:"rect"`
	Budget          Budget   `json:"budget"`
	ToleranceUM     int64    `json:"tolerance_um"`
	HeightDeltaMM   int64    `json:"height_delta_mm"`
	Neighbors       []string `json:"neighbors"`
	Vertices        int      `json:"vertices"`
	Triangles       int      `json:"triangles"`
	LeafCount       int      `json:"leaf_count"`
	HeightErrorUM   int64    `json:"height_error_um"`
	BoundaryErrorUM int64    `json:"boundary_error_um"`
	Undersampled    bool     `json:"undersampled"`
	VertexDigest    string   `json:"vertex_digest"`
	MeshDigest      string   `json:"mesh_digest"`
	Status          string   `json:"status"`
}

type FaultLogEntry struct {
	Kind      string `json:"kind"`
	Code      string `json:"code"`
	BlockID   string `json:"block_id,omitempty"`
	Message   string `json:"message"`
	Isolated  bool   `json:"isolated"`
	Committed int    `json:"committed_blocks"`
}

type StateView struct {
	GeneratedBy    string            `json:"generated_by"`
	SourceVersion  SchemaVersion     `json:"source_version"`
	World          World             `json:"world"`
	Blocks         []BlockView       `json:"blocks"`
	Seams          []SeamSpec        `json:"seams"`
	Meshes         map[string]*Mesh  `json:"meshes"`
	SeamReports    []SeamReport      `json:"seam_reports"`
	Camera         []CameraSample    `json:"camera"`
	Truth          Truth             `json:"truth"`
	Invalidations  []Invalidation    `json:"invalidations"`
	Invalidated    []string          `json:"invalidated"`
	Reused         []string          `json:"reused"`
	Recomputed     []string          `json:"recomputed"`
	WrittenBlocks  []string          `json:"written_blocks"`
	CommitSequence []string          `json:"commit_sequence"`
	Queue          []QueueEvent      `json:"queue"`
	Budget         BudgetUsage       `json:"budget"`
	TruthCheck     *TruthCheck       `json:"truth_check,omitempty"`
	BlockErrors    map[string]*Error `json:"block_errors,omitempty"`
	AttributionErr *Error            `json:"attribution_error,omitempty"`
	Verification   *ThreePathReport  `json:"verification,omitempty"`
	FaultLog       []FaultLogEntry   `json:"fault_log"`
	ReusedCount    int               `json:"reused_count"`
}

type Demo struct {
	mu           sync.Mutex
	project      *Project
	config       EngineConfig
	cacheDir     string
	engine       *Engine
	last         *BakeResult
	lastPlan     []Invalidation
	verification *ThreePathReport
	faults       []FaultLogEntry
}

func NewDemo(cacheDir string, cfg EngineConfig) (*Demo, error) {
	p := NewGenerator(DefaultSeed).Project()
	eng, err := NewEngine(p, cfg, cacheDir)
	if err != nil {
		return nil, err
	}
	res, err := eng.Bake(context.Background())
	if err != nil {
		return nil, err
	}
	return &Demo{project: p, config: cfg, cacheDir: cacheDir, engine: eng, last: res}, nil
}

func (d *Demo) Project() *Project {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.project.Clone()
}

func (d *Demo) View() StateView {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.viewLocked()
}

func (d *Demo) viewLocked() StateView {
	v := StateView{
		GeneratedBy: d.project.GeneratedBy, SourceVersion: d.project.SourceVersion,
		World: d.project.World, Seams: append([]SeamSpec(nil), d.project.Seams...),
		Camera: append([]CameraSample(nil), d.project.Camera...),
		Truth:  d.project.Truth, Meshes: map[string]*Mesh{},
		Invalidations: append([]Invalidation(nil), d.lastPlan...),
		FaultLog:      append([]FaultLogEntry(nil), d.faults...),
	}
	if d.last != nil {
		v.Invalidated = d.last.Invalidated
		v.Reused = d.last.Reused
		v.Recomputed = d.last.Recomputed
		v.WrittenBlocks = d.last.WrittenBlocks
		v.CommitSequence = d.last.CommitSequence
		v.Queue = d.last.Queue
		v.Budget = d.last.Budget
		v.TruthCheck = d.last.TruthCheck
		v.BlockErrors = d.last.BlockErrors
		v.AttributionErr = d.last.AttributionErr
		v.SeamReports = d.last.Seams
		v.ReusedCount = d.last.ReusedCount
		if len(d.last.Invalidations) > 0 {
			v.Invalidations = append(v.Invalidations, d.last.Invalidations...)
		}
		for id, m := range d.last.Meshes {
			v.Meshes[id] = m
		}
	}
	v.Invalidations = dedupeInvalidations(v.Invalidations)
	for _, b := range d.project.Blocks {
		spec, st := d.engine.effectiveBlock(b.ID)
		bv := BlockView{
			ID: b.ID, Rect: b.Rect, Budget: spec.Budget, ToleranceUM: spec.ToleranceUM,
			HeightDeltaMM: st.HeightDeltaMM, Neighbors: append([]string(nil), b.Neighbors...),
			Status: "reused",
		}
		if m := v.Meshes[b.ID]; m != nil {
			bv.Vertices = len(m.Vertices)
			bv.Triangles = len(m.Triangles)
			bv.LeafCount = m.LeafCount
			bv.HeightErrorUM = m.HeightErrorMaxUM
			bv.BoundaryErrorUM = m.BoundaryErrorMaxUM
			bv.Undersampled = m.Undersampled
			bv.VertexDigest = m.VertexDigest
			bv.MeshDigest = DigestMesh(m)
		}
		if v.BlockErrors != nil && v.BlockErrors[b.ID] != nil {
			bv.Status = "failed:" + string(v.BlockErrors[b.ID].Code)
		} else if containsString(v.Recomputed, b.ID) {
			bv.Status = "recomputed"
		}
		v.Blocks = append(v.Blocks, bv)
	}
	sort.Slice(v.Invalidations, func(i, j int) bool {
		if v.Invalidations[i].BlockID != v.Invalidations[j].BlockID {
			return v.Invalidations[i].BlockID < v.Invalidations[j].BlockID
		}
		return v.Invalidations[i].Reason < v.Invalidations[j].Reason
	})
	v.Verification = d.verification
	return v
}

func (d *Demo) ApplyEdit(ctx context.Context, edit Edit) (StateView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	plan, err := d.engine.ApplyEdit(edit)
	if err != nil {
		return StateView{}, err
	}
	res, err := d.engine.Bake(ctx)
	if err != nil {
		return StateView{}, err
	}
	d.lastPlan = plan
	d.last = res
	return d.viewLocked(), nil
}

func (d *Demo) Verify(ctx context.Context) (*ThreePathReport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	edits := DefaultVerificationEdits()
	dir, err := os.MkdirTemp("", "terrain-three-path-*")
	if err != nil {
		return nil, &Error{Code: CodeBlock, Field: "workdir", Message: err.Error()}
	}
	defer os.RemoveAll(dir)
	report, err := VerifyThreePaths(ctx, d.project, d.config, edits, dir)
	d.verification = report
	return report, err
}

func DefaultVerificationEdits() []Edit {
	h1 := int64(140)
	h2 := int64(-90)
	budget := Budget{MaxVertices: 2800, MaxTriangles: 3600}
	tol := int64(1600)
	return []Edit{
		{BlockID: "B02", HeightDeltaMM: &h1},
		{BlockID: "B04", Budget: &budget},
		{BlockID: "B01", ToleranceUM: &tol},
		{BlockID: "B05", HeightDeltaMM: &h2},
	}
}

func (d *Demo) InjectFault(ctx context.Context, kind, blockID string) (StateView, *Error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if blockID == "" {
		blockID = "B02"
	}
	var code Code
	var msg string
	var faultErr *Error
	isolated := false
	switch kind {
	case "decode":
		err := injectDecodeFault(d.project)
		faultErr = asError(err)
		code, msg = codeOf(err), err.Error()
	case "overlap", "over":
		err := injectOverlapFault(d.project)
		faultErr = asError(err)
		code, msg = codeOf(err), err.Error()
	case "adjacency", "adj":
		err := injectAdjacencyFault(d.project)
		faultErr = asError(err)
		code, msg = codeOf(err), err.Error()
	case "block":
		if err := d.engine.SetFault(blockID, "block"); err != nil {
			faultErr = asError(err)
			code, msg = codeOf(err), err.Error()
			break
		}
		res, err := d.engine.Bake(ctx)
		if err != nil {
			faultErr = asError(err)
			code, msg = codeOf(err), err.Error()
			break
		}
		d.last = res
		if be := res.BlockErrors[blockID]; be != nil {
			code, msg = be.Code, be.Error()
			cp := *be
			faultErr = &cp
		} else {
			code, msg = CodeBlock, "fault was not reported"
			faultErr = &Error{Code: CodeBlock, Field: "block", BlockID: blockID, Message: msg}
		}
		isolated = len(res.Meshes) > 0
		_ = d.engine.SetFault(blockID, "")
	default:
		code, msg = CodeSchema, "unknown fault kind"
	}
	entry := FaultLogEntry{
		Kind: kind, Code: string(code), BlockID: blockID, Message: msg,
		Isolated: isolated,
	}
	if d.last != nil {
		entry.Committed = len(d.last.Meshes)
	}
	d.faults = append(d.faults, entry)
	if faultErr == nil {
		faultErr = &Error{Code: code, Field: kind, BlockID: blockID, Message: msg}
	}
	if faultErr.BlockID == "" {
		faultErr.BlockID = blockID
	}
	return d.viewLocked(), faultErr
}

func codeOf(err error) Code {
	if err == nil {
		return ""
	}
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return CodeBlock
}

func injectDecodeFault(p *Project) error {
	data, err := EncodeProject(p, SchemaV2)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	truth, _ := doc["truth"].(map[string]any)
	vertices, _ := truth["vertices"].([]any)
	if len(vertices) == 0 {
		return errDecode("truth.vertices", "no sample available for corruption")
	}
	first, _ := vertices[0].(map[string]any)
	first["height_mm"] = "corrupted-height"
	data, _ = json.Marshal(doc)
	_, err = LoadProject(data)
	return err
}

func injectOverlapFault(p *Project) error {
	data, err := EncodeProject(p, SchemaV1)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	chunks, _ := doc["chunks"].([]any)
	if len(chunks) < 2 {
		return errOver("chunks[1].bounds", "chunk0", "chunk1", "not enough chunks to overlap")
	}
	c0, _ := chunks[0].(map[string]any)
	c1, _ := chunks[1].(map[string]any)
	b0, _ := c0["bounds"].(map[string]any)
	b1, _ := c1["bounds"].(map[string]any)
	b1["min_x"] = b0["min_x"]
	data, _ = json.Marshal(doc)
	_, err = LoadProject(data)
	return err
}

func injectAdjacencyFault(p *Project) error {
	data, err := EncodeProject(p, SchemaV3)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	blocks, _ := doc["blocks"].([]any)
	if len(blocks) == 0 {
		return errAdj("blocks[0].adjacent", "adjacent", "no block available")
	}
	first, _ := blocks[0].(map[string]any)
	adj, _ := first["adjacent"].([]any)
	first["adjacent"] = append(adj, "B99")
	data, _ = json.Marshal(doc)
	_, err = LoadProject(data)
	return err
}

type CameraFrame struct {
	Sample        CameraSample `json:"sample"`
	NearestIndex  int          `json:"nearest_index"`
	VisibleBlocks []string     `json:"visible_blocks"`
}

func CameraAt(p *Project, tMS int64) CameraFrame {
	if p == nil || len(p.Camera) == 0 {
		return CameraFrame{}
	}
	best := 0
	bestDelta := int64(math.MaxInt64)
	for i, c := range p.Camera {
		d := c.TimeMS - tMS
		if d < 0 {
			d = -d
		}
		if d < bestDelta {
			best, bestDelta = i, d
		}
	}
	c := p.Camera[best]
	var visible []string
	rad := c.YawDeg * math.Pi / 180
	halfFOV := c.FOVDeg * math.Pi / 360
	for _, b := range p.Blocks {
		cx := float64(b.Rect.MinX+b.Rect.MaxX) / 2
		cy := float64(b.Rect.MinY+b.Rect.MaxY) / 2
		dx := cx - float64(c.Position.X)
		dy := cy - float64(c.Position.Y)
		dist := math.Hypot(dx, dy)
		ang := math.Atan2(dy, dx)
		diff := math.Abs(math.Remainder(ang-rad, 2*math.Pi))
		if b.ID == c.Focus || (dist <= c.Far && diff <= halfFOV) {
			visible = append(visible, b.ID)
		}
	}
	sort.Strings(visible)
	return CameraFrame{Sample: c, NearestIndex: best, VisibleBlocks: visible}
}

// WriteSamples emits the same terrain in all three versions plus a tiny
// manifest for command-line verification. It uses only generated truth.
func WriteSamples(dir string, p *Project) (map[string]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, v := range []SchemaVersion{SchemaV1, SchemaV2, SchemaV3} {
		data, err := EncodeProject(p, v)
		if err != nil {
			return nil, err
		}
		name := filepath.Join(dir, fmt.Sprintf("terrain.%s.json", v))
		if err := os.WriteFile(name, data, 0o644); err != nil {
			return nil, err
		}
		files[v.String()] = name
	}
	manifest := map[string]any{
		"generated_by": p.GeneratedBy,
		"seed":         p.World.Seed,
		"files":        files,
		"note":         "all inputs and truth are produced by the deterministic generator",
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	name := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(name, append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	files["manifest"] = name
	return files, nil
}

func SchemaCatalog(p *Project) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, v := range []SchemaVersion{SchemaV1, SchemaV2, SchemaV3} {
		data, err := EncodeProject(p, v)
		if err != nil {
			return nil, err
		}
		out[v.String()] = data
	}
	return out, nil
}

func LoadSampleFiles(dir string) (map[SchemaVersion]*Project, error) {
	out := map[SchemaVersion]*Project{}
	for _, v := range []SchemaVersion{SchemaV1, SchemaV2, SchemaV3} {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("terrain.%s.json", v)))
		if err != nil {
			return nil, err
		}
		p, err := LoadProject(data)
		if err != nil {
			return nil, err
		}
		out[v] = p
	}
	return out, nil
}

func dedupeInvalidations(in []Invalidation) []Invalidation {
	seen := map[string]bool{}
	out := make([]Invalidation, 0, len(in))
	for _, iv := range in {
		key := iv.BlockID + "|" + iv.Reason + "|" + iv.Source + "|" + iv.Via + "|" + iv.Field
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, iv)
	}
	return out
}
