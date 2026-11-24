package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"

	"yuvench/errs"
	"yuvench/filters"
	"yuvench/yuv"
)

type VerifyCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type VerifyReport struct {
	OK                bool          `json:"ok"`
	Image             string        `json:"image"`
	Seed              int64         `json:"seed"`
	Checks            []VerifyCheck `json:"checks"`
	Full              *RunResult    `json:"full,omitempty"`
	Incremental       *RunResult    `json:"incremental,omitempty"`
	IncrementalBase   *RunResult    `json:"incrementalBase,omitempty"`
	ResumeFirst       *RunResult    `json:"resumeFirst,omitempty"`
	Resume            *RunResult    `json:"resume,omitempty"`
	RetryFirst        *RunResult    `json:"retryFirst,omitempty"`
	Retry             *RunResult    `json:"retry,omitempty"`
	VersionRejected   *RunResult    `json:"versionRejected,omitempty"`
	SnapshotFull      SnapshotMeta  `json:"snapshotFull"`
	SnapshotResume    SnapshotMeta  `json:"snapshotResume"`
	Heatmap           HeatmapStats  `json:"heatmap"`
	HeatmapRepeatable bool          `json:"heatmapRepeatable"`
}

type verifyState struct {
	report *VerifyReport
	root   string
}

func VerifySamples(manifestPath, imageName, root string, seed int64) (*VerifyReport, error) {
	if root == "" {
		root = filepath.Join("artifacts", "verify")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, &errs.Error{Code: errs.CodeIO, Message: "create verification directory", Cause: err.Error()}
	}
	if _, err := os.Stat(manifestPath); err != nil {
		sampleDir := filepath.Join(root, "..", "samples")
		generated, genErr := yuv.GenerateSamples(sampleDir, seed)
		if genErr != nil {
			return nil, &errs.Error{Code: errs.CodeIO, Message: "generate verification samples", Cause: genErr.Error()}
		}
		manifestPath = generated
	}
	image, err := yuv.LoadImage(manifestPath, imageName)
	if err != nil {
		return nil, err
	}
	if err := yuv.VerifyTruth(image); err != nil {
		return nil, err
	}
	report := &VerifyReport{Image: image.Meta.Name, Seed: seed}
	state := &verifyState{report: report, root: root}
	limits := filters.DefaultLimits()
	baseParams := filters.DefaultParams()
	changedParams := baseParams
	changedParams.YSharpen.Amount = 0.60
	changedParams.UGain.Gain = 1.05
	retryParams := changedParams
	retryParams.YSharpen.Amount = 0.75

	ctx := context.Background()

	// Incremental baseline and selective rerun in one live engine. The first
	// run starts from an empty snapshot, then only the changed stage closure is
	// recomputed while unaffected stage outputs are reused.
	incrementalDB := filepath.Join(root, "incremental.db")
	cleanPath(incrementalDB)
	incStore, err := OpenStore(incrementalDB)
	if err != nil {
		return report, err
	}
	incEngine := NewEngine(incStore)
	baseResult, err := incEngine.Run(ctx, image, baseParams, limits, RunOptions{Mode: "full"})
	if err != nil {
		incEngine.Close()
		return report, err
	}
	report.IncrementalBase = baseResult
	incremental, err := incEngine.Run(ctx, image, changedParams, limits, RunOptions{Mode: "incremental"})
	incEngine.Close()
	if err != nil {
		return report, err
	}
	report.Incremental = incremental
	state.check("full-run", true, "baseline pipeline produced a final image")
	expectedInvalidated := 4
	state.check("incremental-selectivity", len(incremental.Invalidations) == expectedInvalidated,
		fmt.Sprintf("%d of %d stages invalidated", len(incremental.Invalidations), len(incremental.Graph.Nodes)))
	state.check("incremental-cache-reuse", incremental.Stats.CacheHits == 4 && incremental.Stats.Recomputed == 4,
		fmt.Sprintf("recomputed=%d hits=%d", incremental.Stats.Recomputed, incremental.Stats.CacheHits))

	fullB, err := runFresh(ctx, filepath.Join(root, "full.db"), image, changedParams, limits, "full")
	if err != nil {
		return report, err
	}
	report.Full = fullB
	if err := CompareRuns(incremental, fullB, errs.CodeDiff); err != nil {
		state.check("incremental-full-pixel-consistency", false, err.Error())
	} else {
		state.check("incremental-full-pixel-consistency", true, "final and intermediate planes match")
	}

	// Pause after the three-stage luma prefix, then reopen SQLite in a new
	// engine and resume only from committed, version-matched stages.
	resumeDB := filepath.Join(root, "resume.db")
	cleanPath(resumeDB)
	pauseStore, err := OpenStore(resumeDB)
	if err != nil {
		return report, err
	}
	pauseEngine := NewEngine(pauseStore)
	resumeFirst, err := pauseEngine.Run(ctx, image, changedParams, limits, RunOptions{Mode: "full", PauseAfterCommits: 3})
	pauseEngine.Close()
	if err != nil {
		return report, err
	}
	report.ResumeFirst = resumeFirst
	state.check("pause-committed-prefix", resumeFirst.Paused && len(resumeFirst.CommitSequence) == 3,
		fmt.Sprintf("paused=%v commits=%d", resumeFirst.Paused, len(resumeFirst.CommitSequence)))

	resumeStore, err := OpenStore(resumeDB)
	if err != nil {
		return report, err
	}
	resumeEngine := NewEngine(resumeStore)
	resumed, err := resumeEngine.Run(ctx, image, changedParams, limits, RunOptions{Mode: "resume"})
	resumeEngine.Close()
	if err != nil {
		return report, err
	}
	report.Resume = resumed
	state.check("resume-from-committed-stages", resumed.Stats.Recomputed == 5 && resumed.Stats.CacheHits == 3,
		fmt.Sprintf("resumed recomputed=%d hits=%d", resumed.Stats.Recomputed, resumed.Stats.CacheHits))
	if err := CompareRuns(incremental, resumed, errs.CodePath); err != nil {
		state.check("three-path-consistency", false, err.Error())
	} else if err := CompareRuns(resumed, fullB, errs.CodePath); err != nil {
		state.check("three-path-consistency", false, err.Error())
	} else {
		state.check("three-path-consistency", true, "incremental, resume, and full stage/pixel sequences match")
	}
	if equalCommitPrefix(resumeFirst.CommitSequence, fullB.CommitSequence) {
		state.check("commit-sequence-stability", true, "canonical stage sequence and summaries match")
	} else {
		state.check("commit-sequence-stability", false, "paused prefix differs from full commit sequence")
	}
	report.SnapshotFull = fullB.Snapshot
	report.SnapshotResume = resumed.Snapshot
	state.check("snapshot-version-match", fullB.Snapshot.Complete && resumed.Snapshot.Complete && fullB.RunID == resumed.RunID,
		"complete version-matched stage snapshots")

	// Tamper with the global version marker. A new process must reject the
	// entire snapshot rather than partially reusing stages.
	versionDB := filepath.Join(root, "version.db")
	cleanPath(versionDB)
	versionResult, err := runFresh(ctx, versionDB, image, changedParams, limits, "full")
	if err != nil {
		return report, err
	}
	if err := tamperParamVersion(versionDB); err != nil {
		return report, err
	}
	versionStore, err := OpenStore(versionDB)
	if err != nil {
		return report, err
	}
	versionEngine := NewEngine(versionStore)
	versionRejected, err := versionEngine.Run(ctx, image, changedParams, limits, RunOptions{Mode: "incremental"})
	versionEngine.Close()
	if err != nil {
		return report, err
	}
	report.VersionRejected = versionRejected
	state.check("snapshot-version-rejection",
		strings.HasPrefix(versionRejected.SnapshotReason, "snapshot-version-mismatch:") && versionRejected.Stats.Recomputed == 8 && versionRejected.Stats.CacheHits == 0,
		fmt.Sprintf("reason=%s recomputed=%d hits=%d", versionRejected.SnapshotReason, versionRejected.Stats.Recomputed, versionRejected.Stats.CacheHits))
	_ = versionResult

	// Retry an injected stage failure in a live engine. The unaffected stages
	// remain reusable and the retry must equal a clean full run.
	retryDB := filepath.Join(root, "retry.db")
	cleanPath(retryDB)
	retryStore, err := OpenStore(retryDB)
	if err != nil {
		return report, err
	}
	retryEngine := NewEngine(retryStore)
	if _, err := retryEngine.Run(ctx, image, changedParams, limits, RunOptions{Mode: "full"}); err != nil {
		retryEngine.Close()
		return report, err
	}
	faulted := false
	retryFirst, retryErr := retryEngine.Run(ctx, image, retryParams, limits, RunOptions{
		Mode: "incremental",
		Fault: func(node string) error {
			if node == filters.NodeYSharpen && !faulted {
				faulted = true
				return fmt.Errorf("injected stage failure")
			}
			return nil
		},
	})
	report.RetryFirst = retryFirst
	state.check("stage-failure-isolated", retryErr != nil && errs.CodeOf(retryErr) == errs.CodeStage && retryFirst != nil && retryFirst.Stats.Failed == 1,
		"injected Y:sharpen failure was marked failed")
	retryResult, err := retryEngine.Run(ctx, image, retryParams, limits, RunOptions{Mode: "incremental"})
	retryEngine.Close()
	if err != nil {
		return report, err
	}
	report.Retry = retryResult
	state.check("stage-retry-isolation", retryResult.Stats.Recomputed == 3 && retryResult.Stats.CacheHits == 5,
		fmt.Sprintf("retry recomputed=%d hits=%d", retryResult.Stats.Recomputed, retryResult.Stats.CacheHits))
	fullC, err := runFresh(ctx, filepath.Join(root, "full-retry-reference.db"), image, retryParams, limits, "full")
	if err != nil {
		return report, err
	}
	if err := CompareRuns(retryResult, fullC, errs.CodeDiff); err != nil {
		state.check("retry-output-consistency", false, err.Error())
	} else {
		state.check("retry-output-consistency", true, "retry output matches the no-failure path")
	}

	// Heatmap depends only on final images and normalization.
	heat1, heatStats, err := Heatmap(fullB.Original, fullB.Final, changedParams.Heatmap.Normalization)
	if err != nil {
		return report, err
	}
	heat2, heatStats2, err := Heatmap(fullB.Original, fullB.Final, changedParams.Heatmap.Normalization)
	if err != nil {
		return report, err
	}
	repeatable := heatStats.Summary == heatStats2.Summary && imagePixelsEqual(heat1, heat2)
	report.Heatmap = heatStats
	report.HeatmapRepeatable = repeatable
	state.check("heatmap-deterministic", repeatable, "repeat generation is pixel-identical")
	_, _, paramErr := Heatmap(fullB.Original, fullB.Final, 0)
	state.check("heatmap-parameter-validation", errs.CodeOf(paramErr) == errs.CodeParameter, "invalid normalization returns E_PARA")

	// Materialize the artifacts.
	if err := WriteArtifacts(filepath.Join(root, "full"), fullB, heat1); err != nil {
		return report, err
	}
	if err := WriteArtifacts(filepath.Join(root, "incremental"), incremental, heat1); err != nil {
		return report, err
	}
	if err := WriteArtifacts(filepath.Join(root, "resume"), resumed, heat1); err != nil {
		return report, err
	}
	retryHeat, _, err := Heatmap(retryResult.Original, retryResult.Final, retryParams.Heatmap.Normalization)
	if err != nil {
		return report, err
	}
	if err := WriteArtifacts(filepath.Join(root, "retry"), retryResult, retryHeat); err != nil {
		return report, err
	}
	for name, result := range map[string]*RunResult{
		"full":        fullB,
		"incremental": incremental,
		"resume":      resumed,
		"retry":       retryResult,
	} {
		if err := writeJSONFile(filepath.Join(root, name, "report.json"), result); err != nil {
			return report, err
		}
	}

	report.OK = true
	for _, check := range report.Checks {
		if !check.Passed {
			report.OK = false
			break
		}
	}
	if err := writeJSONFile(filepath.Join(root, "report.json"), report); err != nil {
		return report, err
	}
	if !report.OK {
		return report, &errs.Error{Code: errs.CodeVerify, Message: "one or more verification checks failed"}
	}
	return report, nil
}

func (s *verifyState) check(name string, passed bool, detail string) {
	s.report.Checks = append(s.report.Checks, VerifyCheck{Name: name, Passed: passed, Detail: detail})
}

func runFresh(ctx context.Context, dbPath string, image *yuv.Image, params filters.Params, limits filters.Limits, mode string) (*RunResult, error) {
	cleanPath(dbPath)
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	engine := NewEngine(store)
	result, err := engine.Run(ctx, image, params, limits, RunOptions{Mode: mode})
	engine.Close()
	return result, err
}

func cleanPath(base string) {
	_ = os.Remove(base)
	_ = os.Remove(base + "-wal")
	_ = os.Remove(base + "-shm")
}

func tamperParamVersion(dbPath string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE run_meta SET param_version='tampered-version' WHERE id=1`)
	return err
}

func equalCommitPrefix(prefix, full []StageCommit) bool {
	if len(prefix) > len(full) {
		return false
	}
	for index := range prefix {
		if prefix[index].NodeID != full[index].NodeID ||
			prefix[index].Seq != full[index].Seq ||
			prefix[index].CacheKey != full[index].CacheKey ||
			prefix[index].Summary != full[index].Summary {
			return false
		}
	}
	return true
}

func imagePixelsEqual(left, right *image.RGBA) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.Rect != right.Rect || len(left.Pix) != len(right.Pix) {
		return false
	}
	for index := range left.Pix {
		if left.Pix[index] != right.Pix[index] {
			return false
		}
	}
	return true
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
