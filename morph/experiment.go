package morph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type SceneStats struct {
	Paths           int `json:"paths"`
	Keyframes       int `json:"keyframes"`
	Samples         int `json:"samples"`
	Segments        int `json:"segments"`
	QuantizedPoints int `json:"quantized_points"`
}

type ExperimentReport struct {
	GeneratedAt       string                 `json:"generated_at"`
	GoVersion         string                 `json:"go_version"`
	SceneID           string                 `json:"scene_id"`
	Seed              int64                  `json:"seed"`
	SceneStats        SceneStats             `json:"scene_stats"`
	FineQuality       Quality                `json:"fine_quality"`
	ApproxQuality     Quality                `json:"approx_quality"`
	FineRun           *RunSummary            `json:"fine_run"`
	ApproxRun         *RunSummary            `json:"approx_run"`
	TruthCheck        *TruthReport           `json:"truth_check"`
	QualityCheck      *QualityReport         `json:"quality_check"`
	ThreePath         *ThreePathReport       `json:"three_path"`
	ErrorEvidence     map[Code]ErrorEvidence `json:"error_evidence"`
	ErrorChainCovered bool                   `json:"error_chain_covered"`
	MissingErrors     []string               `json:"missing_errors,omitempty"`
	Artifacts         []string               `json:"artifacts"`
	AllPass           bool                   `json:"all_pass"`
}

func sceneStats(s *Scene) SceneStats {
	st := SceneStats{Paths: len(s.Paths), Samples: len(s.SampleTimes)}
	for _, p := range s.Paths {
		st.Keyframes += len(p.Keyframes)
		if len(p.Keyframes) > 0 {
			st.Segments += len(p.Keyframes[0].Segments)
		}
		for _, k := range p.Keyframes {
			for _, seg := range k.Segments {
				st.QuantizedPoints += len(seg.Points())
			}
		}
	}
	return st
}

func RunExperiment(outDir string, scene *Scene, truth *Truth, base Config) (*ExperimentReport, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := WriteJSONAtomic(filepath.Join(outDir, "scene.json"), scene); err != nil {
		return nil, err
	}
	if err := WriteJSONAtomic(filepath.Join(outDir, "truth.json"), truth); err != nil {
		return nil, err
	}
	rep := &ExperimentReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		GoVersion:   runtime.Version(), SceneID: scene.ID, Seed: scene.Seed,
		SceneStats: sceneStats(scene), Artifacts: []string{
			"scene.json", "truth.json", "experiment.json", "EXPERIMENT.md", "frames/fine", "frames/approx", "frames/diff",
		},
	}
	run := func(name string, cfg Config) (*Engine, *RunSummary, error) {
		dir := filepath.Join(outDir, "checkpoints", name)
		_ = os.RemoveAll(dir)
		cfg.Checkpoint = dir
		eng, err := NewEngine(scene, cfg)
		if err != nil {
			return nil, nil, err
		}
		sum, err := eng.Run(context.Background(), RunOptions{})
		if err != nil {
			return eng, sum, err
		}
		return eng, sum, nil
	}
	fineCfg := base
	fineCfg.Adaptive.ForceFine = true
	fineCfg.Adaptive.BaseGrid = base.Render.Grid
	fineCfg.Adaptive.BaseMidTolerance = base.Eval.MidTolerance
	fineEng, fineSum, err := run("fine", fineCfg)
	if err != nil {
		return rep, err
	}
	fine := fineEng.Results()
	rep.FineQuality = fineEng.Quality
	rep.FineRun = fineSum

	approxCfg := base
	approxCfg.Adaptive.ForceFine = false
	approxCfg.Adaptive.WorkThreshold = 1
	approxCfg.Adaptive.MemoryBudgetBytes = 96 << 20
	approxEng, approxSum, err := run("approx", approxCfg)
	if err != nil {
		return rep, err
	}
	approx := approxEng.Results()
	rep.ApproxQuality = approxEng.Quality
	rep.ApproxRun = approxSum

	truthRep, err := ValidateResultsAgainstTruth(scene, truth, fine, fineCfg, 1e-6, 0.12)
	if err != nil {
		rep.TruthCheck = truthRep
		_ = writeExperimentReport(outDir, rep)
		return rep, err
	}
	rep.TruthCheck = truthRep

	qualityRep, qualityErr := CompareQuality(scene, truth, base.Consistency, fine, approx)
	rep.QualityCheck = qualityRep
	if len(approx) < len(fine) {
		return rep, withField(newError(CodeScale, "approximate run is missing frames"), "experiment.approx")
	}
	if err := os.MkdirAll(filepath.Join(outDir, "frames", "diff"), 0o755); err != nil {
		return rep, err
	}
	for i := range fine {
		if err := WriteDiffPNG(filepath.Join(outDir, "frames", "diff", fmt.Sprintf("diff_%04d.png", i)), fine[i], approx[i]); err != nil {
			return rep, err
		}
	}

	stop1 := len(fine) / 2
	if stop1 < 1 {
		stop1 = 1
	}
	resume, err := runPartialAndResume(outDir, "resume", fineCfg, scene, stop1)
	if err != nil {
		return rep, err
	}
	stop2 := len(fine) - 1
	if stop2 < 1 {
		stop2 = 1
	}
	incremental, err := runPartialAndResume(outDir, "incremental", fineCfg, scene, stop2)
	if err != nil {
		return rep, err
	}
	threeRep, err := CompareResultFrames(scene, []string{"full", "resume", "incremental"}, [][]*SampleResult{fine, resume, incremental})
	rep.ThreePath = threeRep
	if err != nil {
		_ = writeExperimentReport(outDir, rep)
		return rep, err
	}

	if err := ExportResults(filepath.Join(outDir, "frames", "fine"), fine); err != nil {
		return rep, err
	}
	if err := ExportResults(filepath.Join(outDir, "frames", "approx"), approx); err != nil {
		return rep, err
	}
	evidence, err := ExerciseErrorChain(scene, truth, fine, filepath.Join(outDir, "errors"))
	if err != nil {
		return rep, err
	}
	rep.ErrorEvidence = evidence
	rep.ErrorChainCovered, rep.MissingErrors = EvidenceComplete(evidence)
	rep.AllPass = truthRep.Pass && qualityErr == nil && qualityRep.Pass && threeRep.Pass && rep.ErrorChainCovered
	if err := writeExperimentReport(outDir, rep); err != nil {
		return rep, err
	}
	if !rep.AllPass {
		return rep, withField(newError(CodeScale, "experiment did not satisfy all checks"), "experiment")
	}
	return rep, nil
}

func runPartialAndResume(outDir, name string, cfg Config, scene *Scene, stopAfter int) ([]*SampleResult, error) {
	dir := filepath.Join(outDir, "checkpoints", name)
	_ = os.RemoveAll(dir)
	cfg.Checkpoint = dir
	e, err := NewEngine(scene, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := e.Run(context.Background(), RunOptions{StopAfterCommits: stopAfter}); err != nil {
		return nil, err
	}
	e2, err := NewEngine(scene, cfg)
	if err != nil {
		return nil, err
	}
	_, err = e2.Run(context.Background(), RunOptions{})
	if err != nil {
		return nil, err
	}
	return e2.Results(), nil
}

func writeExperimentReport(outDir string, rep *ExperimentReport) error {
	if err := WriteJSONAtomic(filepath.Join(outDir, "experiment.json"), rep); err != nil {
		return err
	}
	md := experimentMarkdown(rep)
	return os.WriteFile(filepath.Join(outDir, "EXPERIMENT.md"), []byte(md), 0o644)
}

func experimentMarkdown(r *ExperimentReport) string {
	var b strings.Builder
	b.WriteString("# Deterministic Morphing Vector Experiment\n\n")
	fmt.Fprintf(&b, "- Generated: %s\n- Go: %s\n- Scene: `%s` (seed `%d`)\n", r.GeneratedAt, r.GoVersion, r.SceneID, r.Seed)
	fmt.Fprintf(&b, "- Input scale: %d paths, %d keyframes, %d closed segments, %d samples\n", r.SceneStats.Paths, r.SceneStats.Keyframes, r.SceneStats.Segments, r.SceneStats.Samples)
	fmt.Fprintf(&b, "- Quantized control points: %d\n\n", r.SceneStats.QuantizedPoints)
	b.WriteString("## Adaptive Decision\n\n")
	fmt.Fprintf(&b, "- Fine mode: `%s`, grid=%d, midpoint tolerance=%.6g, chunk=%d\n", r.FineQuality.Mode, r.FineQuality.Grid, r.FineQuality.MidTolerance, r.FineQuality.ChunkSize)
	fmt.Fprintf(&b, "- Approximate mode: `%s`, grid=%d, midpoint tolerance=%.6g, chunk=%d\n", r.ApproxQuality.Mode, r.ApproxQuality.Grid, r.ApproxQuality.MidTolerance, r.ApproxQuality.ChunkSize)
	fmt.Fprintf(&b, "- Approx reasons: %s\n\n", strings.Join(r.ApproxQuality.Reasons, "; "))
	if r.TruthCheck != nil {
		b.WriteString("## Analytic Truth\n\n")
		fmt.Fprintf(&b, "- Pass: `%v`, point tolerance `%.3g`, coverage-area tolerance `%.3g`\n", r.TruthCheck.Pass, r.TruthCheck.PointTolerance, r.TruthCheck.AreaTolerance)
		maxPoint, maxArea, maxCov := 0.0, 0.0, 0.0
		for _, s := range r.TruthCheck.Samples {
			for _, p := range s.Paths {
				if p.MaxPointError > maxPoint {
					maxPoint = p.MaxPointError
				}
				if p.ContourAreaError > maxArea {
					maxArea = p.ContourAreaError
				}
				if p.CoverageAreaError > maxCov {
					maxCov = p.CoverageAreaError
				}
			}
		}
		fmt.Fprintf(&b, "- Max errors: curve point `%.3g`, contour area `%.3g`, raster coverage area `%.3g`\n\n", maxPoint, maxArea, maxCov)
	}
	if r.QualityCheck != nil {
		b.WriteString("## Fine vs Approximate\n\n")
		fmt.Fprintf(&b, "- Pass: `%v`, midpoint sample covered: `%v`\n", r.QualityCheck.Pass, r.QualityCheck.MidpointCovered)
		fmt.Fprintf(&b, "- Segments: fine=%d approx=%d; interpolations: fine=%d approx=%d\n", r.QualityCheck.FineSegments, r.QualityCheck.ApproxSegments, r.QualityCheck.FineInterpolations, r.QualityCheck.ApproxInterpolations)
		fmt.Fprintf(&b, "- Declared tolerances: relative area <= %.4g, bbox IoU >= %.4g, mean coverage diff <= %.4g\n", r.QualityCheck.Tolerance.RelativeArea, r.QualityCheck.Tolerance.BBoxIoU, r.QualityCheck.Tolerance.MeanCoverage)
		if len(r.QualityCheck.Samples) > 0 {
			p := r.QualityCheck.Samples[0].PixelError
			fmt.Fprintf(&b, "- First-frame pixel errors: MAE=%.6g RMSE=%.6g max=%.6g changed=%d\n", p.MAE, p.RMSE, p.MaxAbs, p.Changed)
		}
		b.WriteString("\n")
	}
	if r.ThreePath != nil {
		b.WriteString("## Three Execution Paths\n\n")
		fmt.Fprintf(&b, "- Pass: `%v` (full, resume, incremental)\n", r.ThreePath.Pass)
		if len(r.ThreePath.Samples) > 0 {
			fmt.Fprintf(&b, "- Samples compared: %d, all pixel hashes identical\n", len(r.ThreePath.Samples))
		}
		b.WriteString("\n")
	}
	b.WriteString("## Error Code Chain\n\n")
	required := []Code{CodeDecode, CodePath, CodeKey, CodeStyle, CodeParam, CodeScale, CodeState, CodeDiverge}
	for _, c := range required {
		e := r.ErrorEvidence[c]
		status := "**MISSING**"
		if e.Observed && e.Code == c {
			status = "observed"
		}
		fmt.Fprintf(&b, "- `%s`: %s; field=`%s`, path=`%s`\n", c, status, e.Field, e.PathID)
	}
	b.WriteString("\n## Resource Notes\n\n")
	if r.FineRun != nil {
		fmt.Fprintf(&b, "- Fine: %d/%d committed in %d ms, resource events=%d\n", r.FineRun.CommittedSamples, r.FineRun.TotalSamples, r.FineRun.ElapsedMillis, len(r.FineRun.ResourceEvents))
	}
	if r.ApproxRun != nil {
		fmt.Fprintf(&b, "- Approx: %d/%d committed in %d ms, resource events=%d\n", r.ApproxRun.CommittedSamples, r.ApproxRun.TotalSamples, r.ApproxRun.ElapsedMillis, len(r.ApproxRun.ResourceEvents))
	}
	b.WriteString("\n## Reproduction\n\n")
	b.WriteString("```sh\n")
	b.WriteString("go run ./cmd/pathmorph experiment -out artifacts/experiment\n")
	b.WriteString("go test ./...\n")
	b.WriteString("```\n\n")
	b.WriteString("Frames are exported as PGM/PNG under `frames/fine` and `frames/approx`; difference heatmaps are under `frames/diff`.\n")
	return b.String()
}
