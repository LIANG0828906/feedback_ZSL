package terrain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type AcceptanceReport struct {
	OK             bool                 `json:"ok"`
	Seed           int64                `json:"seed"`
	WorkDir        string               `json:"work_dir"`
	SamplePath     string               `json:"sample_path"`
	Checks         []CheckResult        `json:"checks"`
	BlockSummaries []BlockRunStat       `json:"block_summaries"`
	FaultReports   []*AttributionReport `json:"fault_reports"`
	CommitSequence []int                `json:"commit_sequence"`
	GeneratedAt    string               `json:"generated_at"`
}

func (r *AcceptanceReport) check(name string, err error, detail string) bool {
	if err != nil {
		r.Checks = append(r.Checks, CheckResult{Name: name, Passed: false, Detail: err.Error()})
		r.OK = false
		return false
	}
	r.Checks = append(r.Checks, CheckResult{Name: name, Passed: true, Detail: detail})
	return true
}

func RunAcceptance(workDir string) (*AcceptanceReport, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	report := &AcceptanceReport{OK: true, Seed: 42, WorkDir: workDir, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	ctx := context.Background()

	project := GenerateSample(42)
	samplePath := filepath.Join(workDir, "terrain-seed-42.json")
	report.SamplePath = samplePath
	if err := WriteProject(samplePath, project); err != nil {
		return nil, err
	}
	loaded, err := LoadProject(samplePath)
	report.check("generate_and_load_schema", err, fmt.Sprintf("%d blocks / %d boundaries", len(project.Blocks), len(project.SharedBoundaries)))
	if err != nil {
		return report, nil
	}
	project = loaded

	runSchemaCases(report, project)

	full, err := CompileProject(project)
	if !report.check("deterministic_compile", err, "alignment plan and Delaunay meshes built") {
		return report, writeReport(workDir, report)
	}
	runTruthChecks(report, project, full)
	runMonotonicCheck(report, project)
	runDeterminismCheck(report, project, full)
	runFaultAttribution(report, project)
	runBudgetQueueCheck(report)

	fullPath := filepath.Join(workDir, "full.db")
	_ = os.Remove(fullPath)
	fullSvc, err := NewService(project, ServiceOptions{DBPath: fullPath, Budget: BudgetConfig{MaxParallel: 4, PerWorkerVertexBuffer: 4096, MeshMemoryBudgetBytes: 64 << 20}})
	if err != nil {
		return nil, err
	}
	fullManifest, fullErr := fullSvc.RebuildAll(ctx)
	report.check("parallel_full_commit", fullErr, manifestDetail(fullManifest))
	if fullErr == nil {
		meshes := serviceMeshes(fullSvc.State())
		err = CompareMeshMaps(full.Meshes, meshes)
		report.check("parallel_full_matches_pure_compile", err, "vertices/triangles/errors equal")
		err = compareReportMaps(full.Reports, fullSvc.State().Reports)
		report.check("parallel_boundary_stats_match", err, "fold angle reports equal")
		seq := sequenceBlockNumbers(fullManifest.SnapshotSequence)
		report.CommitSequence = seq
		report.check("stable_commit_sequence", expectIntSlice([]int{0, 1, 2, 3}, seq), fmt.Sprintf("sequence=%v", seq))
		report.BlockSummaries = fullManifest.Blocks
	}
	_ = fullSvc.Close()

	runIncrementalAndRecovery(ctx, report, workDir, project, full)
	runVersionMismatchCheck(report, workDir, project)
	runFailureIsolationCheck(ctx, report, workDir, project, full)

	exportPath := filepath.Join(workDir, "export.json")
	if fullSvc != nil {
		data, err := fullSvc.ExportJSON()
		report.check("export_json", err, exportPath)
		if err == nil {
			_ = os.WriteFile(exportPath, data, 0o644)
		}
	}

	for _, fc := range project.Truth.FaultCases {
		rep, _ := AttributeBoundary(fc)
		if rep != nil {
			report.FaultReports = append(report.FaultReports, rep)
		}
	}
	return report, writeReport(workDir, report)
}

func writeReport(workDir string, report *AcceptanceReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workDir, "acceptance-report.json"), data, 0o644)
}

func runSchemaCases(report *AcceptanceReport, p *Project) {
	raw, _ := json.Marshal(p)
	var root map[string]any
	_ = json.Unmarshal(raw, &root)
	blocks := root["blocks"].([]any)
	b2 := blocks[2].(map[string]any)
	delete(b2, "sample_budget")
	bad, _ := json.Marshal(root)
	_, err := ParseProject(bad)
	coded, ok := AsCodedError(err)
	pass := ok && coded.Code == CodeSchema && coded.BlockNo != nil && *coded.BlockNo == 2 && contains(coded.Path, "sample_budget")
	report.check("schema_missing_field_path_block", errorFromBool(pass, err), fmt.Sprintf("path=%v block=%v", pathOf(err), blockOf(err)))

	raw, _ = json.Marshal(p)
	_ = json.Unmarshal(raw, &root)
	cs := root["alignment_constraints"].([]any)
	c0 := cs[0].(map[string]any)
	c0["sample_column_b"] = []int{0}
	bad, _ = json.Marshal(root)
	_, err = ParseProject(bad)
	coded, ok = AsCodedError(err)
	pass = ok && coded.Code == CodeSchema && contains(coded.Path, "sample_column")
	report.check("schema_boundary_column_length", errorFromBool(pass, err), pathOf(err))

	raw, _ = json.Marshal(p)
	_ = json.Unmarshal(raw, &root)
	bnds := root["shared_boundaries"].([]any)
	b0 := bnds[0].(map[string]any)
	b0["constraint_id"] = "does-not-exist"
	bad, _ = json.Marshal(root)
	_, err = ParseProject(bad)
	coded, ok = AsCodedError(err)
	pass = ok && coded.Code == CodeSchema && contains(coded.Path, "constraint_id")
	report.check("schema_missing_constraint_reference", errorFromBool(pass, err), pathOf(err))
}

func runTruthChecks(report *AcceptanceReport, p *Project, c *CompiledProject) {
	for _, tc := range p.Truth.VertexChecks {
		m := c.Meshes[tc.BlockNo]
		var found *Vertex
		for i := range m.Vertices {
			if m.Vertices[i].GridX == tc.GridX && m.Vertices[i].GridY == tc.GridY {
				found = &m.Vertices[i]
				break
			}
		}
		if found == nil {
			report.check("truth_vertices", fmt.Errorf("block %d point %d,%d missing", tc.BlockNo, tc.GridX, tc.GridY), "")
			return
		}
		if d := absFloat(found.Y - tc.ExpectedHeight); d > tc.Tolerance {
			report.check("truth_vertices", fmt.Errorf("block %d deviation %.9f > tol %.9f", tc.BlockNo, d, tc.Tolerance), "")
			return
		}
	}
	report.check("truth_vertices", nil, fmt.Sprintf("%d specified grid vertices within tolerance", len(p.Truth.VertexChecks)))

	for _, be := range p.Truth.BlockErrorBounds {
		if c.Meshes[be.BlockNo].MaxHeightError > be.DeclaredMaxHeightError {
			report.check("declared_error_bounds", fmt.Errorf("block %d %.6f > %.6f", be.BlockNo, c.Meshes[be.BlockNo].MaxHeightError, be.DeclaredMaxHeightError), "")
			return
		}
	}
	report.check("declared_error_bounds", nil, "all block max errors within recorded upper bounds")
}

func runMonotonicCheck(report *AcceptanceReport, p *Project) {
	block := p.Blocks[3]
	block.RequiredPoints = nil
	block.SampleBudget = block.Grid.Width * block.Grid.Height
	tolerances := []float64{2.0, 0.5, 0.1, 0.02}
	lastTris, lastVerts := 0, 0
	for _, tol := range tolerances {
		block.ErrorTolerance = tol
		m, err := BuildBlockMesh(block, BuildOptions{ParameterVersion: p.ParameterVersion})
		if err != nil {
			report.check("tolerance_monotonic", err, "")
			return
		}
		if len(m.Triangles) < lastTris || len(m.Vertices) < lastVerts {
			report.check("tolerance_monotonic", fmt.Errorf("tol %g produced %d tris after %d", tol, len(m.Triangles), lastTris), "")
			return
		}
		lastTris, lastVerts = len(m.Triangles), len(m.Vertices)
	}
	report.check("tolerance_monotonic", nil, fmt.Sprintf("triangle count non-decreasing to %d", lastTris))
}

func runDeterminismCheck(report *AcceptanceReport, p *Project, base *CompiledProject) {
	plan, _ := PlanAlignment(p)
	const n = 16
	meshes := make([]*Mesh, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := p.Blocks[i%len(p.Blocks)]
			m, err := BuildBlockMesh(b, BuildOptions{ParameterVersion: p.ParameterVersion, ForcedPoints: plan.Forced[b.BlockNo]})
			if err == nil {
				meshes[i] = m
			}
		}(i)
	}
	wg.Wait()
	ref := base.Meshes[0]
	for i, m := range meshes {
		if m == nil {
			report.check("worker_determinism", fmt.Errorf("worker %d failed", i), "")
			return
		}
		if m.BlockNo == 0 {
			if err := DiffMesh(0, ref, m); err != nil {
				report.check("worker_determinism", err, "")
				return
			}
		}
	}
	report.check("worker_determinism", nil, "repeated worker builds are field-identical")
}

func runFaultAttribution(report *AcceptanceReport, p *Project) {
	for _, fc := range p.Truth.FaultCases {
		rep, err := AttributeBoundary(fc)
		if fc.ExpectedCode == CodeAlign {
			var alignErr *AlignError
			if !errors.As(err, &alignErr) || rep == nil || rep.Category != fc.ExpectedCategory {
				report.check("fault_"+fc.ID, fmt.Errorf("expected E_ALIGN/%s, got err=%v rep=%+v", fc.ExpectedCategory, err, rep), "")
				return
			}
			continue
		}
		if err != nil || rep.Category != fc.ExpectedCategory {
			report.check("fault_attribution", fmt.Errorf("%s expected %s got rep=%+v err=%v", fc.ID, fc.ExpectedCategory, rep, err), "")
			return
		}
	}
	report.check("fault_attribution", nil, fmt.Sprintf("%d generated break cases classified", len(p.Truth.FaultCases)))
}

func runBudgetQueueCheck(report *AcceptanceReport) {
	meter := NewBudgetMeter(BudgetConfig{MaxParallel: 1, PerWorkerVertexBuffer: 16, MeshMemoryBudgetBytes: 1024})
	first, err := meter.Acquire(context.Background(), 0, 8, 100)
	if err != nil {
		report.check("budget_queue_reason", err, "")
		return
	}
	acquired := make(chan *Lease, 1)
	go func() {
		l, _ := meter.Acquire(context.Background(), 1, 8, 100)
		acquired <- l
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && meter.Occupancy().Queued == 0 {
		time.Sleep(time.Millisecond * 5)
	}
	occ := meter.Occupancy()
	if occ.Queued == 0 {
		report.check("budget_queue_reason", fmt.Errorf("second worker did not enter queue"), "")
		first.Release()
		return
	}
	first.Complete(20)
	select {
	case lease := <-acquired:
		lease.Complete(20)
	case <-time.After(time.Second):
		report.check("budget_queue_reason", fmt.Errorf("queued worker was not admitted after release"), "")
		return
	}
	events := meter.Events()
	var found bool
	for _, e := range events {
		if e.State == "queued" && e.Reason == "max_parallel" {
			found = true
		}
	}
	report.check("budget_queue_reason", errorFromBool(found, nil), "queue event records block and max_parallel reason")

	_, err = meter.Acquire(context.Background(), 9, 32, 100)
	coded, ok := AsCodedError(err)
	report.check("per_worker_vertex_cap", errorFromBool(ok && coded.Code == CodeBudget, err), "oversized vertex buffer rejected with E_BUDGET")
}

func runIncrementalAndRecovery(ctx context.Context, report *AcceptanceReport, workDir string, p *Project, _ *CompiledProject) {
	editedExpected := cloneProject(p)
	editedExpected.Blocks[1].SampleBudget = 48
	expected, err := CompileProject(editedExpected)
	if err != nil {
		report.check("incremental_expected_compile", err, "")
		return
	}

	incPath := filepath.Join(workDir, "draft-incremental.db")
	_ = os.Remove(incPath)
	draftSvc, err := NewService(cloneProject(p), ServiceOptions{DBPath: incPath, Budget: BudgetConfig{MaxParallel: 1, PerWorkerVertexBuffer: 4096, MeshMemoryBudgetBytes: 64 << 20}})
	if err != nil {
		report.check("draft_incremental", err, "")
		return
	}
	if _, err := draftSvc.RebuildAll(ctx); err != nil {
		report.check("draft_incremental_base", err, "")
		_ = draftSvc.Close()
		return
	}
	manifest, err := draftSvc.ApplyEdit(ctx, 1, EditRequest{SampleBudget: intPtr(48)})
	if err != nil {
		report.check("draft_incremental_edit", err, "")
		_ = draftSvc.Close()
		return
	}
	incState := draftSvc.State()
	if err := CompareMeshMaps(expected.Meshes, serviceMeshes(incState)); err != nil {
		report.check("draft_incremental_matches_full", err, "")
	} else if err := compareReportMaps(expected.Reports, incState.Reports); err != nil {
		report.check("draft_incremental_matches_full", err, "")
	} else {
		block2Hit := false
		for _, b := range manifest.Blocks {
			if b.BlockNo == 2 {
				block2Hit = b.CacheHit
			}
		}
		report.check("draft_incremental_matches_full", nil, "serial draft incremental vertices/triangles/reports equal full")
		report.check("precise_invalidation_reuse", errorFromBool(block2Hit, nil), "block 2 reused cache after block 1 edit")
	}
	_ = draftSvc.Close()

	recPath := filepath.Join(workDir, "recovery.db")
	_ = os.Remove(recPath)
	if err := seedCheckpoint(recPath, editedExpected, expected, []int{0, 1}); err != nil {
		report.check("checkpoint_recovery", err, "")
		return
	}
	recSvc, err := NewService(cloneProject(editedExpected), ServiceOptions{DBPath: recPath, Budget: BudgetConfig{MaxParallel: 2, PerWorkerVertexBuffer: 4096, MeshMemoryBudgetBytes: 64 << 20}})
	if err != nil {
		report.check("checkpoint_recovery", err, "")
		return
	}
	recovered := recSvc.RecoveryBlocks()
	if !reflect.DeepEqual(recovered, []int{0, 1}) {
		report.check("checkpoint_recovery", fmt.Errorf("recovered %v", recovered), "")
	}
	recManifest, err := recSvc.RunIncremental(ctx, nil)
	report.check("checkpoint_recovery", err, fmt.Sprintf("recovered=%v then rebuilt missing blocks", recovered))
	if err == nil {
		if err := CompareMeshMaps(expected.Meshes, serviceMeshes(recSvc.State())); err != nil {
			report.check("recovery_matches_full", err, "")
		} else {
			report.check("recovery_matches_full", compareReportMaps(expected.Reports, recSvc.State().Reports), "recovery vertices and fold stats equal")
		}
		seq := sequenceBlockNumbers(recManifest.SnapshotSequence)
		report.check("three_path_commit_sequence", expectIntSlice([]int{0, 1, 2, 3}, seq), fmt.Sprintf("recovery sequence=%v", seq))
	}
	_ = recSvc.Close()

	fullEditedPath := filepath.Join(workDir, "full-edited.db")
	_ = os.Remove(fullEditedPath)
	fullSvc, err := NewService(cloneProject(editedExpected), ServiceOptions{DBPath: fullEditedPath, Budget: BudgetConfig{MaxParallel: 4, PerWorkerVertexBuffer: 4096, MeshMemoryBudgetBytes: 64 << 20}})
	if err != nil {
		report.check("edited_full_path", err, "")
		return
	}
	fullEditedManifest, err := fullSvc.RebuildAll(ctx)
	report.check("edited_full_path", err, manifestDetail(fullEditedManifest))
	if err == nil {
		report.check("three_path_vertices", CompareMeshMaps(expected.Meshes, serviceMeshes(fullSvc.State())), "draft, recovery, and one-shot full match")
		seq := sequenceBlockNumbers(fullEditedManifest.SnapshotSequence)
		report.check("full_commit_sequence", expectIntSlice([]int{0, 1, 2, 3}, seq), fmt.Sprintf("full sequence=%v", seq))
	}
	_ = fullSvc.Close()

	serialPath := filepath.Join(workDir, "serial-edited.db")
	_ = os.Remove(serialPath)
	serialSvc, err := NewService(cloneProject(editedExpected), ServiceOptions{DBPath: serialPath, Budget: BudgetConfig{MaxParallel: 1, PerWorkerVertexBuffer: 4096, MeshMemoryBudgetBytes: 64 << 20}})
	if err != nil {
		report.check("serial_vs_parallel", err, "")
		return
	}
	serialManifest, err := serialSvc.RebuildAll(ctx)
	if err != nil {
		report.check("serial_vs_parallel", err, "")
	} else {
		err = CompareMeshMaps(expected.Meshes, serviceMeshes(serialSvc.State()))
		report.check("serial_vs_parallel", err, "MaxParallel=1 and MaxParallel=4 produce field-identical vertices/triangles")
		seq := sequenceBlockNumbers(serialManifest.SnapshotSequence)
		report.check("serial_commit_sequence", expectIntSlice([]int{0, 1, 2, 3}, seq), fmt.Sprintf("serial sequence=%v", seq))
	}
	_ = serialSvc.Close()
}

func runVersionMismatchCheck(report *AcceptanceReport, workDir string, p *Project) {
	path := filepath.Join(workDir, "version-mismatch.db")
	_ = os.Remove(path)
	store, err := OpenStore(path)
	if err != nil {
		report.check("version_mismatch_reset", err, "")
		return
	}
	ctx := context.Background()
	if err := store.EnsureSnapshot("snapshot-"+p.ProjectID, p.ProjectID, "stale-algo", "stale-param", time.Now()); err != nil {
		_ = store.Close()
		report.check("version_mismatch_reset", err, "")
		return
	}
	_ = store.Close()
	svc, err := NewService(cloneProject(p), ServiceOptions{DBPath: path})
	if err != nil {
		report.check("version_mismatch_reset", err, "")
		return
	}
	reset := svc.VersionReset()
	_, runErr := svc.RebuildAll(ctx)
	report.check("version_mismatch_reset", errorFromBool(reset && runErr == nil, runErr), "stale checkpoint discarded and full rebuild succeeded")
	_ = svc.Close()
}

func runFailureIsolationCheck(ctx context.Context, report *AcceptanceReport, workDir string, p *Project, expected *CompiledProject) {
	path := filepath.Join(workDir, "failure-isolation.db")
	_ = os.Remove(path)
	svc, err := NewService(cloneProject(p), ServiceOptions{DBPath: path, Budget: BudgetConfig{MaxParallel: 2, PerWorkerVertexBuffer: 4096, MeshMemoryBudgetBytes: 64 << 20}})
	if err != nil {
		report.check("failure_isolation", err, "")
		return
	}
	if _, err := svc.RebuildAll(ctx); err != nil {
		report.check("failure_isolation", err, "baseline should pass")
		_ = svc.Close()
		return
	}
	svc.InjectFailureOnce(2, errors.New("simulated block 2 explosion"))
	manifest, firstErr := svc.RetryBlock(ctx, 2)
	failedOnly := firstErr != nil
	if manifest != nil {
		for _, b := range manifest.Blocks {
			if b.Status == StatusFailed && b.BlockNo != 2 {
				failedOnly = false
			}
		}
	}
	report.check("failure_isolation", errorFromBool(failedOnly, firstErr), "only block 2 marked failed; committed blocks remain")

	retryManifest, retryErr := svc.RetryBlock(ctx, 2)
	if retryErr != nil {
		report.check("block_retry", retryErr, "")
	} else {
		report.check("block_retry", CompareMeshMaps(expected.Meshes, serviceMeshes(svc.State())), "retry restores exact mesh without rebuilding others")
		if retryManifest != nil {
			for _, b := range retryManifest.Blocks {
				if b.BlockNo == 2 && !b.CacheHit && b.Status != StatusClean {
					report.check("block_retry", fmt.Errorf("block 2 status %s", b.Status), "")
					_ = svc.Close()
					return
				}
			}
		}
		report.check("block_retry", nil, "transient fault consumed and retry committed")
	}
	_ = svc.Close()
}

func seedCheckpoint(path string, p *Project, c *CompiledProject, blocks []int) error {
	store, err := OpenStore(path)
	if err != nil {
		return err
	}
	defer store.Close()
	snapshotID := "snapshot-" + p.ProjectID
	if err := store.ResetSnapshot(snapshotID); err != nil {
		return err
	}
	if err := store.EnsureSnapshot(snapshotID, p.ProjectID, AlgorithmVersion, p.ParameterVersion, time.Now()); err != nil {
		return err
	}
	graph := BuildDependencyGraph(p)
	for _, no := range blocks {
		m := c.Meshes[no]
		var deps []Dependency
		for _, d := range graph.Dependencies {
			if d.BlockNo == no {
				deps = append(deps, d)
			}
		}
		var reps []*BoundaryReport
		for _, bnd := range p.SharedBoundaries {
			if (bnd.BlockA == no || bnd.BlockB == no) && c.Reports[bnd.ID] != nil {
				reps = append(reps, c.Reports[bnd.ID])
			}
		}
		hash := MeshContentHash(p.ProjectID, p.Blocks[no], m, deps)
		if err := store.CommitBlock(CommitBlockRequest{
			SnapshotID: snapshotID, ProjectID: p.ProjectID, BlockNo: no, Mesh: m, Dependencies: deps, BoundaryStats: reps,
			VertexDigest: VertexDigest(m.Vertices), TriangleDigest: TriangleDigest(m.Triangles), ContentHash: hash,
			ParameterVersion: p.ParameterVersion, AlgorithmVersion: AlgorithmVersion,
		}); err != nil {
			return err
		}
	}
	return store.CompleteSnapshot(snapshotID)
}

func serviceMeshes(state ServiceState) map[int]*Mesh {
	out := map[int]*Mesh{}
	for no, e := range state.Cache {
		if e != nil && e.Mesh != nil {
			out[no] = e.Mesh
		}
	}
	return out
}

func compareReportMaps(expected map[string]*BoundaryReport, actual map[string]*BoundaryReport) error {
	if len(expected) != len(actual) {
		return fmt.Errorf("report count %d != %d", len(expected), len(actual))
	}
	for id, e := range expected {
		a := actual[id]
		if a == nil {
			return fmt.Errorf("missing report %s", id)
		}
		if !reflect.DeepEqual(e, a) {
			return fmt.Errorf("boundary report %s differs: expected %#v actual %#v", id, e, a)
		}
	}
	return nil
}

func cloneProject(p *Project) *Project {
	data, _ := json.Marshal(p)
	var out Project
	_ = json.Unmarshal(data, &out)
	return &out
}

func sequenceBlockNumbers(records []CommitRecord) []int {
	out := make([]int, 0, len(records))
	for _, r := range records {
		out = append(out, r.Seq)
	}
	return out
}

func expectIntSlice(want, got []int) error {
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("expected %v got %v", want, got)
	}
	return nil
}

func manifestDetail(m *RunManifest) string {
	if m == nil {
		return "nil manifest"
	}
	return fmt.Sprintf("committed=%v sequence=%v counts=%v", m.CommitOrder, sequenceBlockNumbers(m.SnapshotSequence), m.Counts)
}

func errorFromBool(ok bool, err error) error {
	if ok {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("check did not pass")
}

func pathOf(err error) string {
	c, ok := AsCodedError(err)
	if ok {
		return c.Path
	}
	return ""
}

func blockOf(err error) any {
	c, ok := AsCodedError(err)
	if ok && c.BlockNo != nil {
		return *c.BlockNo
	}
	return nil
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexString(s, sub) >= 0)
}

func indexString(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func intPtr(v int) *int { return &v }
