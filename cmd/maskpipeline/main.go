package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/checkpoint"
	"maskpipeline/internal/engine"
	"maskpipeline/internal/model"
	"maskpipeline/internal/sample"
	"maskpipeline/internal/verify"
	versionstore "maskpipeline/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code := execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}
	switch args[0] {
	case "generate":
		if err := runGenerate(args[1:], stdout); err != nil {
			writeError(stderr, err)
			return 1
		}
	case "run":
		if err := runAcceptance(ctx, args[1:], stdout); err != nil {
			writeError(stderr, err)
			return 1
		}
	case "verify":
		if err := runVerify(ctx, args[1:], stdout); err != nil {
			writeError(stderr, err)
			return 1
		}
	case "help", "-h", "--help":
		printUsage(stdout)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `maskpipeline - deterministic mask segmentation preview and verification

Usage:
  go run ./cmd/maskpipeline generate [-out samples] [-seed 42]
  go run ./cmd/maskpipeline run [-samples samples] [-db artifacts/checkpoints.db]
  go run ./cmd/maskpipeline verify -db artifacts/checkpoints.db -sample samples/seed-42-384x256 -session <id>

All default paths are relative to the current workspace.`)
}

func runGenerate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	out := fs.String("out", "samples", "output directory")
	seed := fs.Int64("seed", 42, "deterministic sample seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	generated, err := sample.Generate(sample.GenerateOptions{Seed: *seed, OutDir: *out})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{
		"ok":      true,
		"seed":    *seed,
		"samples": generated,
	})
}

type acceptanceReport struct {
	GeneratedAt        string                    `json:"generated_at"`
	Sample             sampleSummary             `json:"sample"`
	Preview            *engine.PreviewResult     `json:"preview"`
	PreviewConsistency *engine.ConsistencyReport `json:"preview_consistency"`
	Paths              []pathSummary             `json:"paths"`
	ThreePathEqual     bool                      `json:"three_path_equal"`
	ThreePathChecks    map[string]bool           `json:"three_path_checks"`
	Verification       map[string]*verify.Report `json:"verification"`
	Exceptions         []exceptionCheck          `json:"exceptions"`
	BadBlockRetry      *verify.Report            `json:"bad_block_retry_verification,omitempty"`
	Checks             []checkResult             `json:"checks"`
	OverallOK          bool                      `json:"overall_ok"`
}

type sampleSummary struct {
	SampleID          string `json:"sample_id"`
	Dir               string `json:"dir"`
	Seed              int64  `json:"seed"`
	Width             int    `json:"width"`
	Height            int    `json:"height"`
	Channels          int    `json:"channels"`
	TileSize          int    `json:"tile_size"`
	PreviewDeadlineMS int    `json:"preview_deadline_ms"`
	TruthFingerprint  string `json:"truth_fingerprint"`
	Regions           int    `json:"regions"`
}

type pathSummary struct {
	SessionID        string      `json:"session_id"`
	Mode             string      `json:"mode"`
	Status           string      `json:"status"`
	ParamVersion     uint64      `json:"param_version"`
	MaskHash         string      `json:"mask_hash"`
	StatsHash        string      `json:"stats_hash"`
	Stats            model.Stats `json:"stats"`
	CommitSequence   []string    `json:"commit_sequence"`
	CommittedNew     int         `json:"committed_new"`
	Invalidated      bool        `json:"invalidated"`
	InvalidateReason string      `json:"invalidate_reason,omitempty"`
	PausedRun        bool        `json:"paused_run"`
}

type exceptionCheck struct {
	Case     string         `json:"case"`
	Expected string         `json:"expected"`
	Actual   string         `json:"actual"`
	Passed   bool           `json:"passed"`
	Error    any            `json:"error,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

type checkResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

func runAcceptance(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	samplesDir := fs.String("samples", "samples", "sample root directory")
	sampleDir := fs.String("sample", "", "specific sample directory")
	dbPath := fs.String("db", "artifacts/checkpoints.db", "SQLite checkpoint path")
	reportPath := fs.String("report", "artifacts/acceptance.json", "acceptance report path")
	prefix := fs.String("session-prefix", "acceptance", "session id prefix")
	maxParallel := fs.Int("max-parallel", 4, "maximum worker parallelism")
	buffer := fs.Int("per-block-buffer", 2, "bounded per-block result buffer")
	tolerance := fs.Float64("tolerance", -1, "override color tolerance; <0 uses manifest default")
	gradient := fs.Float64("gradient", -1, "override gradient threshold; <0 uses manifest default")
	se := fs.Int("se", 0, "override structure element size; 0 uses manifest default")
	pauseAfter := fs.Int("pause-after", 6, "pause after this many new stage commits; 0 disables")
	fresh := fs.Bool("fresh", true, "delete acceptance sessions before running")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir := *sampleDir
	if dir == "" {
		if !hasAnySample(*samplesDir) {
			if _, err := sample.Generate(sample.GenerateOptions{Seed: 42, OutDir: *samplesDir}); err != nil {
				return err
			}
		}
		var err error
		dir, err = firstSampleDir(*samplesDir)
		if err != nil {
			return err
		}
	}
	smp, err := sample.Load(dir)
	if err != nil {
		return err
	}
	params := smp.Manifest.DefaultsWithSeeds()
	if *tolerance >= 0 {
		params.ColorTolerance = *tolerance
	}
	if *gradient >= 0 {
		params.GradientThreshold = *gradient
	}
	if *se > 0 {
		params.StructureElementSize = *se
	}
	versions, initial, err := versionstore.New(smp.Manifest.Width, smp.Manifest.Height, smp.Manifest.AlgorithmVersion, params)
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	sessionIDs := map[string]string{
		"incremental": *prefix + "-incremental",
		"resume":      *prefix + "-resume",
		"full":        *prefix + "-full",
		"bad":         *prefix + "-bad-block",
	}
	if *fresh {
		for _, id := range sessionIDs {
			if err := store.ResetSession(ctx, id); err != nil {
				return err
			}
		}
	}
	opts := engine.DefaultExecutorOptions()
	opts.MaxParallel = *maxParallel
	opts.PerBlockBuffer = *buffer
	eng, err := engine.New(store, versions, opts)
	if err != nil {
		return err
	}
	report := &acceptanceReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Sample: sampleSummary{
			SampleID: smp.Manifest.SampleID, Dir: smp.Dir, Seed: smp.Manifest.Seed,
			Width: smp.Manifest.Width, Height: smp.Manifest.Height, Channels: smp.Manifest.Channels,
			TileSize: smp.Manifest.TileSize, PreviewDeadlineMS: smp.Manifest.PreviewDeadlineMS,
			TruthFingerprint: smp.TruthFingerprint, Regions: smp.Manifest.Truth.RegionCount,
		},
		ThreePathChecks: make(map[string]bool),
		Verification:    make(map[string]*verify.Report),
	}
	addCheck := func(name string, passed bool, detail string) {
		report.Checks = append(report.Checks, checkResult{Name: name, Passed: passed, Detail: detail})
	}

	preview, previewErr := eng.Preview(ctx, smp)
	report.Preview = preview
	addCheck("preview_within_deadline", previewErr == nil, detailOf(previewErr))

	inc, err := eng.Run(ctx, smp, engine.RunRequest{
		SessionID: sessionIDs["incremental"], Mode: checkpoint.ModeIncremental,
		SnapshotVersion: initial.Version,
	})
	if err != nil {
		return fmt.Errorf("incremental run: %w", err)
	}
	report.Paths = append(report.Paths, summarizePath("incremental", inc))
	if preview != nil {
		consistency, cmpErr := engine.Compare(preview, inc, smp.Manifest.Tolerances)
		report.PreviewConsistency = consistency
		addCheck("preview_exact_consistency", cmpErr == nil, detailOf(cmpErr))
	} else {
		addCheck("preview_exact_consistency", false, "preview unavailable")
	}

	resumeReq := engine.RunRequest{
		SessionID: sessionIDs["resume"], Mode: checkpoint.ModeResume,
		SnapshotVersion: initial.Version, PauseAfterCommits: *pauseAfter,
	}
	resumeFirst, err := eng.Run(ctx, smp, resumeReq)
	if err != nil {
		return fmt.Errorf("resume first run: %w", err)
	}
	report.Paths = append(report.Paths, summarizePath("resume-first", resumeFirst))
	addCheck("checkpoint_pause_reached", resumeFirst.Paused || *pauseAfter == 0,
		fmt.Sprintf("status=%s commits=%d", resumeFirst.Status, resumeFirst.CommittedNew))
	resumeFinal := resumeFirst
	if resumeFirst.Paused {
		resumeReq.PauseAfterCommits = 0
		resumeFinal, err = eng.Run(ctx, smp, resumeReq)
		if err != nil {
			return fmt.Errorf("resume final run: %w", err)
		}
		report.Paths = append(report.Paths, summarizePath("resume-final", resumeFinal))
		addCheck("checkpoint_resume_completed", resumeFinal.Status == "completed", resumeFinal.Status)
	}

	full, err := eng.Run(ctx, smp, engine.RunRequest{
		SessionID: sessionIDs["full"], Mode: checkpoint.ModeFull,
		SnapshotVersion: initial.Version,
	})
	if err != nil {
		return fmt.Errorf("full run: %w", err)
	}
	report.Paths = append(report.Paths, summarizePath("full", full))

	report.ThreePathChecks["mask_pixels_equal"] = bytes.Equal(inc.Labels, resumeFinal.Labels) && bytes.Equal(inc.Labels, full.Labels)
	report.ThreePathChecks["mask_hashes_equal"] = inc.MaskHash == resumeFinal.MaskHash && inc.MaskHash == full.MaskHash
	report.ThreePathChecks["stats_equal"] = model.StatsHash(inc.Stats) == model.StatsHash(resumeFinal.Stats) &&
		model.StatsHash(inc.Stats) == model.StatsHash(full.Stats)
	report.ThreePathChecks["stats_hashes_equal"] = inc.StatsHash == resumeFinal.StatsHash && inc.StatsHash == full.StatsHash
	report.ThreePathChecks["commit_sequences_equal"] = strings.Join(inc.CommitSequence, ",") == strings.Join(resumeFinal.CommitSequence, ",") &&
		strings.Join(inc.CommitSequence, ",") == strings.Join(full.CommitSequence, ",")
	report.ThreePathEqual = allTrue(report.ThreePathChecks)
	addCheck("three_paths_pixel_identical", report.ThreePathEqual, "")

	for name, sessionID := range map[string]string{
		"incremental": sessionIDs["incremental"],
		"resume":      sessionIDs["resume"],
		"full":        sessionIDs["full"],
	} {
		vr, err := verify.Run(ctx, store, smp.Dir, sessionID)
		if err != nil {
			addCheck("verify_"+name, false, detailOf(err))
			continue
		}
		report.Verification[name] = vr
		addCheck("verify_"+name, true, "")
	}

	report.Exceptions = append(report.Exceptions, runExceptionChecks(ctx, store, versions, smp, eng, sessionIDs)...)
	badRetry, badChecks := runBadBlockCheck(ctx, store, versions, smp, opts, sessionIDs["bad"], initial.Version)
	report.Exceptions = append(report.Exceptions, badChecks...)
	report.BadBlockRetry = badRetry
	if badRetry != nil {
		addCheck("bad_block_retry_verified", true, badRetry.SessionID)
	} else {
		addCheck("bad_block_retry_verified", false, "retry did not produce a verification report")
	}

	report.OverallOK = allChecksPassed(report)
	if err := writeFileJSON(*reportPath, report); err != nil {
		return err
	}
	summary := map[string]any{
		"ok":                  report.OverallOK,
		"report":              filepath.ToSlash(*reportPath),
		"sample":              report.Sample.SampleID,
		"preview_latency_ms":  report.PreviewLatency(),
		"preview_deadline_ms": report.Sample.PreviewDeadlineMS,
		"three_path_equal":    report.ThreePathEqual,
		"checks":              report.Checks,
		"exceptions":          report.Exceptions,
	}
	if err := writeJSON(stdout, summary); err != nil {
		return err
	}
	if !report.OverallOK {
		return apperr.New(apperr.CodeInternal, "acceptance", "one or more acceptance checks failed")
	}
	return nil
}

func (r *acceptanceReport) PreviewLatency() float64 {
	if r.Preview == nil {
		return 0
	}
	return r.Preview.Meta.LatencyMS
}

func runExceptionChecks(ctx context.Context, store *checkpoint.Store, versions *versionstore.Store, smp *sample.Sample, eng *engine.Engine, sessionIDs map[string]string) []exceptionCheck {
	var checks []exceptionCheck
	checks = append(checks, expectError("seed_in_background", apperr.CodeSeed, func() error {
		params := smp.Manifest.DefaultsWithSeeds()
		params.Seeds = []model.Point{{X: 0, Y: 0}}
		if _, err := versions.Update(params); err != nil {
			return err
		}
		_, err := eng.Preview(ctx, smp)
		return err
	}))
	checks = append(checks, expectError("region_spill", apperr.CodeSpill, func() error {
		params := smp.Manifest.DefaultsWithSeeds()
		params.ColorTolerance = 441.6729559300637
		params.GradientThreshold = 255
		if _, err := versions.Update(params); err != nil {
			return err
		}
		_, err := eng.Preview(ctx, smp)
		return err
	}))
	checks = append(checks, expectError("parameter_out_of_range", apperr.CodePara, func() error {
		params := smp.Manifest.DefaultsWithSeeds()
		params.StructureElementSize = 4
		_, err := versions.Update(params)
		return err
	}))
	checks = append(checks, expectError("version_mixing", apperr.CodeVersion, func() error {
		_, err := store.Commit(ctx, checkpoint.CommitInput{
			SessionID: sessionIDs["incremental"], TileIndex: 0, Stage: model.StageGrowth,
			Output: []byte{1}, ParamVersion: 2, AlgorithmVersion: smp.Manifest.AlgorithmVersion,
		})
		return err
	}))
	checks = append(checks, expectError("illegal_state_transition", apperr.CodeState, func() error {
		return store.Transition(ctx, sessionIDs["incremental"], 0, model.StageGrowth,
			model.StateInProgress, 1, smp.Manifest.AlgorithmVersion)
	}))
	statePreserved := false
	if st, ok, err := store.GetStage(ctx, sessionIDs["incremental"], 0, model.StageGrowth); err == nil && ok {
		statePreserved = st.State == model.StateCommitted
	}
	checks = append(checks, exceptionCheck{
		Case: "illegal_transition_preserves_state", Expected: string(model.StateCommitted),
		Actual: boolToState(statePreserved, model.StateCommitted), Passed: statePreserved,
	})
	return checks
}

func runBadBlockCheck(ctx context.Context, store *checkpoint.Store, versions *versionstore.Store, smp *sample.Sample, opts engine.ExecutorOptions, sessionID string, paramVersion uint64) (*verify.Report, []exceptionCheck) {
	var checks []exceptionCheck
	badOpts := opts
	badOpts.InjectBadTile = 1
	badOpts.InjectBadStage = model.StageGrowth
	badEngine, err := engine.New(store, versions, badOpts)
	if err != nil {
		return nil, []exceptionCheck{{Case: "bad_block_injection", Expected: apperr.CodeBlock, Actual: apperr.Code(err), Passed: false, Error: toJSONError(err)}}
	}
	badResult, badErr := badEngine.Run(ctx, smp, engine.RunRequest{
		SessionID: sessionID, Mode: checkpoint.ModeFull, SnapshotVersion: paramVersion,
	})
	actual := apperr.Code(badErr)
	passed := actual == apperr.CodeBlock && badResult != nil && badResult.Status == "failed"
	checks = append(checks, exceptionCheck{
		Case: "bad_block_isolated", Expected: apperr.CodeBlock, Actual: actual, Passed: passed,
		Error: toJSONError(badErr),
	})
	if badResult != nil {
		checks[len(checks)-1].Details = map[string]any{
			"committed_new": badResult.CommittedNew,
			"status":        badResult.Status,
		}
	}
	stages, err := store.AllStages(ctx, sessionID)
	if err != nil {
		checks = append(checks, exceptionCheck{Case: "bad_block_state_inspection", Passed: false, Error: toJSONError(err)})
		return nil, checks
	}
	counts := map[string]int{"pending": 0, "in_progress": 0, "committed": 0, "failed": 0}
	failedTile := -1
	otherTilePolluted := false
	for _, st := range stages {
		counts[string(st.State)]++
		if st.State == model.StateFailed {
			failedTile = st.TileIndex
		}
		if failedTile >= 0 && st.TileIndex != failedTile && st.State != model.StateCommitted {
			otherTilePolluted = true
		}
	}
	isolated := failedTile == 1 && counts["failed"] == 1 && counts["pending"] == 2 &&
		counts["committed"] == len(stages)-3 && !otherTilePolluted
	checks = append(checks, exceptionCheck{
		Case: "bad_block_does_not_pollute_other_tiles", Expected: "failed_tile=1,other_tiles=committed",
		Actual: fmt.Sprintf("failed_tile=%d,committed=%d,pending=%d", failedTile, counts["committed"], counts["pending"]),
		Passed: isolated, Details: map[string]any{"states": counts, "other_tile_polluted": otherTilePolluted},
	})
	retryOpts := opts
	retryOpts.InjectBadTile = -1
	retryEngine, err := engine.New(store, versions, retryOpts)
	if err != nil {
		checks = append(checks, exceptionCheck{Case: "bad_block_retry", Passed: false, Error: toJSONError(err)})
		return nil, checks
	}
	retry, retryErr := retryEngine.Run(ctx, smp, engine.RunRequest{
		SessionID: sessionID, Mode: checkpoint.ModeResume, SnapshotVersion: paramVersion,
	})
	checks = append(checks, exceptionCheck{
		Case: "bad_block_retry_completed", Expected: "completed", Actual: statusOf(retry),
		Passed: retryErr == nil && retry != nil && retry.Status == "completed", Error: toJSONError(retryErr),
	})
	if retryErr != nil || retry == nil {
		return nil, checks
	}
	report, verifyErr := verify.Run(ctx, store, smp.Dir, sessionID)
	checks = append(checks, exceptionCheck{
		Case: "bad_block_retry_verified", Expected: "verified", Actual: boolString(verifyErr == nil),
		Passed: verifyErr == nil, Error: toJSONError(verifyErr),
	})
	return report, checks
}

func expectError(name, expected string, fn func() error) exceptionCheck {
	err := fn()
	actual := apperr.Code(err)
	return exceptionCheck{Case: name, Expected: expected, Actual: actual, Passed: actual == expected, Error: toJSONError(err)}
}

func runVerify(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbPath := fs.String("db", "artifacts/checkpoints.db", "SQLite checkpoint path")
	sampleDir := fs.String("sample", "", "sample directory containing sample_manifest.json")
	sessionID := fs.String("session", "", "checkpoint session id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sampleDir == "" || *sessionID == "" {
		return apperr.New(apperr.CodePara, "verify", "sample and session are required")
	}
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	report, err := verify.Run(ctx, store, *sampleDir, *sessionID)
	if err != nil {
		return err
	}
	return writeJSON(stdout, report)
}

func firstSampleDir(root string) (string, error) {
	if _, err := os.Stat(filepath.Join(root, sample.ManifestName)); err == nil {
		return root, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeIO, "find_sample", "read sample root", err)
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, sample.ManifestName)); err == nil {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		return "", apperr.New(apperr.CodeDecode, "find_sample", "no sample manifest found under root").
			WithField("manifest").WithDetail("root", filepath.ToSlash(root))
	}
	return dirs[0], nil
}

func hasAnySample(root string) bool {
	if _, err := os.Stat(filepath.Join(root, sample.ManifestName)); err == nil {
		return true
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, entry.Name(), sample.ManifestName)); err == nil {
			return true
		}
	}
	return false
}

func summarizePath(mode string, r *engine.RunResult) pathSummary {
	if r == nil {
		return pathSummary{Mode: mode}
	}
	return pathSummary{
		SessionID: r.SessionID, Mode: mode, Status: r.Status, ParamVersion: r.ParamVersion,
		MaskHash: r.MaskHash, StatsHash: r.StatsHash, Stats: r.Stats,
		CommitSequence: r.CommitSequence, CommittedNew: r.CommittedNew,
		Invalidated: r.Invalidated, InvalidateReason: r.InvalidateReason, PausedRun: r.Paused,
	}
}

func allTrue(m map[string]bool) bool {
	if len(m) == 0 {
		return false
	}
	for _, v := range m {
		if !v {
			return false
		}
	}
	return true
}

func allChecksPassed(r *acceptanceReport) bool {
	for _, c := range r.Checks {
		if !c.Passed {
			return false
		}
	}
	for _, e := range r.Exceptions {
		if !e.Passed {
			return false
		}
	}
	return true
}

func writeFileJSON(path string, value any) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return apperr.Wrap(apperr.CodeIO, "write_report", "write JSON report", err)
	}
	return nil
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func writeError(w io.Writer, err error) {
	_ = writeJSON(w, map[string]any{"ok": false, "error": toJSONError(err)})
}

func toJSONError(err error) any {
	if err == nil {
		return nil
	}
	if e, ok := apperr.As(err); ok {
		return e
	}
	return map[string]any{"code": apperr.CodeInternal, "message": err.Error()}
}

func detailOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func statusOf(r *engine.RunResult) string {
	if r == nil {
		return ""
	}
	return r.Status
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func boolToState(v bool, state model.State) string {
	if v {
		return string(state)
	}
	return "changed"
}
