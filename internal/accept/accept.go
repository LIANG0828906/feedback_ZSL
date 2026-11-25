package accept

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/compare"
	"qualitybench/internal/core"
	"qualitybench/internal/dataset"
	"qualitybench/internal/generator"
	"qualitybench/internal/model"
	"qualitybench/internal/pipeline"
	"qualitybench/internal/runner"
	"qualitybench/internal/store"
	"qualitybench/internal/verify"
)

type Options struct {
	Out         string
	Seed        int64
	ImageCount  int
	Workers     int
	ResumeAfter int
	TaskDelay   time.Duration
}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Code   string `json:"code,omitempty"`
	Detail any    `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

type PathSummary struct {
	RunID       string `json:"run_id"`
	Completed   bool   `json:"completed"`
	Interrupted bool   `json:"interrupted"`
	Invalidated bool   `json:"invalidated"`
	Images      int    `json:"images"`
	CacheHits   int    `json:"cache_hits"`
	CacheMisses int    `json:"cache_misses"`
	ElapsedNS   int64  `json:"elapsed_ns"`
}

type EditSummary struct {
	InitialVersion   string `json:"initial_version"`
	EditedVersion    string `json:"edited_version"`
	MovedVersion     string `json:"moved_version"`
	RollbackVersion  string `json:"rollback_version"`
	FinalVersion     string `json:"final_version"`
	InitialHash      string `json:"initial_hash"`
	EditedHash       string `json:"edited_hash"`
	MovedHash        string `json:"moved_hash"`
	RollbackHash     string `json:"rollback_hash"`
	FinalHash        string `json:"final_hash"`
	Invalidations    int    `json:"invalidations"`
	RecomputedBlocks int    `json:"recomputed_blocks"`
	UnaffectedReused int    `json:"unaffected_reused"`
	CacheHits        int    `json:"cache_hits"`
	CacheMisses      int    `json:"cache_misses"`
	DirectMatch      bool   `json:"direct_match"`
}

type ResourceSummary struct {
	Workers      int   `json:"workers"`
	BufferWaits  int   `json:"buffer_waits"`
	QueueWaits   int   `json:"queue_waits"`
	MaxWaitNS    int64 `json:"max_wait_ns"`
	StableCommit bool  `json:"stable_commit"`
}

type Report struct {
	SchemaVersion string                 `json:"schema_version"`
	StartedAt     time.Time              `json:"started_at"`
	ElapsedNS     int64                  `json:"elapsed_ns"`
	Seed          int64                  `json:"seed"`
	Sample        map[string]any         `json:"sample"`
	ProjectFP     string                 `json:"project_fingerprint"`
	TruthFP       string                 `json:"truth_fingerprint"`
	Paths         map[string]PathSummary `json:"paths"`
	Comparisons   []map[string]any       `json:"comparisons"`
	Verification  []map[string]any       `json:"verification"`
	Edits         EditSummary            `json:"edits"`
	Resources     ResourceSummary        `json:"resources"`
	SQLite        map[string]any         `json:"sqlite"`
	Checks        []Check                `json:"checks"`
	Passed        bool                   `json:"passed"`
}

func Run(ctx context.Context, opts Options) (*Report, error) {
	start := time.Now()
	if opts.Out == "" {
		opts.Out = filepath.Join("artifacts", "acceptance")
	}
	if opts.Seed == 0 {
		opts.Seed = 424242
	}
	if opts.ImageCount <= 0 {
		opts.ImageCount = 3
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.ResumeAfter <= 0 {
		opts.ResumeAfter = 67
	}
	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return nil, err
	}
	sampleDir := filepath.Join(opts.Out, "samples")
	genOpts := generator.DefaultOptions()
	genOpts.Seed = opts.Seed
	genOpts.ImageCount = opts.ImageCount
	if _, _, err := generator.WriteDataset(sampleDir, genOpts); err != nil {
		return nil, err
	}
	projectPath := filepath.Join(sampleDir, "project.json")
	truthPath := filepath.Join(sampleDir, "truth.json")
	ds, err := dataset.Load(projectPath, truthPath)
	if err != nil {
		return nil, err
	}
	report := &Report{
		SchemaVersion: "qualitybench.acceptance.v1",
		StartedAt:     start.UTC(),
		Seed:          opts.Seed,
		ProjectFP:     ds.ProjectFP,
		TruthFP:       ds.TruthFP,
		Paths:         make(map[string]PathSummary),
		SQLite:        make(map[string]any),
	}
	report.Sample = map[string]any{
		"directory":       sampleDir,
		"images":          len(ds.Project.Images),
		"width":           ds.Project.Width,
		"height":          ds.Project.Height,
		"block_size":      ds.Project.BlockSize,
		"blocks":          (ds.Project.Width / ds.Project.BlockSize) * (ds.Project.Height / ds.Project.BlockSize),
		"steps_per_image": len(ds.Project.Chains[0].Steps),
	}
	addCheck(report, "schema_and_range_validation", true, "", map[string]any{
		"images": len(ds.Project.Images),
	})

	dbPath := filepath.Join(opts.Out, "acceptance.db")
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	st, err := store.Open(dbPath)
	if err != nil {
		return report, err
	}
	defer st.Close()

	runAll := func(runID string, mode pipeline.Mode, stop int) ([]*pipeline.Result, error) {
		return runner.RunImages(ctx, ds, st, runner.Config{
			RunID: runID, Mode: mode, Workers: opts.Workers,
			MaxThreadBufferBytes: 1 << 20, BufferCapacityBytes: 4 << 20,
			PersistentCache: true, StopAfter: stop, TaskDelay: opts.TaskDelay,
		}, nil)
	}
	full, err := runAll("full", pipeline.ModeFull, 0)
	if err != nil {
		return report, err
	}
	report.Paths["full"] = summarizePath(full)
	addCheck(report, "full_recompute_completed", report.Paths["full"].Completed, "", nil)

	live, err := runAll("live", pipeline.ModeLive, 0)
	if err != nil {
		return report, err
	}
	report.Paths["live"] = summarizePath(live)
	addCheck(report, "live_generation_completed", report.Paths["live"].Completed, "", nil)

	resumeFirst, err := runAll("resume", pipeline.ModeLive, opts.ResumeAfter)
	if err != nil {
		return report, err
	}
	interrupted := allInterrupted(resumeFirst)
	addCheck(report, "resume_interrupted_after_checkpoint", interrupted, "", map[string]any{
		"stop_after": opts.ResumeAfter,
	})
	resume, err := runAll("resume", pipeline.ModeResume, 0)
	if err != nil {
		return report, err
	}
	report.Paths["resume"] = summarizePath(resume)
	addCheck(report, "resume_generation_completed", report.Paths["resume"].Completed, "", nil)

	for _, pair := range [][2]string{{"live", "full"}, {"resume", "full"}, {"live", "resume"}} {
		cmp, cmpErr := compare.Runs(ctx, st, pair[0], pair[1], nil)
		entry := map[string]any{"a": pair[0], "b": pair[1]}
		if cmp != nil {
			entry["identical"] = cmp.Identical
			entry["blocks"] = cmp.Blocks
			entry["differences"] = cmp.Differences
		}
		if cmpErr != nil {
			entry["error"] = cmpErr.Error()
		}
		report.Comparisons = append(report.Comparisons, entry)
		addCheck(report, "compare_"+pair[0]+"_"+pair[1], cmpErr == nil && cmp.Identical, string(core.CodeOf(cmpErr)), nil)
	}

	for _, runID := range []string{"full", "live", "resume"} {
		vr, vErr := verify.Run(ctx, st, runID, ds.Truth, ds.TruthFP)
		entry := map[string]any{"run_id": runID}
		if vr != nil {
			entry["verified"] = vr.Verified
			entry["blocks"] = vr.Blocks
			entry["metrics_passed"] = vr.MetricsPassed
			entry["attribution_passed"] = vr.AttributionPassed
			entry["samples_passed"] = vr.SamplesPassed
			entry["failures"] = len(vr.Failures)
		}
		if vErr != nil {
			entry["error"] = vErr.Error()
		}
		report.Verification = append(report.Verification, entry)
		addCheck(report, "independent_verify_"+runID, vErr == nil && vr.Verified, string(core.CodeOf(vErr)), nil)
	}

	editSummary, err := runEdits(ctx, ds, st)
	if err != nil {
		return report, err
	}
	report.Edits = editSummary
	addCheck(report, "localized_edit_direct_match", editSummary.DirectMatch, "", map[string]any{
		"recomputed_blocks": editSummary.RecomputedBlocks,
		"unaffected_reused": editSummary.UnaffectedReused,
	})
	addCheck(report, "rollback_cache_and_pixel_equality",
		editSummary.RollbackHash == editSummary.InitialHash && editSummary.CacheHits > 0,
		"", map[string]any{"cache_hits": editSummary.CacheHits})

	resources, resourceFinal, err := runResourceProbe(ctx, ds, st, opts.Workers)
	if err != nil {
		return report, err
	}
	report.Resources = resources
	addCheck(report, "buffer_bound_wait_recorded", resources.BufferWaits > 0 && resources.MaxWaitNS > 0, "", nil)
	addCheck(report, "buffer_bound_pixels_match",
		resourceFinal == full[0].StepFrames[len(full[0].StepFrames)-1].Hash(), "", nil)

	errorChecks, extra, err := runErrorChecks(ctx, ds, st, dbPath, projectPath)
	if err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, errorChecks...)
	for k, v := range extra {
		report.SQLite[k] = v
	}
	serialized, manifestRows, err := inspectSQLite(dbPath)
	if err != nil {
		return report, err
	}
	report.SQLite["commit_serialized"] = serialized
	report.SQLite["manifest_rows"] = manifestRows
	addCheck(report, "sqlite_commit_sequence_unique", serialized, "", map[string]any{
		"manifest_rows": manifestRows,
	})

	report.SQLite["database"] = dbPath
	report.SQLite["corruption_invalidated"] = extra["corruption_invalidated"]
	report.Passed = true
	for _, check := range report.Checks {
		if !check.Passed {
			report.Passed = false
		}
	}
	report.ElapsedNS = time.Since(start).Nanoseconds()
	if err := WriteReport(opts.Out, report); err != nil {
		return report, err
	}
	if !report.Passed {
		return report, core.NewError(core.CodeStage, "one or more acceptance checks failed")
	}
	return report, nil
}

func inspectSQLite(dbPath string) (bool, int, error) {
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return false, 0, err
	}
	defer raw.Close()
	var total, distinct int
	if err := raw.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT commit_seq) FROM (
SELECT commit_seq FROM block_steps
UNION ALL
SELECT commit_seq FROM block_manifests)`).Scan(&total, &distinct); err != nil {
		return false, 0, err
	}
	var manifests int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM block_manifests`).Scan(&manifests); err != nil {
		return false, 0, err
	}
	return total == distinct && manifests > 0, manifests, nil
}

func runEdits(ctx context.Context, ds *dataset.Dataset, st checkpoint.Store) (EditSummary, error) {
	var out EditSummary
	eng, err := runner.NewEngine(ds, "img-00", st, runner.Config{
		RunID: "edits", Mode: pipeline.ModeLive, Workers: 4,
		MaxThreadBufferBytes: 1 << 20, BufferCapacityBytes: 4 << 20, PersistentCache: true,
	})
	if err != nil {
		return out, err
	}
	initial, err := eng.Run(ctx, pipeline.ModeLive)
	if err != nil {
		return out, err
	}
	out.InitialVersion = initial.VersionID
	out.InitialHash = initial.StepFrames[len(initial.StepFrames)-1].Hash()

	strength := 0.71
	edit, err := eng.ApplyEdit(ctx, model.Edit{
		Op: "set_strength", ImageID: "img-00", Step: 2, Strength: &strength,
	})
	if err != nil {
		return out, err
	}
	out.EditedVersion = edit.VersionID
	out.EditedHash = edit.FinalFrameHash
	out.Invalidations += len(edit.Invalidations)
	out.RecomputedBlocks += edit.RecomputedBlocks
	out.UnaffectedReused += edit.UnaffectedReused
	out.CacheHits += edit.Cache.Hits
	out.CacheMisses += edit.Cache.Misses

	editedChain := ds.Project.Chains[0].Clone()
	editedChain.Steps[1].Strength = strength
	direct1, err := runDirectChain(ctx, ds, st, "edits-direct-1", editedChain)
	if err != nil {
		return out, err
	}
	directMatch := sameFrames(eng.Frames(), direct1.StepFrames)

	moveEdit, err := eng.ApplyEdit(ctx, model.Edit{
		Op: "move_step", ImageID: "img-00", From: 5, To: 2,
	})
	if err != nil {
		return out, err
	}
	out.MovedVersion = moveEdit.VersionID
	out.MovedHash = moveEdit.FinalFrameHash
	out.Invalidations += len(moveEdit.Invalidations)
	out.RecomputedBlocks += moveEdit.RecomputedBlocks
	out.UnaffectedReused += moveEdit.UnaffectedReused
	out.CacheHits += moveEdit.Cache.Hits
	out.CacheMisses += moveEdit.Cache.Misses

	movedChain := ds.Project.Chains[0].Clone()
	overexpose := movedChain.Steps[4]
	movedChain.Steps = append(movedChain.Steps[:1],
		append([]model.Step{overexpose}, movedChain.Steps[1:4]...)...)
	movedChain.Steps[2].Strength = strength
	direct2, err := runDirectChain(ctx, ds, st, "edits-direct-2", movedChain)
	if err != nil {
		return out, err
	}
	directMatch = directMatch && sameFrames(eng.Frames(), direct2.StepFrames)

	rollback, err := eng.Rollback(ctx, out.InitialVersion)
	if err != nil {
		return out, err
	}
	out.RollbackVersion = rollback.VersionID
	out.RollbackHash = rollback.FinalFrameHash
	out.CacheHits += rollback.Cache.Hits
	out.CacheMisses += rollback.Cache.Misses

	reapply, err := eng.ApplyEdit(ctx, model.Edit{
		Op: "set_strength", ImageID: "img-00", Step: 2, Strength: &strength,
	})
	if err != nil {
		return out, err
	}
	out.FinalVersion = reapply.VersionID
	out.FinalHash = reapply.FinalFrameHash
	out.CacheHits += reapply.Cache.Hits
	out.CacheMisses += reapply.Cache.Misses
	directMatch = directMatch && reapply.FinalFrameHash == direct1.StepFrames[len(direct1.StepFrames)-1].Hash()
	out.DirectMatch = directMatch && out.Invalidations > 0 && out.UnaffectedReused > 0
	return out, nil
}

func runDirectChain(ctx context.Context, ds *dataset.Dataset, st checkpoint.Store,
	runID string, chain model.Chain) (*pipeline.Result, error) {
	eng, err := pipeline.New(ds.Baseline["img-00"], chain, pipeline.Options{
		RunID: runID, ImageID: "img-00", ImageSeed: ds.Project.Images[0].Seed,
		ProjectFingerprint: ds.ProjectFP, TruthFingerprint: ds.TruthFP,
		AlgorithmVersion: ds.Project.AlgorithmVersion, BlockSize: ds.Project.BlockSize,
		Workers: 4, MaxThreadBufferBytes: 1 << 20, BufferCapacityBytes: 4 << 20,
		PersistentCache: true, Store: st, AttributionShare: ds.Project.AttributionShare,
	})
	if err != nil {
		return nil, err
	}
	res, err := eng.Run(ctx, pipeline.ModeFull)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func runResourceProbe(ctx context.Context, ds *dataset.Dataset, st checkpoint.Store,
	workers int) (ResourceSummary, string, error) {
	scratch := int64(ds.Project.Width*ds.Project.Height*ds.Project.Channels*2 + 4096)
	eng, err := runner.NewEngine(ds, "img-00", st, runner.Config{
		RunID: "resources", Mode: pipeline.ModeFull, Workers: workers,
		MaxThreadBufferBytes: scratch, BufferCapacityBytes: scratch,
		PersistentCache: false,
	})
	if err != nil {
		return ResourceSummary{}, "", err
	}
	res, err := eng.Run(ctx, pipeline.ModeFull)
	if err != nil {
		return ResourceSummary{}, "", err
	}
	var maxWait time.Duration
	for _, event := range append(res.BufferWaits, res.QueueWaits...) {
		if event.Duration > maxWait {
			maxWait = event.Duration
		}
	}
	return ResourceSummary{
		Workers: workers, BufferWaits: len(res.BufferWaits), QueueWaits: len(res.QueueWaits),
		MaxWaitNS: maxWait.Nanoseconds(), StableCommit: res.Completed,
	}, res.StepFrames[len(res.StepFrames)-1].Hash(), nil
}

func runErrorChecks(ctx context.Context, ds *dataset.Dataset, st checkpoint.Store,
	dbPath, projectPath string) ([]Check, map[string]any, error) {
	var checks []Check
	extra := make(map[string]any)
	mk := func(name string, err error, want core.Code, path string, step int, field string) {
		check := Check{Name: name, Passed: false, Code: string(core.CodeOf(err)), Error: errString(err)}
		qe, _ := core.As(err)
		passed := core.CodeOf(err) == want
		if path != "" && qe != nil && qe.Path != path {
			passed = false
		}
		if step > 0 && (qe == nil || qe.Step != step) {
			passed = false
		}
		if field != "" && (qe == nil || qe.Field != field) {
			passed = false
		}
		check.Passed = passed
		checks = append(checks, check)
	}
	missing := mutateProjectJSON(ds, func(m map[string]any) { delete(m, "schema_version") })
	_, _, err := dataset.LoadProject(writeTempJSON(projectPath, "missing.json", missing))
	mk("E_SCHEMA_missing_field", err, core.CodeSchema, "schema_version", 0, "")

	outOfRange := mutateProjectJSON(ds, func(m map[string]any) {
		images := m["images"].([]any)
		shapes := images[0].(map[string]any)["shapes"].([]any)
		shapes[0].(map[string]any)["bbox"].(map[string]any)["x"] = -1.0
	})
	_, _, err = dataset.LoadProject(writeTempJSON(projectPath, "range.json", outOfRange))
	mk("E_RANGE_coordinate", err, core.CodeRange, "images[0].shapes[0].bbox", 0, "")

	badPara := mutateProjectJSON(ds, func(m map[string]any) {
		chains := m["chains"].([]any)
		steps := chains[0].(map[string]any)["steps"].([]any)
		steps[0].(map[string]any)["strength"] = 2.0
	})
	_, _, err = dataset.LoadProject(writeTempJSON(projectPath, "para.json", badPara))
	mk("E_PARA_strength", err, core.CodePara, "", 1, "strength")

	badBlock := mutateProjectJSON(ds, func(m map[string]any) { m["block_size"] = 20.0 })
	_, _, err = dataset.LoadProject(writeTempJSON(projectPath, "block.json", badBlock))
	mk("E_BLOCK_with_candidates", err, core.CodeBlock, "", 0, "")
	if e, _ := core.As(err); e != nil {
		extra["block_candidates"] = e.Details["candidates"]
	}

	m := core.NewMachine(core.StatePending, 1, &core.BlockRef{Index: 0}, "acceptance")
	err = m.Transition(core.StateCommitted)
	statePreserved := m.State() == core.StatePending
	check := Check{Name: "E_STATE_preserves_state", Passed: core.CodeOf(err) == core.CodeState && statePreserved,
		Code: string(core.CodeOf(err)), Error: errString(err)}
	checks = append(checks, check)

	faultEngine, err := runner.NewEngine(ds, "img-00", st, runner.Config{
		RunID: "fault", Mode: pipeline.ModeFull, Workers: 2,
		MaxThreadBufferBytes: 1 << 20, BufferCapacityBytes: 4 << 20, PersistentCache: false,
		Fault: func(step int, block core.Rect) error {
			if step == 2 && block.X == 32 {
				return fmt.Errorf("synthetic acceptance stage fault")
			}
			return nil
		},
	})
	if err != nil {
		return checks, extra, err
	}
	_, err = faultEngine.Run(ctx, pipeline.ModeFull)
	qe, _ := core.As(err)
	stagePassed := core.CodeOf(err) == core.CodeStage && qe != nil && qe.Step == 2 && qe.Block != nil && qe.Block.X == 32
	checks = append(checks, Check{Name: "E_STAGE_step_and_block", Passed: stagePassed,
		Code: string(core.CodeOf(err)), Error: errString(err)})

	if err := cloneRun(dbPath, "full", "diff-corrupt"); err != nil {
		return checks, extra, err
	}
	if err := corruptBlockPixels(dbPath, "diff-corrupt", 2, 32, 16); err != nil {
		return checks, extra, err
	}
	cmp, cmpErr := compare.Runs(ctx, st, "full", "diff-corrupt", nil)
	diffPassed := core.CodeOf(cmpErr) == core.CodeDiff && cmp != nil && cmp.FirstDiff != nil &&
		cmp.FirstDiff.Step == 2 && cmp.FirstDiff.Block != nil &&
		cmp.FirstDiff.Block.X == 32 && cmp.FirstDiff.Block.Y == 16 && cmp.FirstDiff.Field == "pixels"
	detail := map[string]any{}
	if cmp != nil && cmp.FirstDiff != nil {
		detail = map[string]any{
			"step": cmp.FirstDiff.Step, "block": cmp.FirstDiff.Block, "field": cmp.FirstDiff.Field,
		}
	}
	checks = append(checks, Check{Name: "E_DIFF_first_block", Passed: diffPassed,
		Code: string(core.CodeOf(cmpErr)), Detail: detail, Error: errString(cmpErr)})

	if err := runCorruptResume(ctx, ds, st, dbPath); err != nil {
		extra["corruption_invalidated"] = false
		checks = append(checks, Check{Name: "sqlite_hash_mismatch_invalidates_all",
			Passed: false, Error: err.Error()})
	} else {
		extra["corruption_invalidated"] = true
		checks = append(checks, Check{Name: "sqlite_hash_mismatch_invalidates_all", Passed: true})
	}
	return checks, extra, nil
}

func runCorruptResume(ctx context.Context, ds *dataset.Dataset, st checkpoint.Store, dbPath string) error {
	eng, err := runner.NewEngine(ds, "img-00", st, runner.Config{
		RunID: "resume-corrupt", Mode: pipeline.ModeLive, Workers: 2, StopAfter: 17,
		MaxThreadBufferBytes: 1 << 20, BufferCapacityBytes: 4 << 20, PersistentCache: false,
	})
	if err != nil {
		return err
	}
	first, err := eng.Run(ctx, pipeline.ModeLive)
	if err != nil {
		return err
	}
	if !first.Interrupted {
		return fmt.Errorf("corruption probe did not interrupt")
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	_, err = raw.Exec(`UPDATE block_steps SET output_hash='acceptance-corrupt'
WHERE run_id='resume-corrupt' AND rowid=(
  SELECT MIN(rowid) FROM block_steps WHERE run_id='resume-corrupt' AND step=1
)`)
	_ = raw.Close()
	if err != nil {
		return err
	}
	eng2, err := runner.NewEngine(ds, "img-00", st, runner.Config{
		RunID: "resume-corrupt", Mode: pipeline.ModeResume, Workers: 2,
		MaxThreadBufferBytes: 1 << 20, BufferCapacityBytes: 4 << 20, PersistentCache: false,
	})
	if err != nil {
		return err
	}
	second, err := eng2.Run(ctx, pipeline.ModeResume)
	if err != nil {
		return err
	}
	if !second.Invalidated || !second.FallbackFull || !second.Completed {
		return fmt.Errorf("expected invalidate+full, got invalidated=%v fallback=%v completed=%v",
			second.Invalidated, second.FallbackFull, second.Completed)
	}
	return nil
}

func cloneRun(dbPath, source, target string) error {
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer raw.Close()
	var maxSeq int64
	if err := raw.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM (
SELECT MAX(commit_seq) AS seq FROM block_steps
UNION ALL SELECT MAX(commit_seq) AS seq FROM block_manifests)`).Scan(&maxSeq); err != nil {
		return err
	}
	tx, err := raw.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO runs
SELECT ?,image_id,algorithm_version,chain_fingerprint,project_fingerprint,truth_fingerprint,
       width,height,channels,block_size,step_count,'completed',created_at,updated_at
FROM runs WHERE run_id=?`, target, source); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO block_steps
SELECT ?,image_id,step,block_x,block_y,block_w,block_h,state,cache_key,input_hash,
       output_hash,output_digest,chain_fingerprint,algorithm_version,pixels,
       commit_seq+?,created_at
FROM block_steps WHERE run_id=?`, target, maxSeq, source); err != nil {
		return err
	}
	return tx.Commit()
}

func corruptBlockPixels(dbPath, runID string, step, bx, by int) error {
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer raw.Close()
	var (
		imageID, state, cacheKey, inputHash, outputHash, digest, chainFP, algo string
		w, h                                                                   int
		compressed                                                             []byte
		seq                                                                    int64
		y, x                                                                   int
		st                                                                     int
	)
	err = raw.QueryRow(`SELECT image_id,step,block_x,block_y,block_w,block_h,state,cache_key,
input_hash,output_hash,output_digest,chain_fingerprint,algorithm_version,pixels,commit_seq
FROM block_steps WHERE run_id=? AND step=? AND block_x=? AND block_y=?`,
		runID, step, bx, by).Scan(&imageID, &st, &x, &y, &w, &h, &state, &cacheKey,
		&inputHash, &outputHash, &digest, &chainFP, &algo, &compressed, &seq)
	if err != nil {
		return err
	}
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return err
	}
	pixels, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return err
	}
	if len(pixels) == 0 {
		return fmt.Errorf("empty pixel block")
	}
	pixels[0] ^= 0x5a
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(pixels); err != nil {
		return err
	}
	_ = zw.Close()
	artifact := checkpoint.Artifact{
		RunID: runID, ImageID: imageID, Step: st,
		Block: core.Rect{X: x, Y: y, W: w, H: h}, State: core.State(state),
		CacheKey: cacheKey, InputHash: inputHash, OutputHash: checkpoint.PixelHash(pixels),
		ChainFingerprint: chainFP, AlgorithmVersion: algo, Pixels: pixels, CommitSeq: seq,
	}
	artifact.OutputDigest = artifact.ComputeOutputDigest()
	_, err = raw.Exec(`UPDATE block_steps SET pixels=?,output_hash=?,output_digest=?
WHERE run_id=? AND step=? AND block_x=? AND block_y=?`,
		buf.Bytes(), artifact.OutputHash, artifact.OutputDigest, runID, step, bx, by)
	return err
}

func mutateProjectJSON(ds *dataset.Dataset, mutate func(map[string]any)) map[string]any {
	data, _ := json.Marshal(ds.Project)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	mutate(m)
	return m
}

func writeTempJSON(basePath, name string, v any) string {
	path := filepath.Join(filepath.Dir(basePath), name)
	data, _ := json.MarshalIndent(v, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
	return path
}

func summarizePath(results []*pipeline.Result) PathSummary {
	out := PathSummary{Images: len(results)}
	for _, result := range results {
		out.RunID = result.RunID
		out.Completed = out.Completed || result.Completed
		out.Interrupted = out.Interrupted || result.Interrupted
		out.Invalidated = out.Invalidated || result.Invalidated
		out.CacheHits += result.Cache.Hits
		out.CacheMisses += result.Cache.Misses
		out.ElapsedNS += result.Elapsed.Nanoseconds()
	}
	return out
}

func allInterrupted(results []*pipeline.Result) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if !result.Interrupted {
			return false
		}
	}
	return true
}

func sameFrames(a, b []*core.Frame) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == nil || b[i] == nil || a[i].Hash() != b[i].Hash() {
			return false
		}
	}
	return true
}

func addCheck(report *Report, name string, passed bool, code string, detail any) {
	report.Checks = append(report.Checks, Check{Name: name, Passed: passed, Code: code, Detail: detail})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func WriteReport(out string, report *Report) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(out, "report.json"), data, 0o644); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("QualityBench acceptance summary\n")
	b.WriteString("===============================\n\n")
	fmt.Fprintf(&b, "seed: %d\n", report.Seed)
	fmt.Fprintf(&b, "project_fingerprint: %s\n", report.ProjectFP)
	fmt.Fprintf(&b, "truth_fingerprint: %s\n", report.TruthFP)
	fmt.Fprintf(&b, "passed: %v\n", report.Passed)
	fmt.Fprintf(&b, "elapsed_ms: %.3f\n\n", float64(report.ElapsedNS)/1e6)
	b.WriteString("Paths:\n")
	keys := make([]string, 0, len(report.Paths))
	for k := range report.Paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := report.Paths[k]
		fmt.Fprintf(&b, "  %-7s completed=%v interrupted=%v cache_hits=%d cache_misses=%d\n",
			k, p.Completed, p.Interrupted, p.CacheHits, p.CacheMisses)
	}
	b.WriteString("\nChecks:\n")
	for _, c := range report.Checks {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "  %-4s %-38s", status, c.Name)
		if c.Code != "" {
			fmt.Fprintf(&b, " %s", c.Code)
		}
		if c.Error != "" {
			fmt.Fprintf(&b, " %s", c.Error)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nEdits:\n")
	fmt.Fprintf(&b, "  invalidations=%d recomputed_blocks=%d unaffected_reused=%d cache_hits=%d direct_match=%v\n",
		report.Edits.Invalidations, report.Edits.RecomputedBlocks,
		report.Edits.UnaffectedReused, report.Edits.CacheHits, report.Edits.DirectMatch)
	fmt.Fprintf(&b, "  initial_hash=%s\n  rollback_hash=%s\n  final_hash=%s\n",
		report.Edits.InitialHash, report.Edits.RollbackHash, report.Edits.FinalHash)
	b.WriteString("\nResources:\n")
	fmt.Fprintf(&b, "  workers=%d buffer_waits=%d queue_waits=%d max_wait_ms=%.3f\n",
		report.Resources.Workers, report.Resources.BufferWaits, report.Resources.QueueWaits,
		float64(report.Resources.MaxWaitNS)/1e6)
	return os.WriteFile(filepath.Join(out, "summary.txt"), []byte(b.String()), 0o644)
}
