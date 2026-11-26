package strokelab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type EvalSummary struct {
	Mode             RunMode          `json:"mode"`
	CompositeHash    string           `json:"composite_hash"`
	ReferenceHash    string           `json:"reference_hash"`
	TopologyPassed   bool             `json:"topology_passed"`
	DifferencePixels int              `json:"difference_pixels"`
	AttributionCount int              `json:"attribution_differences"`
	Culprits         []CulpritSummary `json:"culprits,omitempty"`
	ReusedBlocks     int              `json:"reused_blocks"`
	RecomputedBlocks int              `json:"recomputed_blocks"`
	CommittedStrokes int              `json:"committed_strokes"`
}

type VerificationReport struct {
	Format              string            `json:"format"`
	GeneratedAt         string            `json:"generated_at"`
	Seed                int64             `json:"seed"`
	SelectedSample      string            `json:"selected_sample"`
	Suite               *SuiteManifest    `json:"suite"`
	Full                EvalSummary       `json:"full"`
	Incremental         EvalSummary       `json:"incremental"`
	Resume              EvalSummary       `json:"resume"`
	IncrementalCompare  *PathComparison   `json:"incremental_vs_full"`
	ResumeCompare       *PathComparison   `json:"resume_vs_full"`
	InterruptedCommits  int               `json:"interrupted_commits"`
	CheckpointAfterStop CheckpointStatus  `json:"checkpoint_after_stop"`
	CheckpointResumed   CheckpointStatus  `json:"checkpoint_resumed"`
	Edit                Edit              `json:"edit"`
	Invalidation        *InvalidationPlan `json:"invalidation"`
	ExpectedAffected    []int             `json:"expected_affected_blocks"`
	RecomputedAfterEdit []int             `json:"recomputed_after_edit_blocks"`
	EditedReuseCompare  *PathComparison   `json:"edited_reuse_vs_clean_full"`
	EditedTopologyErr   string            `json:"edited_topology_error,omitempty"`
	AllPassed           bool              `json:"all_passed"`
	Artifacts           map[string]string `json:"artifacts"`
}

func RunAcceptance(seed int64, outDir string) (*VerificationReport, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	suite, err := GenerateSuite(seed)
	if err != nil {
		return nil, err
	}
	suiteDir := filepath.Join(outDir, "samples")
	suiteManifest, err := WriteSuite(suiteDir, suite)
	if err != nil {
		return nil, err
	}
	sample := suite.Samples[2]
	for _, s := range suite.Samples {
		if s.Name == "v3-grouped-128" {
			sample = s
			break
		}
	}
	sample, err = LoadSampleDir(filepath.Join(suiteDir, sample.Name))
	if err != nil {
		return nil, fmt.Errorf("load generated sample from disk: %w", err)
	}
	report := &VerificationReport{
		Format:      "stroke-verification/v1",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Seed:        seed, SelectedSample: sample.Name, Suite: suiteManifest,
		Artifacts: make(map[string]string),
	}
	fullEngine, err := NewEngine(sample.Project)
	if err != nil {
		return nil, err
	}
	full, err := fullEngine.Run(context.Background(), RunOptions{Mode: RunFull})
	if err != nil {
		return nil, fmt.Errorf("full run: %w", err)
	}
	report.Full = summarizeEval(full)
	report.Full.ReferenceHash = HashHex(sample.Project.Truth.ReferenceMask)
	cachePath := filepath.Join(outDir, "cache", "blocks.json")
	if full.Cache == nil {
		return nil, coded(CodeCheckpoint, "full run produced no block cache")
	}
	if err := full.Cache.Save(cachePath); err != nil {
		return nil, err
	}
	persistedCache, err := LoadBlockCache(cachePath)
	if err != nil {
		return nil, err
	}

	incPath := filepath.Join(outDir, "checkpoints", "incremental.ckpt")
	if err := removeIfExists(incPath); err != nil {
		return nil, err
	}
	incCP, err := OpenCheckpoint(incPath)
	if err != nil {
		return nil, err
	}
	incEngine, _ := NewEngine(sample.Project)
	incremental, err := incEngine.Run(context.Background(), RunOptions{
		Mode: RunIncremental, RunID: "verify-incremental", Checkpoint: incCP,
	})
	if err != nil {
		_ = incCP.Close()
		return nil, fmt.Errorf("incremental run: %w", err)
	}
	if err := incCP.Close(); err != nil {
		return nil, err
	}
	report.Incremental = summarizeEval(incremental)
	report.Incremental.ReferenceHash = HashHex(sample.Project.Truth.ReferenceMask)
	report.IncrementalCompare = CompareEvaluations(incremental, full)

	resumePath := filepath.Join(outDir, "checkpoints", "resume.ckpt")
	if err := removeIfExists(resumePath); err != nil {
		return nil, err
	}
	cp, err := OpenCheckpoint(resumePath)
	if err != nil {
		return nil, err
	}
	stopEngine, _ := NewEngine(sample.Project)
	partial, stopErr := stopEngine.Run(context.Background(), RunOptions{
		Mode: RunIncremental, RunID: "verify-resume", Checkpoint: cp,
		StopAfterCommits: 3,
	})
	if !errors.Is(stopErr, ErrSimulatedAbort) {
		_ = cp.Close()
		return nil, fmt.Errorf("expected simulated interruption, got %v", stopErr)
	}
	report.InterruptedCommits = partial.CommittedStrokes
	if err := cp.Close(); err != nil {
		return nil, err
	}
	reopened, err := OpenCheckpoint(resumePath)
	if err != nil {
		return nil, err
	}
	report.CheckpointAfterStop = reopened.Status()
	resumeEngine, _ := NewEngine(sample.Project)
	resumed, err := resumeEngine.Run(context.Background(), RunOptions{
		Mode: RunResume, RunID: "verify-resume", Checkpoint: reopened,
	})
	if err != nil {
		_ = reopened.Close()
		return nil, fmt.Errorf("resume run: %w", err)
	}
	report.Resume = summarizeEval(resumed)
	report.Resume.ReferenceHash = HashHex(sample.Project.Truth.ReferenceMask)
	report.CheckpointResumed = reopened.Status()
	report.CheckpointResumed.Complete = report.CheckpointResumed.CommittedStrokes == len(sample.Project.Strokes)
	if err := reopened.Close(); err != nil {
		return nil, err
	}
	report.ResumeCompare = CompareEvaluations(resumed, full)

	newWeight := sample.Project.Strokes[0].Weight - 600
	edit := Edit{StrokeID: sample.Project.Strokes[0].ID, Weight: &newWeight}
	edited, err := ApplyEdit(sample.Project, edit)
	if err != nil {
		return nil, err
	}
	plan, err := PlanInvalidation(sample.Project, edited, edit)
	if err != nil {
		return nil, err
	}
	report.Edit = edit
	report.Invalidation = plan
	report.ExpectedAffected = append([]int(nil), plan.Affected...)
	editedEngine, _ := NewEngine(edited)
	reuseEval, reuseErr := editedEngine.Run(context.Background(), RunOptions{
		Mode: RunFull, ReuseCache: persistedCache,
	})
	if reuseErr != nil {
		if ErrorCode(reuseErr) != CodeTopology {
			return nil, fmt.Errorf("edited cache-reuse run: %w", reuseErr)
		}
		report.EditedTopologyErr = reuseErr.Error()
	}
	report.RecomputedAfterEdit = append([]int(nil), reuseEval.RecomputedBlocks...)
	cleanEngine, _ := NewEngine(edited)
	cleanEval, cleanErr := cleanEngine.Run(context.Background(), RunOptions{Mode: RunFull})
	if cleanErr != nil {
		if ErrorCode(cleanErr) != CodeTopology {
			return nil, fmt.Errorf("edited clean run: %w", cleanErr)
		}
	}
	report.EditedReuseCompare = CompareEvaluations(reuseEval, cleanEval)
	if _, _, err := WriteEvaluationDiagnostics(filepath.Join(outDir, "reports", "full"),
		sample, full, nil, report.IncrementalCompare); err != nil {
		return nil, err
	}
	resumeReportPath, _, err := WriteEvaluationDiagnostics(filepath.Join(outDir, "reports", "resume"),
		sample, resumed, nil, report.ResumeCompare)
	if err != nil {
		return nil, err
	}
	editedSample := &Sample{
		Name: sample.Name + "-edited", Version: sample.Version,
		Project: edited, CandidateMask: edited.CandidateMask,
	}
	editedReportPath, _, err := WriteEvaluationDiagnostics(filepath.Join(outDir, "reports", "edited"),
		editedSample, reuseEval, plan, report.EditedReuseCompare)
	if err != nil {
		return nil, err
	}
	report.Artifacts["suite"] = suiteDir
	report.Artifacts["block_cache"] = cachePath
	report.Artifacts["full_report"] = filepath.Join(outDir, "reports", "full", "report.html")
	report.Artifacts["resume_report"] = resumeReportPath
	report.Artifacts["edited_report"] = editedReportPath
	report.Artifacts["verification_json"] = filepath.Join(outDir, "verification.json")
	report.AllPassed = verifyAcceptance(report)
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(report.Artifacts["verification_json"], append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	if err := writeVerificationMarkdown(filepath.Join(outDir, "verification.md"), report); err != nil {
		return nil, err
	}
	if !report.AllPassed {
		return report, coded(CodeCheckpoint, "acceptance verification did not pass")
	}
	return report, nil
}

func summarizeEval(eval *Evaluation) EvalSummary {
	s := EvalSummary{Mode: eval.Mode}
	s.CompositeHash = HashHex(eval.Composite)
	if eval.Topology != nil {
		s.TopologyPassed = eval.Topology.Passed
		s.DifferencePixels = eval.Topology.DifferencePixels
	}
	if eval.Attribution != nil {
		s.AttributionCount = len(eval.Attribution.Differences)
		s.Culprits = append([]CulpritSummary(nil), eval.Attribution.Culprits...)
	}
	s.ReusedBlocks = len(eval.ReusedBlocks)
	s.RecomputedBlocks = len(eval.RecomputedBlocks)
	s.CommittedStrokes = eval.CommittedStrokes
	return s
}

func verifyAcceptance(r *VerificationReport) bool {
	if !r.Full.TopologyPassed || !r.Incremental.TopologyPassed || !r.Resume.TopologyPassed {
		return false
	}
	if r.IncrementalCompare == nil || !r.IncrementalCompare.Equal {
		return false
	}
	if r.ResumeCompare == nil || !r.ResumeCompare.Equal {
		return false
	}
	if r.InterruptedCommits != 3 || r.CheckpointAfterStop.Records != 3 {
		return false
	}
	if !r.CheckpointResumed.Complete {
		return false
	}
	if r.Invalidation == nil || len(r.ExpectedAffected) == 0 {
		return false
	}
	if !sameIntSet(r.ExpectedAffected, r.RecomputedAfterEdit) {
		return false
	}
	if r.EditedReuseCompare == nil || !r.EditedReuseCompare.Equal {
		return false
	}
	return true
}

func sameIntSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[int]int, len(a))
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		m[v]--
		if m[v] < 0 {
			return false
		}
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

func writeVerificationMarkdown(path string, r *VerificationReport) error {
	status := "PASS"
	if !r.AllPassed {
		status = "FAIL"
	}
	text := fmt.Sprintf(`# StrokeLab acceptance

- Status: **%s**
- Seed: %d
- Selected sample: %s
- Full composite: %s
- Incremental composite: %s
- Resume composite: %s
- Incremental == full: %v
- Resume == full: %v
- Interrupted at: %d committed strokes
- Checkpoint after interruption: %d records, tail ignored=%v
- Checkpoint resumed: %d committed strokes
- Edited stroke: %s
- Affected blocks: %d
- Recomputed after edit: %d
- Cache-reuse == clean full: %v
- Resume report: %s
- Edited report: %s
`,
		status, r.Seed, r.SelectedSample,
		r.Full.CompositeHash, r.Incremental.CompositeHash, r.Resume.CompositeHash,
		r.IncrementalCompare != nil && r.IncrementalCompare.Equal,
		r.ResumeCompare != nil && r.ResumeCompare.Equal,
		r.InterruptedCommits, r.CheckpointAfterStop.Records, r.CheckpointAfterStop.TailIgnored,
		r.CheckpointResumed.CommittedStrokes, r.Edit.StrokeID,
		len(r.ExpectedAffected), len(r.RecomputedAfterEdit),
		r.EditedReuseCompare != nil && r.EditedReuseCompare.Equal,
		r.Artifacts["resume_report"], r.Artifacts["edited_report"])
	return os.WriteFile(path, []byte(text), 0o644)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
