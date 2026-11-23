package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"meshforge/internal/errs"
	"meshforge/internal/generator"
	"meshforge/internal/mesh"
)

type stageOutput struct {
	Mesh    mesh.Mesh
	Stats   mesh.Stats
	Details map[string]any
}

type PipelineState struct {
	RunID            string               `json:"run_id"`
	InputHash        string               `json:"input_hash"`
	ParameterVersion string               `json:"parameter_version"`
	AlgorithmVersion string               `json:"algorithm_version"`
	Decision         Decision             `json:"decision"`
	Stages           []StageRecord        `json:"stages"`
	Progress         Progress             `json:"progress"`
	ResumeStatus     string               `json:"resume_status"`
	ResumeReason     string               `json:"resume_reason,omitempty"`
	FinalStats       *mesh.Stats          `json:"final_stats,omitempty"`
	TopologyCurve    []TopologyCurvePoint `json:"topology_curve"`
	StageOrder       []StageName          `json:"stage_order"`
}

type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

type TopologyCurvePoint struct {
	Stage     StageName `json:"stage"`
	Vertices  int       `json:"vertices"`
	Faces     int       `json:"faces"`
	Edges     int       `json:"edges"`
	Euler     int       `json:"euler_characteristic"`
	Boundary  int       `json:"boundary_loops"`
	Committed bool      `json:"committed"`
}

type Pipeline struct {
	mu     sync.Mutex
	opMu   sync.Mutex
	sample generator.Sample
	cfg    Config

	decision         Decision
	stages           []StageRecord
	meshes           []mesh.Mesh
	checkpoint       *CheckpointStore
	runID            string
	inputHash        string
	paramVersion     string
	algorithmVersion string
	resumeStatus     string
	resumeReason     string
}

func NewPipeline(sample generator.Sample, cfg Config, checkpointDir string, resume bool) (*Pipeline, error) {
	cfg = cfg.Normalized()
	if e := cfg.Validate(); e != nil {
		return nil, e
	}
	decision, e := Decide(sample.Mesh, cfg)
	if e != nil {
		return nil, e
	}
	inputHash := sample.Mesh.ContentHash()
	paramVersion := cfg.ParameterVersion()
	runID := stableRunID(inputHash, paramVersion)
	p := &Pipeline{
		sample:           sample,
		cfg:              cfg,
		decision:         decision,
		stages:           make([]StageRecord, len(StageOrder)),
		meshes:           make([]mesh.Mesh, len(StageOrder)),
		checkpoint:       NewCheckpointStore(checkpointDir),
		runID:            runID,
		inputHash:        inputHash,
		paramVersion:     paramVersion,
		algorithmVersion: AlgorithmVersion,
		resumeStatus:     "fresh",
	}
	for i, name := range StageOrder {
		p.stages[i] = StageRecord{
			Index: i, Name: name, Status: StatusPending, ParamsHash: paramVersion,
		}
	}
	resumed := false
	if !resume {
		if err := p.checkpoint.Invalidate(); err != nil {
			return nil, errs.New(errs.CodeIO, "checkpoint reset failed: %v", err)
		}
	}
	if resume {
		state, invalid, err := p.checkpoint.Load(inputHash, paramVersion, AlgorithmVersion)
		if err != nil {
			return nil, errs.New(errs.CodeIO, "checkpoint load failed: %v", err)
		}
		if invalid != "" {
			p.resumeStatus = "invalidated"
			p.resumeReason = invalid
			if err := p.checkpoint.Invalidate(); err != nil {
				return nil, errs.New(errs.CodeIO, "checkpoint invalidation failed: %v", err)
			}
		} else if state != nil {
			if err := p.restore(state); err != nil {
				return nil, errs.New(errs.CodeIO, "checkpoint restore failed: %v", err)
			}
			p.resumeStatus = fmt.Sprintf("resumed_stage_%d", state.Snapshot.StageIndex)
			resumed = true
		}
	}
	if !resumed {
		p.checkpoint.ResetManifestForNewRun(runID, inputHash, paramVersion, AlgorithmVersion)
	}
	return p, nil
}

func stableRunID(inputHash, paramVersion string) string {
	sum := sha256.Sum256([]byte(inputHash + "|" + paramVersion + "|" + AlgorithmVersion))
	return "run-" + hex.EncodeToString(sum[:8])
}

func (p *Pipeline) restore(state *ResumeState) error {
	if len(state.Snapshot.Stages) != len(StageOrder) {
		return fmt.Errorf("snapshot stage matrix has wrong length")
	}
	last := state.Snapshot.StageIndex
	copy(p.stages, state.Snapshot.Stages)
	for i := 0; i <= last; i++ {
		if p.stages[i].Status != StatusCommitted {
			return fmt.Errorf("snapshot has non-committed stage %d", i)
		}
		p.stages[i].ParamsHash = state.Snapshot.ParameterVersion
	}
	for i := last + 1; i < len(p.stages); i++ {
		p.stages[i].Status = StatusPending
		p.stages[i].ParamsHash = p.paramVersion
		p.stages[i].Error = nil
		p.stages[i].MeshHash = ""
		p.stages[i].ParentHash = ""
	}
	p.meshes[last] = state.Mesh
	p.checkpoint.ResetManifestForNewRun(
		state.Manifest.RunID,
		state.Manifest.InputHash,
		state.Manifest.ParameterVersion,
		state.Manifest.AlgorithmVersion,
	)
	// Restore the on-disk commit list without rewriting snapshots.
	p.checkpoint.manifest = state.Manifest
	return nil
}

func (p *Pipeline) Run() error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	for i := range p.stages {
		p.mu.Lock()
		status := p.stages[i].Status
		p.mu.Unlock()
		if status == StatusCommitted {
			continue
		}
		if status == StatusFailed {
			return p.stageError(i)
		}
		if status == StatusProcessing {
			return errs.New(errs.CodeState, "stage %s is already processing", p.stages[i].Name)
		}
		if err := p.runOneLocked(i); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) RunStage(index int) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	if index < 0 || index >= len(p.stages) {
		return errs.New(errs.CodeState, "stage index %d out of range", index)
	}
	for i := 0; i < index; i++ {
		if p.stages[i].Status != StatusCommitted {
			return errs.New(errs.CodeState, "cannot run stage %s before %s is committed",
				p.stages[index].Name, p.stages[i].Name)
		}
	}
	if p.stages[index].Status == StatusCommitted {
		return errs.New(errs.CodeState, "stage %s is committed and frozen", p.stages[index].Name)
	}
	if p.stages[index].Status == StatusFailed {
		return errs.New(errs.CodeState, "stage %s is failed; use Retry", p.stages[index].Name)
	}
	return p.runOneLocked(index)
}

func (p *Pipeline) Retry(index int) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	if index < 0 || index >= len(p.stages) {
		return errs.New(errs.CodeState, "stage index %d out of range", index)
	}
	if p.stages[index].Status != StatusFailed {
		return errs.New(errs.CodeState, "only a failed stage can be retried; %s is %s",
			p.stages[index].Name, p.stages[index].Status)
	}
	for i := 0; i < index; i++ {
		if p.stages[i].Status != StatusCommitted {
			return errs.New(errs.CodeState, "committed prefix is incomplete at %s", p.stages[i].Name)
		}
	}
	return p.runOneLocked(index)
}

func (p *Pipeline) runOneLocked(index int) error {
	stage := p.stages[index].Name
	p.mu.Lock()
	if index > 0 && p.stages[index-1].Status != StatusCommitted {
		p.mu.Unlock()
		return errs.New(errs.CodeState, "previous stage %s is not committed", p.stages[index-1].Name)
	}
	from := p.stages[index].Status
	to := StatusProcessing
	if !legalTransition(from, to) {
		p.mu.Unlock()
		return transitionError(from, to, stage)
	}
	p.stages[index].Status = to
	p.stages[index].Error = nil
	p.stages[index].StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	p.mu.Unlock()

	start := time.Now()
	back := p.sample.Mesh
	if index > 0 {
		back = p.meshes[index-1]
	}
	out, err := computeStage(index, back, p.sample, p.cfg, p.decision)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		ce, ok := errs.As(err)
		if !ok {
			ce = errs.New(errs.CodeGeom, "%v", err)
		}
		ce.Stage = string(stage)
		p.mu.Lock()
		p.stages[index].Status = StatusFailed
		p.stages[index].Error = ce
		p.stages[index].DurationMS = elapsed
		p.mu.Unlock()
		return ce
	}

	parentHash := p.inputHash
	if index > 0 {
		parentHash = p.meshes[index-1].ContentHash()
	}
	meshHash := out.Mesh.ContentHash()
	stats := out.Stats
	p.mu.Lock()
	p.meshes[index] = out.Mesh
	p.stages[index].Status = StatusCommitted
	p.stages[index].ParamsHash = p.paramVersion
	p.stages[index].MeshHash = meshHash
	p.stages[index].ParentHash = parentHash
	p.stages[index].Stats = &stats
	p.stages[index].Details = out.Details
	p.stages[index].CommittedAt = time.Now().UTC().Format(time.RFC3339Nano)
	p.stages[index].DurationMS = elapsed
	snap := Snapshot{
		Format:           SnapshotFormat,
		RunID:            p.runID,
		InputHash:        p.inputHash,
		ParameterVersion: p.paramVersion,
		AlgorithmVersion: p.algorithmVersion,
		StageIndex:       index,
		Stage:            stage,
		ParentHash:       parentHash,
		MeshHash:         meshHash,
		Mesh:             mesh.NewDocument(out.Mesh),
		Stages:           cloneStageRecords(p.stages),
	}
	p.mu.Unlock()
	if err := p.checkpoint.Commit(snap); err != nil {
		ce := errs.New(errs.CodeIO, "checkpoint commit failed for %s: %v", stage, err)
		ce.Stage = string(stage)
		p.mu.Lock()
		p.stages[index].Status = StatusFailed
		p.stages[index].Error = ce
		p.mu.Unlock()
		return ce
	}
	return nil
}

func cloneStageRecords(in []StageRecord) []StageRecord {
	out := make([]StageRecord, len(in))
	copy(out, in)
	for i := range out {
		if out[i].Stats != nil {
			s := *out[i].Stats
			out[i].Stats = &s
		}
		if out[i].Details != nil {
			out[i].Details = cloneMap(out[i].Details)
		}
		if out[i].Error != nil {
			e := *out[i].Error
			out[i].Error = &e
		}
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func computeStage(index int, back mesh.Mesh, sample generator.Sample, cfg Config, decision Decision) (stageOutput, error) {
	stage := StageOrder[index]
	switch stage {
	case StageDiagnose:
		diag, err := Diagnose(sample.Mesh, sample.Truth)
		if err != nil {
			return stageOutput{}, err
		}
		details := map[string]any{
			"expected":       diag.Expected,
			"actual":         diag.Stats,
			"boundary_loops": len(diag.BoundaryLoops.Loops),
			"open_chains":    diag.BoundaryLoops.OpenChains,
			"decision_mode":  decision.Selected,
			"decision_rule":  decision.Rule,
		}
		return stageOutput{Mesh: sample.Mesh.Clone(), Stats: diag.Stats, Details: details}, nil
	case StageWeld:
		probe, err := chunkProbe(back, cfg, decision, string(stage))
		if err != nil {
			return stageOutput{}, err
		}
		out, report, welErr := WeldAndCanonicalize(back, WeldOptions{
			Tolerance: cfg.WeldTolerance, MinComponentPct: cfg.MinComponentPct,
		})
		if welErr != nil {
			return stageOutput{}, welErr
		}
		stats := buildStats(out, false)
		return stageOutput{
			Mesh:  out,
			Stats: stats,
			Details: map[string]any{
				"weld": report, "mode": decision.Selected,
				"chunk_probe": probe,
			},
		}, nil
	case StageFill:
		probe, err := chunkProbe(back, cfg, decision, string(stage))
		if err != nil {
			return stageOutput{}, err
		}
		out, report, fillErr := FillHoles(back, FillOptions{PreferConvexFan: cfg.PreferConvexFan})
		if fillErr != nil {
			return stageOutput{}, fillErr
		}
		if e := validateTruthTopology(out, sample.Truth, string(stage)); e != nil {
			return stageOutput{}, e
		}
		return stageOutput{
			Mesh: out, Stats: buildStats(out, false),
			Details: map[string]any{"fill": report, "chunk_probe": probe},
		}, nil
	case StageOrient:
		probe, err := chunkProbe(back, cfg, decision, string(stage))
		if err != nil {
			return stageOutput{}, err
		}
		out, report, orientErr := UnifyOrientation(back)
		if orientErr != nil {
			return stageOutput{}, orientErr
		}
		if e := validateTruthTopology(out, sample.Truth, string(stage)); e != nil {
			return stageOutput{}, e
		}
		return stageOutput{
			Mesh: out, Stats: buildStats(out, false),
			Details: map[string]any{"orient": report, "chunk_probe": probe},
		}, nil
	case StageDenoise:
		probe, err := chunkProbe(back, cfg, decision, string(stage))
		if err != nil {
			return stageOutput{}, err
		}
		out, report, smoothErr := SmoothLaplacian(back, SmoothOptions{
			Weight: cfg.SmoothWeight, Iterations: cfg.SmoothIterations,
		}, sample.Truth)
		if smoothErr != nil {
			return stageOutput{}, smoothErr
		}
		if e := validateTruthTopology(out, sample.Truth, string(stage)); e != nil {
			return stageOutput{}, e
		}
		if !report.Converged {
			e := errs.New(errs.CodePara,
				"laplacian parameters did not reduce known-point error (mean %.9g -> %.9g)",
				report.KnownBefore.Mean, report.KnownAfter.Mean)
			e.Details = report
			return stageOutput{}, e
		}
		return stageOutput{
			Mesh: out, Stats: buildStats(out, false),
			Details: map[string]any{"smooth": report, "chunk_probe": probe},
		}, nil
	case StageStats:
		if e := validateTruthTopology(back, sample.Truth, string(stage)); e != nil {
			return stageOutput{}, e
		}
		stats := buildStats(back, true)
		truthMismatch := []string{}
		if stats.Vertices != sample.Truth.Repaired.Vertices {
			truthMismatch = append(truthMismatch, "vertices")
		}
		if stats.Faces != sample.Truth.Repaired.Faces {
			truthMismatch = append(truthMismatch, "faces")
		}
		if stats.EulerCharacteristic != sample.Truth.Repaired.Euler {
			truthMismatch = append(truthMismatch, "euler_characteristic")
		}
		if stats.NonManifoldEdges != 0 {
			truthMismatch = append(truthMismatch, "non_manifold_edges")
		}
		if stats.BoundaryLoops != 0 || stats.OpenBoundaryChains != 0 {
			truthMismatch = append(truthMismatch, "boundary")
		}
		if stats.DuplicateVertices != 0 {
			truthMismatch = append(truthMismatch, "duplicate_vertices")
		}
		if stats.FlippedFaces != 0 || stats.OrientationConflictEdges != 0 {
			truthMismatch = append(truthMismatch, "orientation")
		}
		if stats.SelfIntersections != 0 {
			truthMismatch = append(truthMismatch, "self_intersections")
		}
		if len(truthMismatch) > 0 {
			e := errs.New(errs.CodeGeom, "final mesh failed truth checks: %v", truthMismatch)
			e.Details = stats
			return stageOutput{}, e
		}
		return stageOutput{
			Mesh: back.Clone(), Stats: stats,
			Details: map[string]any{"final": stats, "truth": sample.Truth.Repaired,
				"decision": decision},
		}, nil
	default:
		return stageOutput{}, fmt.Errorf("unknown stage %s", stage)
	}
}

func buildStats(m mesh.Mesh, withSelfIntersections bool) mesh.Stats {
	topo := mesh.BuildTopology(m)
	orientation := mesh.AnalyzeOrientation(m)
	boundary := mesh.BoundaryLoops(m)
	comps := mesh.FaceComponents(m, mesh.ComponentAllSharedEdges)
	nm := 0
	for _, e := range topo.Edges {
		if len(e.Uses) > 2 {
			nm++
		}
	}
	minArea := 0.0
	if len(m.Faces) > 0 {
		minArea = m.FaceArea(0)
		for i := 1; i < len(m.Faces); i++ {
			if a := m.FaceArea(i); a < minArea {
				minArea = a
			}
		}
	}
	stats := mesh.Stats{
		Vertices:                 len(m.Vertices),
		Faces:                    len(m.Faces),
		Edges:                    len(topo.Edges),
		EulerCharacteristic:      len(m.Vertices) - len(topo.Edges) + len(m.Faces),
		NonManifoldEdges:         nm,
		DuplicateVertices:        mesh.DuplicateVertexCount(m),
		FlippedFaces:             orientation.FlippedFaces,
		OrientationConflictEdges: orientation.ConflictEdges,
		BoundaryLoops:            len(boundary.Loops),
		OpenBoundaryChains:       boundary.OpenChains,
		Components:               len(comps),
		ThinSheetComponents:      mesh.ThinSheetComponentCount(m),
		UnusedVertices:           mesh.UnusedVertexCount(m),
		MinArea:                  minArea,
		TotalArea:                m.SurfaceArea(),
		SignedVolume:             m.SignedVolume(),
	}
	if withSelfIntersections {
		stats.SelfIntersections = mesh.CountSelfIntersections(m)
	}
	return stats
}

func chunkProbe(m mesh.Mesh, cfg Config, decision Decision, stage string) (map[string]any, error) {
	if decision.Selected != ModeSpatial {
		return map[string]any{"enabled": false, "stage": stage}, nil
	}
	faces := make([][3]int, len(m.Faces))
	for i, f := range m.Faces {
		faces[i] = f.Indices()
	}
	chunkSize := decision.Chunk.MaxFacesPerChunk
	if chunkSize < 1 {
		chunkSize = cfg.ChunkTargetFaces
	}
	pool := NewWorkerPool(cfg.MaxParallel, decision.Chunk.WorkerBufferFaces)
	summaries, err := pool.ChunkSummary(faces, chunkSize)
	if err != nil {
		e := errs.New(errs.CodeScale, "chunk worker buffer rejected task: %v", err)
		e.Item = "per_worker_buffer"
		return nil, e
	}
	raw, _ := json.Marshal(summaries)
	sum := sha256.Sum256(raw)
	return map[string]any{
		"enabled":             true,
		"stage":               stage,
		"chunks":              len(summaries),
		"chunk_size":          chunkSize,
		"max_parallel":        pool.MaxWorkers,
		"worker_buffer_faces": decision.Chunk.WorkerBufferFaces,
		"summary_hash":        hex.EncodeToString(sum[:8]),
	}, nil
}

func (p *Pipeline) State() PipelineState {
	p.mu.Lock()
	defer p.mu.Unlock()
	stages := cloneStageRecords(p.stages)
	state := PipelineState{
		RunID:            p.runID,
		InputHash:        p.inputHash,
		ParameterVersion: p.paramVersion,
		AlgorithmVersion: p.algorithmVersion,
		Decision:         p.decision,
		Stages:           stages,
		Progress:         Progress{Committed: p.committedCountLocked(), Total: len(StageOrder)},
		ResumeStatus:     p.resumeStatus,
		ResumeReason:     p.resumeReason,
		StageOrder:       append([]StageName(nil), StageOrder...),
	}
	for _, st := range stages {
		point := TopologyCurvePoint{Stage: st.Name, Committed: st.Status == StatusCommitted}
		if st.Stats != nil {
			point.Vertices = st.Stats.Vertices
			point.Faces = st.Stats.Faces
			point.Edges = st.Stats.Edges
			point.Euler = st.Stats.EulerCharacteristic
			point.Boundary = st.Stats.BoundaryLoops
		}
		state.TopologyCurve = append(state.TopologyCurve, point)
	}
	for i := len(stages) - 1; i >= 0; i-- {
		if stages[i].Stats != nil {
			s := *stages[i].Stats
			state.FinalStats = &s
			break
		}
	}
	return state
}

func (p *Pipeline) committedCountLocked() int {
	var n int
	for _, s := range p.stages {
		if s.Status == StatusCommitted {
			n++
		}
	}
	return n
}

func (p *Pipeline) stageError(index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stages[index].Error != nil {
		return p.stages[index].Error
	}
	return errs.New(errs.CodeState, "stage %s failed without an error record", p.stages[index].Name)
}

func (p *Pipeline) SetSmoothOptions(weight float64, iterations int) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stages[stageIndex(StageDenoise)].Status == StatusCommitted ||
		p.stages[stageIndex(StageStats)].Status == StatusCommitted {
		return errs.New(errs.CodeState, "denoise/stats are committed; smoothing parameters are frozen")
	}
	opt := SmoothOptions{Weight: weight, Iterations: iterations}
	if e := ValidateSmoothOptions(opt); e != nil {
		return e
	}
	oldCfg := p.cfg
	p.cfg.SmoothWeight = weight
	p.cfg.SmoothIterations = iterations
	newVersion := p.cfg.ParameterVersion()
	if err := p.checkpoint.SetParameterVersion(newVersion); err != nil {
		p.cfg = oldCfg
		return errs.New(errs.CodeIO, "failed to persist parameter version: %v", err)
	}
	p.paramVersion = newVersion
	for i, st := range p.stages {
		if st.Status != StatusCommitted {
			p.stages[i].ParamsHash = p.paramVersion
		}
	}
	return nil
}

func (p *Pipeline) SetWeldOptions(tolerance, minComponentPct float64) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stages[stageIndex(StageWeld)].Status == StatusCommitted {
		return errs.New(errs.CodeState, "weld is committed; weld parameters are frozen")
	}
	if tolerance < 0 {
		return errs.New(errs.CodeTolo, "weld tolerance must be non-negative")
	}
	if minComponentPct <= 0 || minComponentPct >= 1 {
		return errs.New(errs.CodePara, "min component percentage must be in (0,1)")
	}
	oldCfg := p.cfg
	p.cfg.WeldTolerance = tolerance
	p.cfg.MinComponentPct = minComponentPct
	newVersion := p.cfg.ParameterVersion()
	if err := p.checkpoint.SetParameterVersion(newVersion); err != nil {
		p.cfg = oldCfg
		return errs.New(errs.CodeIO, "failed to persist parameter version: %v", err)
	}
	p.paramVersion = newVersion
	for i, st := range p.stages {
		if st.Status != StatusCommitted {
			p.stages[i].ParamsHash = p.paramVersion
		}
	}
	return nil
}

func (p *Pipeline) MeshForStage(name StageName) (mesh.Mesh, bool) {
	i := stageIndex(name)
	if i < 0 {
		return mesh.Mesh{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.meshes[i].Vertices) > 0 {
		return p.meshes[i].Clone(), true
	}
	if p.stages[i].Status == StatusCommitted {
		return p.checkpoint.LoadStageMesh(i)
	}
	return mesh.Mesh{}, false
}

func (p *Pipeline) FinalMesh() (mesh.Mesh, bool) {
	return p.MeshForStage(StageStats)
}

func (p *Pipeline) Decision() Decision {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.decision
}
