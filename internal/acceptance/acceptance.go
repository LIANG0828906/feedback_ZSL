package acceptance

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"volumekernel/internal/checkpoint"
	"volumekernel/internal/generator"
	"volumekernel/internal/imageout"
	"volumekernel/internal/kernel"
	"volumekernel/internal/model"
	"volumekernel/internal/pipeline"
	"volumekernel/internal/scene"
)

type Options struct {
	Seed        int64
	CameraIndex int
	OutDir      string
	WorkDir     string
}

type PathResult struct {
	Pixels           int     `json:"pixels"`
	FrameHash        string  `json:"frame_hash"`
	ElapsedMS        float64 `json:"elapsed_ms"`
	CacheHits        int     `json:"cache_hits,omitempty"`
	CacheMisses      int     `json:"cache_misses,omitempty"`
	CheckpointReused int     `json:"checkpoint_reused,omitempty"`
	CheckpointBlocks int     `json:"checkpoint_blocks,omitempty"`
}

type ConvergencePoint struct {
	Steps          int     `json:"steps"`
	MaxAbsError    float64 `json:"max_abs_error"`
	MeanAbsError   float64 `json:"mean_abs_error"`
	ErrorDecreased bool    `json:"error_decreased"`
}

type InvalidationResult struct {
	Edit             pipeline.Edit `json:"edit"`
	AffectedBlocks   int           `json:"affected_blocks"`
	UnaffectedBlocks int           `json:"unaffected_blocks"`
	AffectedPixels   int           `json:"affected_pixels"`
	AffectedList     []string      `json:"affected_list"`
	CacheRowsDeleted int64         `json:"cache_rows_deleted"`
	CacheHits        int           `json:"cache_hits_after_edit"`
	CacheMisses      int           `json:"cache_misses_after_edit"`
	GraphEdges       int           `json:"graph_edges"`
	Reason           string        `json:"reason"`
}

type ExceptionResult struct {
	Case         string             `json:"case"`
	Code         model.Code         `json:"code"`
	Message      string             `json:"message"`
	RayID        string             `json:"ray_id,omitempty"`
	MediumID     string             `json:"medium_id,omitempty"`
	CameraID     string             `json:"camera_id,omitempty"`
	Field        string             `json:"field,omitempty"`
	BlockX       int                `json:"block_x"`
	BlockY       int                `json:"block_y"`
	FirstPixelX  int                `json:"first_pixel_x"`
	FirstPixelY  int                `json:"first_pixel_y"`
	Measurements map[string]float64 `json:"measurements,omitempty"`
}

type Report struct {
	GeneratedAt        string                 `json:"generated_at"`
	ElapsedMS          float64                `json:"elapsed_ms"`
	Seed               int64                  `json:"seed"`
	SceneID            string                 `json:"scene_id"`
	SceneHash          string                 `json:"scene_hash"`
	SnapshotVersion    string                 `json:"snapshot_version"`
	CameraVersion      string                 `json:"camera_version"`
	RecipeVersion      string                 `json:"recipe_version"`
	AlgorithmVersion   string                 `json:"algorithm_version"`
	CameraIndex        int                    `json:"camera_index"`
	TruthRays          int                    `json:"truth_rays"`
	Validation         string                 `json:"validation"`
	FullPath           PathResult             `json:"full_path"`
	RealtimePath       PathResult             `json:"realtime_path"`
	ResumePath         PathResult             `json:"resume_path"`
	ThreePathsEqual    bool                   `json:"three_paths_equal"`
	Preview            PathResult             `json:"preview_path"`
	PreviewDeadlineMet bool                   `json:"preview_deadline_met"`
	PreviewLatencyMS   int                    `json:"preview_latency_ms"`
	PreviewCompare     pipeline.CompareReport `json:"preview_compare"`
	Convergence        []ConvergencePoint     `json:"convergence"`
	NaN                bool                   `json:"non_finite_values"`
	Invalidation       InvalidationResult     `json:"invalidation"`
	Exceptions         []ExceptionResult      `json:"exceptions"`
	QueueEvents        []model.QueueEvent     `json:"queue_events"`
	CheckpointBlocks   int                    `json:"checkpoint_blocks"`
	SQLiteDriver       string                 `json:"sqlite_driver"`
	Artifacts          []string               `json:"artifacts"`
}

func Run(opts Options) (Report, error) {
	started := time.Now()
	if opts.Seed == 0 {
		opts.Seed = generator.DefaultSeed
	}
	if opts.CameraIndex < 0 {
		opts.CameraIndex = 2
	}
	if opts.OutDir == "" {
		opts.OutDir = "artifacts"
	}
	if opts.WorkDir == "" {
		tmp, err := os.MkdirTemp("", "volume-kernel-*")
		if err != nil {
			return Report{}, err
		}
		defer os.RemoveAll(tmp)
		opts.WorkDir = tmp
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return Report{}, err
	}
	for _, name := range []string{"realtime.db", "resume.db", "cache.db", "failure.db"} {
		_ = os.Remove(filepath.Join(opts.WorkDir, name))
		_ = os.Remove(filepath.Join(opts.WorkDir, name+"-shm"))
		_ = os.Remove(filepath.Join(opts.WorkDir, name+"-wal"))
	}
	report := Report{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Seed:             opts.Seed,
		CameraIndex:      opts.CameraIndex,
		AlgorithmVersion: model.AlgorithmVersion,
		SQLiteDriver:     "modernc.org/sqlite v1.34.5 (pure Go)",
	}
	s := generator.Generate(opts.Seed)
	if err := scene.Validate(s); err != nil {
		return report, err
	}
	report.SceneID = s.ID
	report.Validation = "PASS"
	snap, err := model.NewSnapshot(s)
	if err != nil {
		return report, err
	}
	report.SceneHash = snap.SceneHash
	report.SnapshotVersion = snap.Version
	report.CameraVersion = snap.CameraVersion
	report.RecipeVersion = snap.RecipeVersion
	truth, err := generator.BuildTruth(s, []int{opts.CameraIndex})
	if err != nil {
		return report, err
	}
	report.TruthRays = len(truth.Rays)
	if err := writeJSON(filepath.Join(opts.OutDir, "scene.json"), s); err != nil {
		return report, err
	}
	if err := writeJSON(filepath.Join(opts.OutDir, "truth.json"), truth); err != nil {
		return report, err
	}
	report.Artifacts = append(report.Artifacts, "scene.json", "truth.json")

	engine := &pipeline.Engine{}
	full, err := engine.Render(snap, pipeline.RenderOptions{CameraIndex: opts.CameraIndex, Mode: "exact", MaxParallel: 1})
	if err != nil {
		return report, err
	}
	report.FullPath = pathResult(full, 0)
	if err := imageout.WriteFramePNG(filepath.Join(opts.OutDir, "exact.png"), full.Frame, 8); err != nil {
		return report, err
	}
	report.Artifacts = append(report.Artifacts, "exact.png")

	realtimeDB := filepath.Join(opts.WorkDir, "realtime.db")
	realtimeStore, err := checkpoint.Open(realtimeDB)
	if err != nil {
		return report, err
	}
	realtime, err := (&pipeline.Engine{Store: realtimeStore}).Render(snap, pipeline.RenderOptions{
		CameraIndex: opts.CameraIndex, Mode: "exact", MaxParallel: 4,
		UsePersistentCache: true, UseCheckpoint: true, CommitCheckpoint: true,
	})
	if err != nil {
		realtimeStore.Close()
		return report, err
	}
	rtCount, _ := realtimeStore.CheckpointCount()
	realtimeStore.Close()
	report.RealtimePath = pathResult(realtime, rtCount)
	report.QueueEvents = realtime.Frame.Queue

	resumeDB := filepath.Join(opts.WorkDir, "resume.db")
	resumeStore, err := checkpoint.Open(resumeDB)
	if err != nil {
		return report, err
	}
	defer resumeStore.Close()
	partial, err := (&pipeline.Engine{Store: resumeStore}).Render(snap, pipeline.RenderOptions{
		CameraIndex: opts.CameraIndex, Mode: "exact", MaxParallel: 2,
		UseCheckpoint: true, CommitCheckpoint: true, CommitLimit: 5,
	})
	if err != nil {
		return report, err
	}
	if !partial.StoppedEarly {
		return report, fmt.Errorf("checkpoint interruption did not stop early")
	}
	if err := resumeStore.Close(); err != nil {
		return report, err
	}
	resumeStore, err = checkpoint.Open(resumeDB)
	if err != nil {
		return report, err
	}
	resumed, err := (&pipeline.Engine{Store: resumeStore}).Render(snap, pipeline.RenderOptions{
		CameraIndex: opts.CameraIndex, Mode: "exact", MaxParallel: 3,
		UseCheckpoint: true, Resume: true, CommitCheckpoint: true,
	})
	if err != nil {
		return report, err
	}
	cpCount, err := resumeStore.CheckpointCount()
	if err != nil {
		return report, err
	}
	report.ResumePath = pathResult(resumed, cpCount)
	report.CheckpointBlocks = cpCount

	report.ThreePathsEqual = pipeline.FramesEqualPixels(full.Frame, realtime.Frame) &&
		pipeline.FramesEqualPixels(full.Frame, resumed.Frame) &&
		report.FullPath.FrameHash == report.RealtimePath.FrameHash &&
		report.FullPath.FrameHash == report.ResumePath.FrameHash
	if !report.ThreePathsEqual {
		return report, fmt.Errorf("three exact rendering paths are not pixel-identical")
	}

	preview, err := engine.Render(snap, pipeline.RenderOptions{CameraIndex: opts.CameraIndex, Mode: "preview", MaxParallel: 4})
	if err != nil {
		return report, err
	}
	report.Preview = pathResult(preview, 0)
	report.PreviewDeadlineMet = preview.PreviewDeadlineMet
	report.PreviewLatencyMS = s.Recipe.PreviewLatencyMS
	previewReport, err := pipeline.CompareFrames(preview.Frame, full.Frame, s.Recipe.PixelTolerance)
	report.PreviewCompare = previewReport
	if err != nil {
		return report, err
	}
	report.NaN = hasNonFinite(full.Frame) || hasNonFinite(preview.Frame)
	if report.NaN {
		return report, fmt.Errorf("render produced non-finite values")
	}
	if err := imageout.WriteFramePNG(filepath.Join(opts.OutDir, "preview.png"), preview.Frame, 8); err != nil {
		return report, err
	}
	report.Artifacts = append(report.Artifacts, "preview.png")

	report.Convergence, err = convergence(s, truth.Rays, opts.CameraIndex, []int{16, 32, 64, 96, 192})
	if err != nil {
		return report, err
	}
	inv, err := validateInvalidation(opts.WorkDir, snap, opts.CameraIndex)
	if err != nil {
		return report, err
	}
	report.Invalidation = inv
	report.Exceptions, err = collectExceptions(opts.WorkDir, s, snap, preview.Frame, full.Frame, opts.CameraIndex)
	if err != nil {
		return report, err
	}
	report.ElapsedMS = float64(time.Since(started).Microseconds()) / 1000

	reportPath := filepath.Join(opts.OutDir, "verification.json")
	if err := writeJSON(reportPath, report); err != nil {
		return report, err
	}
	report.Artifacts = append(report.Artifacts, "verification.json")
	mdPath := filepath.Join(opts.OutDir, "report.md")
	if err := os.WriteFile(mdPath, []byte(markdown(report)), 0o644); err != nil {
		return report, err
	}
	report.Artifacts = append(report.Artifacts, "report.md")
	if err := writeJSON(reportPath, report); err != nil {
		return report, err
	}
	return report, nil
}

func pathResult(res pipeline.RenderResult, checkpointBlocks int) PathResult {
	return PathResult{
		Pixels:           len(res.Frame.Pixels),
		FrameHash:        frameDigest(res.Frame),
		ElapsedMS:        res.Frame.ElapsedMS,
		CacheHits:        res.CacheHits,
		CacheMisses:      res.CacheMisses,
		CheckpointReused: res.CheckpointReused,
		CheckpointBlocks: checkpointBlocks,
	}
}

func frameDigest(frame model.Frame) string {
	type p struct {
		X, Y int
		T, L float64
	}
	ps := make([]p, len(frame.Pixels))
	for i, px := range frame.Pixels {
		ps[i] = p{px.X, px.Y, px.Transmittance, px.ScatteredRadiance}
	}
	h, _ := model.HashValue(ps)
	return h
}

func convergence(s model.Scene, truthRays []model.TruthRay, cameraIndex int, stepsList []int) ([]ConvergencePoint, error) {
	byRay := make(map[string]model.TruthRay, len(truthRays))
	for _, tr := range truthRays {
		byRay[tr.RayID] = tr
	}
	var out []ConvergencePoint
	last := 0.0
	for i, steps := range stepsList {
		maxErr := 0.0
		sum := 0.0
		for y := 0; y < s.Viewport.Height; y++ {
			for x := 0; x < s.Viewport.Width; x++ {
				pr, _, err := kernel.RenderRay(s, s.CameraTrack.Samples[cameraIndex], s.Viewport, x, y, kernel.Options{
					Steps: steps, ShadowSamples: s.Recipe.ShadowSamples, StepBudget: steps,
				})
				if err != nil {
					return out, err
				}
				tr := byRay[pr.RayID]
				d := abs(pr.ScatteredRadiance - tr.ScatteredRadiance)
				if dt := abs(pr.Transmittance - tr.Transmittance); dt > d {
					d = dt
				}
				sum += d
				if d > maxErr {
					maxErr = d
				}
			}
		}
		mean := sum / float64(s.Viewport.Width*s.Viewport.Height)
		decreased := i == 0 || maxErr <= last+1e-10
		if !decreased {
			return out, fmt.Errorf("step convergence is not monotonic at %d steps: %.9f > %.9f", steps, maxErr, last)
		}
		out = append(out, ConvergencePoint{Steps: steps, MaxAbsError: maxErr, MeanAbsError: mean, ErrorDecreased: decreased})
		last = maxErr
	}
	return out, nil
}

func validateInvalidation(workDir string, snap model.SceneSnapshot, cameraIndex int) (InvalidationResult, error) {
	var out InvalidationResult
	store, err := checkpoint.Open(filepath.Join(workDir, "cache.db"))
	if err != nil {
		return out, err
	}
	defer store.Close()
	engine := &pipeline.Engine{Store: store}
	base, err := engine.Render(snap, pipeline.RenderOptions{
		CameraIndex: cameraIndex, Mode: "exact", MaxParallel: 4, UsePersistentCache: true,
	})
	if err != nil {
		return out, err
	}
	graph, err := pipeline.BuildDependencyGraph(snap, cameraIndex)
	if err != nil {
		return out, err
	}
	edit := pipeline.Edit{Kind: pipeline.EditVoxel, MediumID: "media-beta", IX: 2, IY: 2, IZ: 2, Reason: "acceptance brush edit"}
	inv, err := pipeline.InvalidateForEdit(graph, edit, store)
	if err != nil {
		return out, err
	}
	edited := generator.CloneScene(snap.Scene)
	edited = pipeline.ApplyEdit(edited, edit)
	if err := scene.Validate(edited); err != nil {
		return out, err
	}
	editedSnap, err := model.NewSnapshot(edited)
	if err != nil {
		return out, err
	}
	after, err := engine.Render(editedSnap, pipeline.RenderOptions{
		CameraIndex: cameraIndex, Mode: "exact", MaxParallel: 4, UsePersistentCache: true,
	})
	if err != nil {
		return out, err
	}
	out = InvalidationResult{
		Edit:             edit,
		AffectedBlocks:   len(inv.AffectedBlocks),
		UnaffectedBlocks: inv.Unaffected,
		AffectedPixels:   inv.AffectedPixels,
		AffectedList:     inv.AffectedBlocks,
		CacheRowsDeleted: inv.CacheRowsDeleted,
		CacheHits:        after.CacheHits,
		CacheMisses:      after.CacheMisses,
		GraphEdges:       len(graph.Edges),
		Reason:           "voxel-level dependency propagation: density voxel -> ray segment -> block -> pixel",
	}
	if len(inv.AffectedBlocks) == 0 || len(inv.AffectedBlocks) >= len(base.Frame.Blocks) {
		return out, fmt.Errorf("invalidation did not affect a strict subset of blocks")
	}
	if after.CacheHits != inv.Unaffected {
		return out, fmt.Errorf("unaffected cache reuse mismatch: hits=%d unaffected=%d", after.CacheHits, inv.Unaffected)
	}
	if after.CacheMisses != len(inv.AffectedBlocks) {
		return out, fmt.Errorf("affected cache miss mismatch: misses=%d affected=%d", after.CacheMisses, len(inv.AffectedBlocks))
	}
	return out, nil
}

func collectExceptions(workDir string, s model.Scene, snap model.SceneSnapshot, preview, exact model.Frame, cameraIndex int) ([]ExceptionResult, error) {
	var out []ExceptionResult
	raw, err := json.Marshal(s)
	if err != nil {
		return out, err
	}
	_, err = scene.LoadJSON(raw[:len(raw)/2])
	out = append(out, exception("decode-truncated-medium", err))

	_, err = scene.Inject(s, scene.InjectOrder)
	out = append(out, exception("camera-order-inversion", err))

	_, err = scene.Inject(s, scene.InjectDens)
	out = append(out, exception("density-resolution-overflow", err))

	badCoeff := generator.CloneScene(s)
	badCoeff.Media[0].Absorption = -0.1
	out = append(out, exception("negative-absorption", scene.Validate(badCoeff)))

	_, _, err = kernel.RenderRay(s, s.CameraTrack.Samples[cameraIndex], s.Viewport, s.Viewport.Width/2, s.Viewport.Height/2, kernel.Options{
		Steps: s.Recipe.Steps, ShadowSamples: s.Recipe.ShadowSamples, StepBudget: 8,
	})
	out = append(out, exception("step-budget-exceeded", err))

	failStore, err := checkpoint.Open(filepath.Join(workDir, "failure.db"))
	if err != nil {
		return out, err
	}
	_, err = (&pipeline.Engine{Store: failStore}).Render(snap, pipeline.RenderOptions{
		CameraIndex: cameraIndex, Mode: "exact", UseCheckpoint: true, CommitCheckpoint: true,
		MaxParallel: 2, FailBlock: &pipeline.BlockCoord{X: 1, Y: 0},
	})
	failStore.Close()
	out = append(out, exception("single-block-failure", err))

	_, err = pipeline.CompareFrames(preview, exact, 0)
	out = append(out, exception("preview-exact-divergence", err))

	want := []model.Code{model.EDecode, model.EOrder, model.EDensity, model.EPara, model.EBudget, model.EBlock, model.EDiverge}
	for i := range out {
		if out[i].Code != want[i] {
			return out, fmt.Errorf("exception %s produced %s, want %s", out[i].Case, out[i].Code, want[i])
		}
	}
	return out, nil
}

func exception(caseName string, err error) ExceptionResult {
	r := ExceptionResult{Case: caseName}
	var ke *model.Error
	if !errors.As(err, &ke) {
		r.Message = fmt.Sprintf("%v", err)
		return r
	}
	r.Code = ke.Code
	r.Message = ke.Error()
	r.RayID = ke.RayID
	r.MediumID = ke.MediumID
	r.CameraID = ke.CameraID
	r.Field = ke.Field
	r.BlockX, r.BlockY = ke.BlockX, ke.BlockY
	r.FirstPixelX, r.FirstPixelY = ke.FirstPixelX, ke.FirstPixelY
	r.Measurements = ke.Measurements
	return r
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func markdown(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Volume Kernel Verification\n\n")
	fmt.Fprintf(&b, "- Scene: `%s`\n- Seed: `%d`\n- Snapshot: `%s`\n- Camera: `%d`\n- Algorithm: `%s`\n- SQLite: %s\n\n", r.SceneID, r.Seed, r.SnapshotVersion, r.CameraIndex, r.AlgorithmVersion, r.SQLiteDriver)
	fmt.Fprintf(&b, "## Acceptance Summary\n\n")
	fmt.Fprintf(&b, "| Check | Result |\n|---|---|\n")
	fmt.Fprintf(&b, "| Scene validation | %s |\n", r.Validation)
	fmt.Fprintf(&b, "| Three exact paths equal | %t |\n", r.ThreePathsEqual)
	fmt.Fprintf(&b, "| Pixels per path | %d / %d / %d |\n", r.FullPath.Pixels, r.RealtimePath.Pixels, r.ResumePath.Pixels)
	fmt.Fprintf(&b, "| Frame digests | `%s` / `%s` / `%s` |\n", shortHash(r.FullPath.FrameHash), shortHash(r.RealtimePath.FrameHash), shortHash(r.ResumePath.FrameHash))
	fmt.Fprintf(&b, "| Preview deadline | %t (%.3f ms / %d ms) |\n", r.PreviewDeadlineMet, r.Preview.ElapsedMS, r.PreviewLatencyMS)
	fmt.Fprintf(&b, "| Preview max / mean error | %.9g / %.9g |\n", r.PreviewCompare.MaxAbsError, r.PreviewCompare.MeanAbsError)
	fmt.Fprintf(&b, "| Preview over-tolerance rate | %.6f |\n", r.PreviewCompare.OverToleranceRate)
	fmt.Fprintf(&b, "| Checkpoint blocks | %d |\n", r.CheckpointBlocks)
	fmt.Fprintf(&b, "| Invalidated / reused blocks | %d / %d |\n", r.Invalidation.AffectedBlocks, r.Invalidation.UnaffectedBlocks)
	fmt.Fprintf(&b, "| Cache hits / misses after edit | %d / %d |\n", r.Invalidation.CacheHits, r.Invalidation.CacheMisses)
	fmt.Fprintf(&b, "| Budget queue events | %d |\n\n", len(r.QueueEvents))
	fmt.Fprintf(&b, "## Step Convergence\n\n| Steps | Max abs error | Mean abs error |\n|---:|---:|---:|\n")
	for _, p := range r.Convergence {
		fmt.Fprintf(&b, "| %d | %.9g | %.9g |\n", p.Steps, p.MaxAbsError, p.MeanAbsError)
	}
	fmt.Fprintf(&b, "\n## Exception Chains\n\n| Case | Code | Location |\n|---|---|---|\n")
	for _, e := range r.Exceptions {
		var locParts []string
		if e.MediumID != "" {
			locParts = append(locParts, "medium="+e.MediumID)
		}
		if e.CameraID != "" {
			locParts = append(locParts, "camera="+e.CameraID)
		}
		if e.Field != "" {
			locParts = append(locParts, "field="+e.Field)
		}
		if e.BlockX >= 0 || e.BlockY >= 0 {
			locParts = append(locParts, fmt.Sprintf("block=(%d,%d)", e.BlockX, e.BlockY))
		}
		if e.RayID != "" {
			locParts = append(locParts, "ray="+e.RayID)
		}
		if e.FirstPixelX >= 0 {
			locParts = append(locParts, fmt.Sprintf("pixel=(%d,%d)", e.FirstPixelX, e.FirstPixelY))
		}
		loc := strings.Join(locParts, " ")
		fmt.Fprintf(&b, "| %s | `%s` | %s |\n", e.Case, e.Code, strings.ReplaceAll(loc, "|", "\\|"))
	}
	fmt.Fprintf(&b, "\n## Artifacts\n\n")
	for _, a := range r.Artifacts {
		fmt.Fprintf(&b, "- `%s`\n", a)
	}
	fmt.Fprintf(&b, "\nRun: `go run ./cmd/volume-lab verify`\n")
	return b.String()
}

func shortHash(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func hasNonFinite(frame model.Frame) bool {
	for _, p := range frame.Pixels {
		if !finite(p.Transmittance) || !finite(p.ScatteredRadiance) {
			return true
		}
	}
	return false
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
