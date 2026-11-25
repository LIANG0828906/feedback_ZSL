package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"terrainpipeline/internal/artifact"
	"terrainpipeline/internal/bake"
	"terrainpipeline/internal/budget"
	"terrainpipeline/internal/checkpoint"
	"terrainpipeline/internal/compare"
	"terrainpipeline/internal/depgraph"
	"terrainpipeline/internal/model"
	"terrainpipeline/internal/pipeline"
	"terrainpipeline/internal/projectio"
	"terrainpipeline/internal/samplegen"
	"terrainpipeline/internal/seams"
)

type Check struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Code     string `json:"code,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
}

type Report struct {
	WorkDir         string              `json:"work_dir"`
	Passed          bool                `json:"passed"`
	Checks          []Check             `json:"checks"`
	TruthChecks     []Check             `json:"truth_checks"`
	Invalidation    map[string]any      `json:"invalidation,omitempty"`
	SeamAttribution map[string]int      `json:"seam_attribution,omitempty"`
	Budget          map[string]any      `json:"budget,omitempty"`
	CommitSequences map[string][]string `json:"commit_sequences,omitempty"`
	ArtifactFiles   map[string]int      `json:"artifact_files,omitempty"`
	ExceptionMatrix []Check             `json:"exception_matrix,omitempty"`
}

func RunAll(work string) (*Report, error) {
	if err := os.RemoveAll(work); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	rep := &Report{WorkDir: work, Passed: true, Invalidation: map[string]any{}, Budget: map[string]any{}, CommitSequences: map[string][]string{}, ArtifactFiles: map[string]int{}}
	ctx := context.Background()

	sampleDir := filepath.Join(work, "sample")
	project := samplegen.Generate(model.FixedSeed)
	if err := projectio.WriteSample(sampleDir, project); err != nil {
		return nil, err
	}
	loaded, err := projectio.LoadProject(sampleDir)
	if err != nil {
		rep.addFail("sample-load", "", "合法样本加载失败: %v", err)
		return rep, nil
	}
	rep.addPass("sample-generate-load", "", "固定种子样本生成并通过结构校验")

	liveDB, err := checkpoint.Open(filepath.Join(work, "live.db"))
	if err != nil {
		return nil, err
	}
	defer liveDB.Close()
	liveSvc := pipeline.NewService(filepath.Join(work, "live-cache"), liveDB)
	parallelBudget := budget.Options{MaxWorkers: 3, PixelBufferPixels: 16, NormalMapMemoryBytes: 4096}
	base, err := liveSvc.Run(ctx, loaded, pipeline.RunOptions{Budget: parallelBudget})
	if err != nil {
		return nil, err
	}
	if len(base.FailedBlocks) != 0 {
		rep.addFail("base-bake", "", "基础烘焙存在失败块: %v", base.FailedBlocks)
	} else {
		rep.addPass("base-bake", "", "7 块全部烘焙并提交")
	}
	checkTruth(rep, loaded, base)
	recordSeamAttribution(rep, base)

	applyElevationEdit(loaded)
	elev, err := liveSvc.Run(ctx, loaded, pipeline.RunOptions{Edits: []depgraph.Edit{{Type: "elevation", BlockID: "B01"}}, Budget: parallelBudget})
	if err != nil {
		return nil, err
	}
	checkStringSet(rep, "invalidation-elevation", elev.Plan.Invalidated, []string{"B01", "B02"})
	checkStringSet(rep, "reuse-after-elevation", elev.Plan.Reused, []string{"B03", "B04", "B05", "B06", "B07"})
	rep.Invalidation["elevation_B01"] = elev.Plan

	setRockStrength(loaded, 0.86)
	mat, err := liveSvc.Run(ctx, loaded, pipeline.RunOptions{Edits: []depgraph.Edit{{Type: "material", LayerID: "rock"}}, Budget: parallelBudget})
	if err != nil {
		return nil, err
	}
	checkStringSet(rep, "invalidation-material-rock", mat.Plan.Invalidated, []string{"B01", "B02", "B03", "B04", "B05"})
	checkStringSet(rep, "reuse-material-rock", mat.Plan.Reused, []string{"B06", "B07"})
	rep.Invalidation["material_rock"] = mat.Plan

	setForwardOperator(loaded)
	op, err := liveSvc.Run(ctx, loaded, pipeline.RunOptions{Edits: []depgraph.Edit{{Type: "operator"}}, Budget: parallelBudget})
	if err != nil {
		return nil, err
	}
	checkStringSet(rep, "invalidation-operator-global", op.Plan.Invalidated, loaded.SortedBlockIDs())
	checkSeamErrorContext(rep, op)

	recSeq, recOut, err := runRecovery(ctx, work)
	if err != nil {
		return nil, err
	}
	fullP := finalProject()
	fullStore, err := checkpoint.Open(filepath.Join(work, "full.db"))
	if err != nil {
		return nil, err
	}
	defer fullStore.Close()
	fullSvc := pipeline.NewService(filepath.Join(work, "full-cache"), fullStore)
	fullOut, err := fullSvc.Run(ctx, fullP, pipeline.RunOptions{Budget: parallelBudget})
	if err != nil {
		return nil, err
	}

	serialP := finalProject()
	serialStore, err := checkpoint.Open(filepath.Join(work, "serial.db"))
	if err != nil {
		return nil, err
	}
	defer serialStore.Close()
	serialSvc := pipeline.NewService(filepath.Join(work, "serial-cache"), serialStore)
	serialBudget := budget.Options{MaxWorkers: 1, PixelBufferPixels: 8, NormalMapMemoryBytes: 4096}
	serialOut, err := serialSvc.Run(ctx, serialP, pipeline.RunOptions{Budget: serialBudget})
	if err != nil {
		return nil, err
	}

	checkDiff(rep, "three-path-live-vs-full", compare.BakeOutputs(fullOut.Results, op.Results, fullOut.Seams, op.Seams))
	checkDiff(rep, "three-path-recovery-vs-full", compare.BakeOutputs(fullOut.Results, recOut.Results, fullOut.Seams, recOut.Seams))
	checkDiff(rep, "parallel-vs-serial", compare.BakeOutputs(fullOut.Results, serialOut.Results, fullOut.Seams, serialOut.Seams))

	liveSeq, err := liveDB.CommitSequence(op.Generation)
	if err != nil {
		return nil, err
	}
	fullSeq, err := fullStore.CommitSequence(fullOut.Generation)
	if err != nil {
		return nil, err
	}
	rep.CommitSequences["live_final"] = commitKeys(liveSeq)
	rep.CommitSequences["recovery_final"] = commitKeys(recSeq)
	rep.CommitSequences["full_final"] = commitKeys(fullSeq)
	checkStringSet(rep, "commit-sequence-live-recovery-full", rep.CommitSequences["recovery_final"], rep.CommitSequences["live_final"])
	checkStringSet(rep, "commit-sequence-full-matches", rep.CommitSequences["full_final"], rep.CommitSequences["live_final"])

	exportDirs := map[string]map[string]*model.BlockResult{
		"art-live": op.Results, "art-recovery": recOut.Results,
		"art-full": fullOut.Results, "art-serial": serialOut.Results,
	}
	hashes := map[string]map[string]string{}
	for dirName, blocks := range exportDirs {
		dir := filepath.Join(work, dirName)
		for _, result := range blocks {
			if err := artifact.ExportBlockArtifacts(dir, result); err != nil {
				return nil, err
			}
		}
		var seamReports []*model.SeamReport
		switch dirName {
		case "art-live":
			seamReports = op.Seams
		case "art-recovery":
			seamReports = recOut.Seams
		default:
			seamReports = fullOut.Seams
		}
		if dirName == "art-serial" {
			seamReports = serialOut.Seams
		}
		if err := artifact.ExportSeams(dir, seamReports); err != nil {
			return nil, err
		}
		hash, err := compare.DirectoryHashes(dir)
		if err != nil {
			return nil, err
		}
		hashes[dirName] = hash
		rep.ArtifactFiles[dirName] = len(hash)
	}
	firstMismatch := ""
	for name, hash := range hashes {
		if mismatch, ok := compare.EqualHashMaps(hashes["art-full"], hash); !ok {
			firstMismatch = name + ":" + mismatch
			break
		}
	}
	rep.addCheck(Check{Name: "deterministic-artifact-bytes", Passed: firstMismatch == "", Code: string(model.CodeDiff), Detail: orDefault(firstMismatch, "normal/slope/data/seam 文件哈希全部一致")})

	checkBudget(rep, op, serialOut)
	checkFailureRetry(ctx, rep, work, base.Results)
	runExceptionMatrix(rep, work, sampleDir, fullOut)

	for _, check := range rep.Checks {
		if !check.Passed {
			rep.Passed = false
		}
	}
	for _, check := range rep.TruthChecks {
		if !check.Passed {
			rep.Passed = false
		}
	}
	for _, check := range rep.ExceptionMatrix {
		if !check.Passed {
			rep.Passed = false
		}
	}
	return rep, nil
}

func (r *Report) addCheck(c Check) {
	r.Checks = append(r.Checks, c)
	if !c.Passed {
		r.Passed = false
	}
}

func (r *Report) addPass(name, code, detail string, args ...any) {
	r.addCheck(Check{Name: name, Passed: true, Code: code, Detail: fmt.Sprintf(detail, args...)})
}

func (r *Report) addFail(name, code string, detail string, args ...any) {
	r.addCheck(Check{Name: name, Passed: false, Code: code, Detail: fmt.Sprintf(detail, args...)})
}

func checkTruth(rep *Report, p *model.Project, out *pipeline.Outcome) {
	for _, truth := range p.Truth.Normals {
		result := out.Results[truth.BlockID]
		if result == nil {
			rep.TruthChecks = append(rep.TruthChecks, Check{Name: "normal-truth-" + truth.BlockID, Passed: false, Code: "E_REF", Detail: "结果缺失"})
			continue
		}
		pixel := result.Pixels[truth.J*result.NX+truth.I]
		angle := angleBetween(pixel.Normal, truth.Normal)
		slopeDelta := abs(pixel.SlopeDeg - truth.SlopeDeg)
		passed := angle <= truth.Tolerance && slopeDelta <= truth.Tolerance
		rep.TruthChecks = append(rep.TruthChecks, Check{
			Name: fmt.Sprintf("normal-truth-%s-%d-%d", truth.BlockID, truth.I, truth.J), Passed: passed,
			Expected: map[string]any{"normal": truth.Normal, "slope": truth.SlopeDeg},
			Actual:   map[string]any{"normal": pixel.Normal, "slope": pixel.SlopeDeg, "angle_error_deg": angle, "slope_error_deg": slopeDelta},
			Detail:   fmt.Sprintf("angle_error=%.6f° slope_error=%.6f° tolerance=%.3f", angle, slopeDelta, truth.Tolerance),
		})
	}
	for _, truth := range p.Truth.Seams {
		report := seams.FindReport(out.Seams, truth.SeamID)
		if report == nil {
			rep.TruthChecks = append(rep.TruthChecks, Check{Name: "seam-truth-" + truth.SeamID, Passed: false, Code: "E_REF"})
			continue
		}
		var actual float64
		detail := ""
		if truth.Metric == "welded" {
			actual = report.Points[truth.Index].WeldedAngleDeg
			detail = fmt.Sprintf("welded angle=%.6f", actual)
		} else {
			actual = report.Points[truth.Index].GeometryAngleDeg
			detail = fmt.Sprintf("geometry side angle=%.6f raw=%.6f corrected=%.6f", actual, report.Points[truth.Index].RawAngleDeg, report.Points[truth.Index].Basis["corrected_geometry_angle_deg"])
		}
		passed := abs(actual-truth.AngleDeg) <= truth.Tolerance
		rep.TruthChecks = append(rep.TruthChecks, Check{
			Name: "seam-truth-" + truth.SeamID, Passed: passed,
			Expected: truth.AngleDeg, Actual: actual, Detail: detail + fmt.Sprintf(" tolerance=%.3f", truth.Tolerance),
		})
	}
}

func recordSeamAttribution(rep *Report, out *pipeline.Outcome) {
	counts := map[string]int{}
	for _, seam := range out.Seams {
		for category, count := range seam.AttributionCounts {
			counts[string(category)] += count
		}
	}
	rep.SeamAttribution = counts
	hasGeometric := counts[string(model.AttributionGeometric)] > 0
	hasMaterial := counts[string(model.AttributionMaterial)] > 0
	hasDifferential := counts[string(model.AttributionDifferential)] > 0
	rep.addCheck(Check{Name: "seam-geometric-attribution", Passed: hasGeometric, Code: string(model.AttributionGeometric), Detail: "至少识别一个真实几何折角"})
	rep.addCheck(Check{Name: "seam-material-attribution", Passed: hasMaterial, Code: string(model.AttributionMaterial), Detail: "至少识别一个权重混合伪突变"})
	rep.addCheck(Check{Name: "seam-differential-attribution", Passed: hasDifferential, Code: string(model.AttributionDifferential), Detail: "至少识别一个差分算子伪突变"})
}

func applyFinalEdits(p *model.Project) {
	applyElevationEdit(p)
	setRockStrength(p, 0.86)
	setForwardOperator(p)
}

func applyElevationEdit(p *model.Project) {
	b := p.BlockByID("B01")
	i, j := b.NX-1, 2
	b.Elevation[b.Index(i, j)] += 0.22
}

func setRockStrength(p *model.Project, strength float64) {
	for i := range p.Layers {
		if p.Layers[i].ID == "rock" {
			p.Layers[i].Strength = strength
		}
	}
}

func setForwardOperator(p *model.Project) {
	p.Params.Differential = "forward"
	p.ParamVersion = p.Params.ParameterVersion()
	p.ManifestHash = model.ProjectContentHash(p)
}

func finalProject() *model.Project {
	p := samplegen.Generate(model.FixedSeed)
	applyFinalEdits(p)
	return p
}

func runRecovery(ctx context.Context, work string) ([]checkpoint.CommitEntry, *pipeline.Outcome, error) {
	dir := filepath.Join(work, "recovery-cache")
	dbPath := filepath.Join(work, "recovery.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, nil, err
	}
	svc := pipeline.NewService(dir, store)
	p := samplegen.Generate(model.FixedSeed)
	budgetOpts := budget.Options{MaxWorkers: 3, PixelBufferPixels: 16, NormalMapMemoryBytes: 4096}
	if _, err := svc.Run(ctx, p, pipeline.RunOptions{Budget: budgetOpts}); err != nil {
		return nil, nil, err
	}
	edgeEdit := depgraph.Edit{Type: "elevation", BlockID: "B01"}
	b := p.BlockByID("B01")
	b.Elevation[b.Index(b.NX-1, 2)] += 0.22
	if _, err := svc.Run(ctx, p, pipeline.RunOptions{Edits: []depgraph.Edit{edgeEdit}, Budget: budgetOpts}); err != nil {
		return nil, nil, err
	}
	for i := range p.Layers {
		if p.Layers[i].ID == "rock" {
			p.Layers[i].Strength = 0.86
		}
	}
	if _, err := svc.Run(ctx, p, pipeline.RunOptions{Edits: []depgraph.Edit{{Type: "material", LayerID: "rock"}}, Budget: budgetOpts}); err != nil {
		return nil, nil, err
	}
	p.Params.Differential = "forward"
	p.ParamVersion = p.Params.ParameterVersion()
	crashOut, err := svc.Run(ctx, p, pipeline.RunOptions{Edits: []depgraph.Edit{{Type: "operator"}}, Budget: budgetOpts, CrashAfterCommits: 2})
	if !errors.Is(err, pipeline.ErrCrash) {
		return nil, nil, fmt.Errorf("expected injected crash, got %v", err)
	}
	if len(crashOut.Commit) != 2 {
		return nil, nil, fmt.Errorf("crash should leave exactly 2 durable commits, got %d", len(crashOut.Commit))
	}
	if err := store.Close(); err != nil {
		return nil, nil, err
	}

	store2, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer store2.Close()
	svc2 := pipeline.NewService(dir, store2)
	out, err := svc2.Run(ctx, p, pipeline.RunOptions{Edits: []depgraph.Edit{{Type: "operator"}}, Budget: budgetOpts, Resume: true})
	if err != nil {
		return nil, nil, err
	}
	seq, err := store2.CommitSequence(out.Generation)
	return seq, out, err
}

func checkBudget(rep *Report, out *pipeline.Outcome, serial *pipeline.Outcome) {
	reasons := map[string]int{}
	for _, event := range out.QueueEvents {
		reasons[event.Reason]++
	}
	serialReasons := map[string]int{}
	for _, event := range serial.QueueEvents {
		serialReasons[event.Reason]++
	}
	rep.Budget["parallel_queue_events"] = reasons
	rep.Budget["serial_queue_events"] = serialReasons
	rep.Budget["parallel_event_count"] = len(out.QueueEvents)
	rep.Budget["serial_event_count"] = len(serial.QueueEvents)
	rep.addCheck(Check{Name: "budget-queue-recorded", Passed: reasons["normal_map_memory"] > 0 || reasons["max_workers"] > 0, Code: "E_BUDGET", Detail: "预算耗尽时记录排队原因且不丢块"})
	rep.addCheck(Check{Name: "budget-all-blocks-committed", Passed: len(out.Results) == 7 && len(out.Commit) == 7, Detail: fmt.Sprintf("results=%d commits=%d", len(out.Results), len(out.Commit))})
}

func checkSeamErrorContext(rep *Report, out *pipeline.Outcome) {
	if len(out.SeamErrors) == 0 {
		rep.addCheck(Check{Name: "E_SEAM-pair-context", Passed: false, Code: string(model.CodeSeam), Detail: "编辑后的样本未产生预期不可判定接缝点"})
		return
	}
	e := out.SeamErrors[0]
	requiredKeys := []string{"seam_id", "pixel_a", "pixel_b", "raw_angle_deg", "normal_a", "normal_b"}
	missing := []string{}
	for _, key := range requiredKeys {
		if e.Details == nil || e.Details[key] == nil {
			missing = append(missing, key)
		}
	}
	passed := e.Code == model.CodeSeam && len(missing) == 0 && e.BlockID != ""
	rep.addCheck(Check{
		Name: "E_SEAM-pair-context", Passed: passed, Code: string(model.CodeSeam),
		Detail: fmt.Sprintf("block_pair=%s field=%s missing=%v raw=%v", e.BlockID, e.Field, missing, e.Details["raw_angle_deg"]),
		Actual: e.Details,
	})
}

func checkFailureRetry(ctx context.Context, rep *Report, work string, healthy map[string]*model.BlockResult) {
	project := samplegen.Generate(model.FixedSeed)
	dbPath := filepath.Join(work, "failure.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		rep.addFail("block-failure-isolation", "", "打开失败检查点失败: %v", err)
		return
	}
	defer store.Close()
	svc := pipeline.NewService(filepath.Join(work, "failure-cache"), store)
	first, err := svc.Run(ctx, project, pipeline.RunOptions{
		Budget:     budget.Options{MaxWorkers: 2, PixelBufferPixels: 16, NormalMapMemoryBytes: 8192},
		FailBlocks: map[string]string{"B07": "injected deterministic block bake failure"},
	})
	if err != nil {
		rep.addFail("block-failure-isolation", "", "单块注入异常不应中断调度: %v", err)
		return
	}
	_, failed := first.FailedBlocks["B07"]
	firstPassed := failed && first.Results["B07"] == nil && len(first.Results) == 6 && len(first.Commit) == 6
	rep.addCheck(Check{Name: "block-failure-isolated", Passed: firstPassed, Code: "E_IO", Detail: fmt.Sprintf("failed=%v results=%d commits=%d", first.FailedBlocks, len(first.Results), len(first.Commit))})

	retry, err := svc.Run(ctx, project, pipeline.RunOptions{Budget: budget.Options{MaxWorkers: 1}})
	if err != nil {
		rep.addFail("block-retry", "", "逐块重试失败: %v", err)
		return
	}
	diffErr := compare.BakeOutputs(healthy, retry.Results, nil, nil)
	retryPassed := len(retry.FailedBlocks) == 0 && len(retry.Results) == 7 && diffErr == nil
	detail := "重试块成功且其他块复用已提交快照"
	if diffErr != nil {
		detail = diffErr.Error()
	}
	rep.addCheck(Check{Name: "block-retry-without-pollution", Passed: retryPassed, Code: string(model.CodeDiff), Detail: detail})
}

func checkDiff(rep *Report, name string, diffErr *model.Error) {
	if diffErr == nil {
		rep.addPass(name, string(model.CodeDiff), "逐像素/逐接缝一致")
		return
	}
	rep.addFail(name, string(model.CodeDiff), "%s", diffErr.Error())
}

func checkStringSet(rep *Report, name string, actual, expected []string) {
	if equalStrings(actual, expected) {
		rep.addPass(name, "", "集合精确匹配: %v", expected)
	} else {
		rep.addCheck(Check{Name: name, Passed: false, Expected: expected, Actual: actual, Detail: "集合不匹配"})
	}
}

func commitKeys(entries []checkpoint.CommitEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, fmt.Sprintf("%s@%s=%s", e.BlockID, e.ContentHash[:10], e.ResultHash[:10]))
	}
	return out
}

func angleBetween(a, b model.Vec3) float64 {
	return bake.AngleDeg(a, b)
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
