package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"atelier/internal/apperr"
	"atelier/internal/checkpoint"
	"atelier/internal/engine"
	"atelier/internal/model"
	"atelier/internal/sample"
	"atelier/internal/scenarioio"
	"atelier/internal/validate"
)

type checkResult struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Detail   string `json:"detail"`
	Duration string `json:"duration"`
}

type zoneMeasurement struct {
	ZoneID        string  `json:"zoneId"`
	TruthPoints   int     `json:"truthPoints"`
	RebuiltPoints int     `json:"rebuiltPoints"`
	PointDelta    int     `json:"pointDelta"`
	DensityError  float64 `json:"densityError"`
	RarityL1      float64 `json:"rarityL1"`
}

type verificationReport struct {
	Passed        bool              `json:"passed"`
	GeneratedAt   string            `json:"generatedAt"`
	Seed          int64             `json:"seed"`
	RegionID      string            `json:"regionId"`
	RegionName    string            `json:"regionName"`
	ZoneCount     int               `json:"zoneCount"`
	Algorithm     string            `json:"algorithm"`
	Checks        []checkResult     `json:"checks"`
	Zones         []zoneMeasurement `json:"zones"`
	TruthedPoints int               `json:"truthPoints"`
	RebuiltPoints int               `json:"rebuiltPoints"`
	Heatmap       map[string]any    `json:"heatmap"`
	Incremental   map[string]any    `json:"incremental"`
	Checkpoints   map[string]any    `json:"checkpoints"`
	Artifacts     map[string]string `json:"artifacts"`
}

func main() {
	_ = flag.Bool("verify", true, "run the deterministic acceptance suite")
	seed := flag.Int64("seed", sample.DefaultSeed, "deterministic sample seed")
	out := flag.String("out", "artifacts/verification.json", "verification report path")
	sampleOut := flag.String("sample-out", "", "generated scenario JSON path (default artifacts/scenario-seed-<seed>.json)")
	stateDir := flag.String("state-dir", "artifacts/verification-state", "temporary checkpoint state directory")
	flag.Parse()
	if *sampleOut == "" {
		*sampleOut = fmt.Sprintf("artifacts/scenario-seed-%d.json", *seed)
	}

	started := time.Now()
	report := verify(*seed, *sampleOut, *stateDir)
	report.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	report.Artifacts["report"] = *out
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "create report directory:", err)
		os.Exit(1)
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal report:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
		os.Exit(1)
	}
	printReport(report, time.Since(started))
	if !report.Passed {
		os.Exit(1)
	}
}

func verify(seed int64, sampleOut, stateDir string) verificationReport {
	ctx := context.Background()
	report := verificationReport{
		Seed: seed, Algorithm: model.AlgorithmVersion,
		Checks: []checkResult{}, Artifacts: map[string]string{},
		Heatmap: map[string]any{}, Incremental: map[string]any{}, Checkpoints: map[string]any{},
	}
	scenario := sample.Generate(seed)
	report.TruthedPoints = len(scenario.Truth.Points)
	report.RegionID = scenario.Region.ID
	report.RegionName = scenario.Region.Name
	report.ZoneCount = len(scenario.Region.Zones)
	primaryZone := firstZoneID(scenario)
	primaryExclusion := scenario.Region.Exclusions[0].ID

	addCheck(&report, "schema-and-geometry-validation", func() error {
		return validate.Scenario(&scenario)
	}, "local generator output validated")

	if err := os.MkdirAll(filepath.Dir(sampleOut), 0o755); err == nil {
		if err := scenarioio.Write(sampleOut, scenario); err == nil {
			if _, readErr := scenarioio.Read(sampleOut); readErr == nil {
				report.Artifacts["scenario"] = sampleOut
			}
		}
	}

	eng, err := engine.New(scenario)
	if err != nil {
		addFailed(&report, "engine-initialization", err)
		return report
	}
	snap, err := eng.FullRebuild(ctx)
	if err != nil {
		addFailed(&report, "full-rebuild", err)
		return report
	}
	report.RebuiltPoints = len(snap.Points)
	report.Heatmap = map[string]any{
		"width": snap.Heatmap.Width, "height": snap.Heatmap.Height,
		"cellSize": snap.Heatmap.CellSize, "pixels": len(snap.Heatmap.Values),
		"tileSize": snap.Heatmap.TileSize, "reusedPixels": snap.Invalidation.ReusedPixels,
		"recomputedPixels": snap.Invalidation.RecomputedPixels,
		"workerMetrics":    snap.Metrics,
	}
	addBool(&report, "worker-resource-boundaries",
		snap.Metrics.MaxParallelism == scenario.Config.MaxParallelism &&
			snap.Metrics.PerWorkerBufferSize == scenario.Config.PerWorkerBufferSize &&
			snap.Metrics.MaxParallelism > 0 && snap.Metrics.PerWorkerBufferSize > 0,
		fmt.Sprintf("maxParallelism=%d perWorkerBuffer=%d tasks=%d blocked=%d",
			snap.Metrics.MaxParallelism, snap.Metrics.PerWorkerBufferSize, snap.Metrics.Tasks, snap.Metrics.BufferBlockEvents))

	addCheck(&report, "reconstruction-count-tolerance", func() error {
		limit := math.Max(engine.ReconCountAbsTol, engine.ReconCountRelTol*float64(len(scenario.Truth.Points)))
		if diff := math.Abs(float64(len(snap.Points) - len(scenario.Truth.Points))); diff > limit {
			return fmt.Errorf("rebuilt=%d truth=%d diff=%.0f limit=%.0f", len(snap.Points), len(scenario.Truth.Points), diff, limit)
		}
		return nil
	}, fmt.Sprintf("rebuilt=%d truth=%d", len(snap.Points), len(scenario.Truth.Points)))

	truthByZone := map[string][]model.TruthPoint{}
	for _, point := range scenario.Truth.Points {
		truthByZone[point.ZoneID] = append(truthByZone[point.ZoneID], point)
	}
	var densityPass, rarityPass = true, true
	for _, zoneState := range snap.ZoneStates {
		truth := truthByZone[zoneState.ZoneID]
		actualDensity := float64(len(truth)) / zoneState.Estimate.EffectiveArea
		densityError := math.Abs(zoneState.Estimate.Density - actualDensity)
		rarityL1 := l1(zoneState.Estimate.Rarity, truthRarity(truth))
		if densityError > engine.DensityAbsTolerance {
			densityPass = false
		}
		if rarityL1 > engine.RarityL1Tolerance {
			rarityPass = false
		}
		report.Zones = append(report.Zones, zoneMeasurement{
			ZoneID: zoneState.ZoneID, TruthPoints: len(truth),
			RebuiltPoints: len(zoneState.Points),
			PointDelta:    len(zoneState.Points) - len(truth),
			DensityError:  densityError, RarityL1: rarityL1,
		})
	}
	addBool(&report, "zone-density-tolerance", densityPass,
		fmt.Sprintf("declared absolute tolerance %.3f", engine.DensityAbsTolerance))
	addBool(&report, "rarity-mixture-tolerance", rarityPass,
		fmt.Sprintf("declared L1 tolerance %.3f", engine.RarityL1Tolerance))

	maxHeatError := 0.0
	for _, obs := range scenario.Observations {
		value, ok := snap.Heatmap.ValueAtXY(obs.X, obs.Y)
		if !ok {
			maxHeatError = math.Inf(1)
			break
		}
		maxHeatError = math.Max(maxHeatError, math.Abs(value-obs.Density))
	}
	addBool(&report, "heatmap-at-observed-grid", maxHeatError <= engine.HeatAbsTolerance,
		fmt.Sprintf("max abs error %.6f, tolerance %.4f", maxHeatError, engine.HeatAbsTolerance))
	report.Heatmap["maxObservedGridError"] = maxHeatError

	secondEngine, err := engine.New(scenario)
	if err == nil {
		second, buildErr := secondEngine.FullRebuild(ctx)
		if buildErr == nil {
			addBool(&report, "deterministic-rebuild", sameResult(snap, second),
				"identical points, estimates, heatmap values and cache-key sequence")
		} else {
			addFailed(&report, "deterministic-rebuild", buildErr)
		}
	} else {
		addFailed(&report, "deterministic-rebuild", err)
	}

	updated, err := eng.UpdateZoneParams(ctx, primaryZone, model.ZoneParams{DensityScale: 1.07, RarityShift: 0})
	if err != nil {
		addFailed(&report, "incremental-zone-edit", err)
	} else {
		inv := updated.Invalidation
		report.Incremental["densityEdit"] = map[string]any{
			"affectedZones": inv.AffectedZones,
			"reusedZones":   inv.ReusedZoneCount, "reusedPixels": inv.ReusedPixels,
			"recomputedPixels": inv.RecomputedPixels,
			"affectedTiles":    inv.AffectedTileCount, "reusedTiles": inv.ReusedTileCount,
		}
		addBool(&report, "density-edit-reuses-sampling", !inv.AffectedZones[0].Resample,
			"sampling key unchanged; only estimate/reconstruction/affected heat pixels rerun")
		compare, _, compareErr := eng.CompareWithFull(ctx)
		addBool(&report, "incremental-equals-full-after-zone-edit", compareErr == nil && compare.Equal,
			compareDetail(compare, compareErr))
	}

	rarityOnly, err := eng.UpdateZoneParams(ctx, primaryZone, model.ZoneParams{DensityScale: 1.07, RarityShift: 0.12})
	if err != nil {
		addFailed(&report, "rarity-only-edit", err)
	} else {
		report.Incremental["rarityEdit"] = map[string]any{
			"recomputedPixels": rarityOnly.Invalidation.RecomputedPixels,
			"reusedPixels":     rarityOnly.Invalidation.ReusedPixels,
			"heatInvalidated":  rarityOnly.Invalidation.AffectedZones[0].Heatmap,
		}
		addBool(&report, "rarity-edit-reuses-heatmap", rarityOnly.Invalidation.RecomputedPixels == 0,
			"rarity does not affect density pixels; all heatmap pixels reused")
	}

	moved, err := eng.MoveExclusion(ctx, primaryExclusion, 2, 3)
	if err != nil {
		addFailed(&report, "exclusion-move", err)
	} else {
		report.Incremental["exclusionMove"] = map[string]any{
			"affectedZones":    moved.Invalidation.AffectedZones,
			"reusedZones":      moved.Invalidation.ReusedZoneCount,
			"reusedPixels":     moved.Invalidation.ReusedPixels,
			"recomputedPixels": moved.Invalidation.RecomputedPixels,
			"manifestEntries":  len(moved.Invalidation.Entries),
		}
		addBool(&report, "exclusion-precise-propagation", moved.Invalidation.ReusedZoneCount > 0 && moved.Invalidation.ReusedPixels > 0,
			fmt.Sprintf("affected %d zones, reused %d zones and %d pixels",
				len(moved.Invalidation.AffectedZones), moved.Invalidation.ReusedZoneCount, moved.Invalidation.ReusedPixels))
		compare, _, compareErr := eng.CompareWithFull(ctx)
		addBool(&report, "incremental-equals-full-after-exclusion", compareErr == nil && compare.Equal,
			compareDetail(compare, compareErr))
	}

	broken := snap
	broken.Heatmap = cloneHeatmap(snap.Heatmap)
	broken.Heatmap.Values[123] += 0.5
	_, diffErr := engine.CompareSnapshots(broken, snap)
	var ae *apperr.Error
	diffLocated := errors.As(diffErr, &ae) && ae.Code == apperr.CodeDiff && ae.PixelX == 3 && ae.PixelY == 2
	detail := "expected E_DIFF at pixel (3,2)"
	if ae != nil {
		detail = fmt.Sprintf("E_DIFF points to pixel (%d,%d)", ae.PixelX, ae.PixelY)
	}
	addBool(&report, "first-difference-locator", diffLocated, detail)

	verifyCheckpoints(ctx, &report, scenario, snap, stateDir)

	report.Passed = true
	for _, check := range report.Checks {
		if !check.Passed {
			report.Passed = false
			break
		}
	}
	return report
}

func verifyCheckpoints(ctx context.Context, report *verificationReport, scenario model.Scenario, baseline engine.Snapshot, stateDir string) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		addFailed(report, "checkpoint-directory", err)
		return
	}
	dbPath := filepath.Join(stateDir, "checkpoints.db")
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		addFailed(report, "checkpoint-open", err)
		return
	}
	checkEng, err := engine.New(scenario)
	if err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-engine", err)
		return
	}
	checkEng.SetCheckpointSink(store)
	if _, err := checkEng.FullRebuild(ctx); err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-full-rebuild", err)
		return
	}
	request := checkpoint.VerifyRequest{
		ExpectedScenarioHash:  checkEng.ScenarioHash(),
		ExpectedAlgorithm:     model.AlgorithmVersion,
		ExpectedParamsVersion: checkEng.ExpectedParamsVersions(),
		ExpectedZoneOrder:     checkEng.ZoneOrder(),
	}
	completed, err := store.LoadLatest(ctx, request)
	if err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-load-complete", err)
		return
	}
	addBool(report, "checkpoint-complete-snapshot", completed.Valid && completed.RecoverySource == checkpoint.SourceCompleted,
		fmt.Sprintf("source=%s zones=%d", completed.RecoverySource, len(completed.Zones)))
	_ = store.Close()

	// Forced termination: commit a two-zone prefix and leave the session running.
	store, err = checkpoint.Open(dbPath)
	if err != nil {
		addFailed(report, "checkpoint-reopen", err)
		return
	}
	sessionID, err := store.BeginSession(ctx, checkEng.ScenarioHash(), checkEng.ZoneOrder())
	if err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-forced-begin", err)
		return
	}
	for i := 0; i < 2; i++ {
		if err := store.SaveZone(ctx, sessionID, baseline.ZoneStates[i]); err != nil {
			_ = store.Close()
			addFailed(report, "checkpoint-forced-save", err)
			return
		}
	}
	forcedRows, _ := store.CheckpointRows(ctx, sessionID)
	_ = store.Close()

	store, err = checkpoint.Open(dbPath)
	if err != nil {
		addFailed(report, "checkpoint-forced-reopen", err)
		return
	}
	forced, err := store.LoadLatest(ctx, request)
	if err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-forced-load", err)
		return
	}
	addBool(report, "checkpoint-forced-recovery-source", forced.Valid &&
		forced.RecoverySource == checkpoint.SourceForcedTermination && len(forced.Zones) == 2,
		fmt.Sprintf("source=%s resumedZones=%d committedRows=%d valid=%v reason=%s",
			forced.RecoverySource, len(forced.Zones), len(forcedRows), forced.Valid, forced.DiscardReason))
	resumeEng, err := engine.New(scenario)
	if err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-resume-engine", err)
		return
	}
	resumeEng.SetCheckpointSink(store)
	resumed, err := resumeEng.ResumeFromCheckpoint(ctx, forced.Zones, engine.RecoveryReport{
		Source: forced.RecoverySource, SnapshotStatus: forced.Status,
		ResumedZones: zoneIDs(forced.Zones), RecoveredFromStore: true,
	})
	if err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-resume", err)
		return
	}
	compare, _, compareErr := resumeEng.CompareWithFull(ctx)
	addBool(report, "checkpoint-resume-equals-full", compareErr == nil && compare.Equal,
		fmt.Sprintf("resumed %d zones; %s", len(forced.Zones), compareDetail(compare, compareErr)))
	report.Checkpoints["forcedTermination"] = map[string]any{
		"source": forced.RecoverySource, "status": forced.Status,
		"resumedZones": zoneIDs(forced.Zones), "revision": resumed.Revision,
		"committedRows": len(forcedRows), "valid": forced.Valid,
		"discardReason":         forced.DiscardReason,
		"baselineParamsVersion": baseline.ZoneStates[0].ParamsVersion,
		"expectedParamsVersion": request.ExpectedParamsVersion[baseline.ZoneStates[0].ZoneID],
	}
	_ = store.Close()

	// Active cancel: explicit cancelled status must be distinguished from forced termination.
	store, err = checkpoint.Open(dbPath)
	if err != nil {
		addFailed(report, "checkpoint-cancel-open", err)
		return
	}
	cancelSession, err := store.BeginSession(ctx, checkEng.ScenarioHash(), checkEng.ZoneOrder())
	if err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-cancel-begin", err)
		return
	}
	if err := store.SaveZone(ctx, cancelSession, baseline.ZoneStates[0]); err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-cancel-save", err)
		return
	}
	if err := store.FinishSession(ctx, cancelSession, checkpoint.StatusCancelled); err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-cancel-finish", err)
		return
	}
	_ = store.Close()
	store, err = checkpoint.Open(dbPath)
	if err != nil {
		addFailed(report, "checkpoint-cancel-reopen", err)
		return
	}
	cancelled, err := store.LoadLatest(ctx, request)
	if err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-cancel-load", err)
		return
	}
	addBool(report, "checkpoint-active-cancel-source", cancelled.Valid &&
		cancelled.RecoverySource == checkpoint.SourceActiveCancel,
		fmt.Sprintf("source=%s status=%s zones=%d valid=%v reason=%s",
			cancelled.RecoverySource, cancelled.Status, len(cancelled.Zones), cancelled.Valid, cancelled.DiscardReason))
	report.Checkpoints["activeCancel"] = map[string]any{
		"source": cancelled.RecoverySource, "status": cancelled.Status, "zones": len(cancelled.Zones),
	}
	_ = store.Close()

	// Any parameter-version mismatch must invalidate the whole snapshot.
	badRequest := request
	badRequest.ExpectedParamsVersion = cloneStringMap(request.ExpectedParamsVersion)
	firstZone := checkEng.ZoneOrder()[0]
	badRequest.ExpectedParamsVersion[firstZone] = "tampered-version"
	store, err = checkpoint.Open(dbPath)
	if err != nil {
		addFailed(report, "checkpoint-discard-open", err)
		return
	}
	discarded, err := store.LoadLatest(ctx, badRequest)
	if err != nil {
		_ = store.Close()
		addFailed(report, "checkpoint-discard-load", err)
		return
	}
	rows, _ := store.CheckpointRows(ctx, cancelled.SessionID)
	addBool(report, "checkpoint-mismatch-invalidates-all", !discarded.Valid && discarded.Discarded && len(discarded.Zones) == 0 && len(rows) == 0,
		fmt.Sprintf("discarded=%v reason=%s remainingRows=%d", discarded.Discarded, discarded.DiscardReason, len(rows)))
	report.Checkpoints["mismatchDiscard"] = map[string]any{
		"discarded": discarded.Discarded, "reason": discarded.DiscardReason,
		"remainingRows": len(rows),
	}
	_ = store.Close()
	report.Artifacts["checkpointDatabase"] = dbPath
}

func addCheck(report *verificationReport, name string, fn func() error, detail string) {
	start := time.Now()
	err := fn()
	check := checkResult{Name: name, Passed: err == nil, Detail: detail, Duration: time.Since(start).String()}
	if err != nil {
		check.Detail = err.Error()
	}
	report.Checks = append(report.Checks, check)
}

func addBool(report *verificationReport, name string, passed bool, detail string) {
	report.Checks = append(report.Checks, checkResult{Name: name, Passed: passed, Detail: detail})
}

func addFailed(report *verificationReport, name string, err error) {
	report.Checks = append(report.Checks, checkResult{Name: name, Passed: false, Detail: err.Error()})
}

func sameResult(a, b engine.Snapshot) bool {
	if len(a.Points) != len(b.Points) || len(a.Heatmap.Values) != len(b.Heatmap.Values) || len(a.ZoneStates) != len(b.ZoneStates) {
		return false
	}
	ab, _ := json.Marshal(struct {
		Zones []model.ZoneState
		Heat  model.Heatmap
	}{a.ZoneStates, a.Heatmap})
	bb, _ := json.Marshal(struct {
		Zones []model.ZoneState
		Heat  model.Heatmap
	}{b.ZoneStates, b.Heatmap})
	// Revision fields are runtime counters, not deterministic outputs.
	var left, right any
	_ = json.Unmarshal(ab, &left)
	_ = json.Unmarshal(bb, &right)
	return stripRevision(left) == stripRevision(right)
}

func stripRevision(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "revision" {
				typed[key] = float64(0)
			} else {
				typed[key] = stripRevision(child)
			}
		}
	case []any:
		for i := range typed {
			typed[i] = stripRevision(typed[i])
		}
	}
	b, _ := json.Marshal(value)
	return string(b)
}

func truthRarity(points []model.TruthPoint) map[string]float64 {
	counts := map[string]int{}
	for _, point := range points {
		counts[point.Rarity]++
	}
	out := map[string]float64{}
	for _, kind := range model.RarityKinds {
		if len(points) > 0 {
			out[kind] = float64(counts[kind]) / float64(len(points))
		}
	}
	return out
}

func l1(a, b map[string]float64) float64 {
	var total float64
	for _, kind := range model.RarityKinds {
		total += math.Abs(a[kind] - b[kind])
	}
	return total
}

func compareDetail(result engine.ComparisonResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if !result.Equal {
		return fmt.Sprintf("mismatch=%+v", result.Mismatch)
	}
	return fmt.Sprintf("points=%d pixels=%d zones=%d cacheSequence=equal",
		result.PointCount, result.PixelCount, result.ZoneCount)
}

func cloneHeatmap(in model.Heatmap) model.Heatmap {
	out := in
	out.Values = append([]float64(nil), in.Values...)
	out.ZoneIDs = append([]string(nil), in.ZoneIDs...)
	out.TileHashes = append([]string(nil), in.TileHashes...)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func zoneIDs(states []model.ZoneState) []string {
	out := make([]string, 0, len(states))
	for _, state := range states {
		out = append(out, state.ZoneID)
	}
	return out
}

func firstZoneID(scenario model.Scenario) string {
	zones := append([]model.Zone(nil), scenario.Region.Zones...)
	sort.SliceStable(zones, func(i, j int) bool {
		if zones[i].Ordinal == zones[j].Ordinal {
			return zones[i].ID < zones[j].ID
		}
		return zones[i].Ordinal < zones[j].Ordinal
	})
	if len(zones) == 0 {
		return ""
	}
	return zones[0].ID
}

func printReport(report verificationReport, elapsed time.Duration) {
	fmt.Println("Atelier deterministic acceptance")
	fmt.Printf("seed=%d region=%s/%s zones=%d algorithm=%s elapsed=%s\n",
		report.Seed, report.RegionID, report.RegionName, report.ZoneCount,
		report.Algorithm, elapsed.Round(time.Millisecond))
	fmt.Printf("truth points=%d rebuilt points=%d heatmap=%dx%d\n", report.TruthedPoints, report.RebuiltPoints, report.Heatmap["width"], report.Heatmap["height"])
	checks := append([]checkResult(nil), report.Checks...)
	sort.SliceStable(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	for _, check := range checks {
		status := "PASS"
		if !check.Passed {
			status = "FAIL"
		}
		fmt.Printf("[%s] %-42s %s\n", status, check.Name, check.Detail)
	}
	fmt.Printf("result=%v report=%s\n", report.Passed, report.Artifacts["report"])
}
