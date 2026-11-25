package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	tc "thermalcache"
)

type versionSummary struct {
	Fingerprint string `json:"fingerprint"`
	Ticks       int    `json:"ticks"`
	Batches     int    `json:"batches"`
	Equal       bool   `json:"equal_to_reference"`
}

type demoReport struct {
	Seed        int64                     `json:"seed"`
	Versions    map[string]versionSummary `json:"versions"`
	Timeline    []int64                   `json:"timeline"`
	Core        map[string]any            `json:"core_observations"`
	Checkpoint  map[string]any            `json:"checkpoint"`
	Interrupt   map[string]any            `json:"interrupt_rollback"`
	Incremental []map[string]any          `json:"incremental"`
}

func main() {
	seed := flag.Int64("seed", 42, "deterministic fixture seed")
	outDir := flag.String("out", "artifacts", "output directory")
	flag.Parse()
	if err := run(*seed, *outDir); err != nil {
		fmt.Fprintf(os.Stderr, "thermalcache demo failed: %v\n", err)
		os.Exit(1)
	}
}

func run(seed int64, outDir string) error {
	if err := os.MkdirAll(filepath.Join(outDir, "generated"), 0o755); err != nil {
		return err
	}
	fixture, err := tc.GenerateFixture(seed)
	if err != nil {
		return err
	}
	for _, version := range []string{tc.SchemaV1, tc.SchemaV2, tc.SchemaV3} {
		path := filepath.Join(outDir, "generated", version+".json")
		if err := os.WriteFile(path, fixture.Documents[version], 0o644); err != nil {
			return err
		}
	}

	report := demoReport{
		Seed:     seed,
		Versions: map[string]versionSummary{},
		Core:     map[string]any{}, Checkpoint: map[string]any{},
		Interrupt: map[string]any{}, Incremental: []map[string]any{},
	}
	var reference string
	var referenceProject *tc.Project
	for _, version := range []string{tc.SchemaV1, tc.SchemaV2, tc.SchemaV3} {
		project, err := tc.LoadProject(fixture.Documents[version])
		if err != nil {
			return fmt.Errorf("load %s: %w", version, err)
		}
		if referenceProject == nil {
			referenceProject = project
		}
		engine, err := tc.NewEngine(project)
		if err != nil {
			return fmt.Errorf("engine %s: %w", version, err)
		}
		result, err := engine.Run()
		if err != nil {
			return fmt.Errorf("run %s: %w", version, err)
		}
		normalized, err := normalized(result)
		if err != nil {
			return err
		}
		if reference == "" {
			reference = normalized
			sum := sha256.Sum256([]byte(reference))
			report.Timeline = result.Timeline
			report.Core = coreObservations(result)
			report.Core["result_digest"] = hex.EncodeToString(sum[:])
		}
		report.Versions[version] = versionSummary{
			Fingerprint: result.Fingerprint,
			Ticks:       len(result.Ticks),
			Batches:     len(result.Batches),
			Equal:       normalized == reference,
		}
	}
	for _, version := range []string{tc.SchemaV1, tc.SchemaV2, tc.SchemaV3} {
		if !report.Versions[version].Equal {
			return fmt.Errorf("%s result differs from v1 reference", version)
		}
	}
	if !referenceProject.SemanticEqual(fixture.Canonical) {
		return fmt.Errorf("normalized loader result differs from the generated canonical project")
	}

	v1Project, err := tc.LoadProject(fixture.Documents[tc.SchemaV1])
	if err != nil {
		return err
	}
	v1Engine, err := tc.NewEngine(v1Project)
	if err != nil {
		return err
	}
	full, err := v1Engine.Run()
	if err != nil {
		return err
	}
	checkpointPath := filepath.Join(outDir, "checkpoint.sqlite")
	if err := v1Engine.Checkpoint(checkpointPath); err != nil {
		return err
	}
	cp, err := tc.LoadCheckpoint(checkpointPath)
	if err != nil {
		return err
	}
	report.Checkpoint["path"] = checkpointPath
	report.Checkpoint["format_version"] = cp.FormatVersion
	report.Checkpoint["schema_version"] = cp.SchemaVersion
	report.Checkpoint["committed_tick"] = cp.CommittedTick
	report.Checkpoint["objects"] = len(cp.Objects)
	report.Checkpoint["batches"] = len(cp.Batches)

	// Demonstrate rollback for an interrupted pre-commit batch.
	interruptProject, _ := tc.LoadProject(fixture.Documents[tc.SchemaV1])
	interruptEngine, err := tc.NewEngine(interruptProject)
	if err != nil {
		return err
	}
	if _, err := interruptEngine.RunOne(); err != nil {
		return err
	}
	batch, err := interruptEngine.StartMigration("o05", tc.TierHot)
	if err != nil {
		return err
	}
	if _, err := interruptEngine.Gate("o05", tc.OpWrite, ""); err != nil {
		return err
	}
	beforeInterrupt, _ := interruptEngine.State("o05")
	beforeOcc := interruptEngine.Occupancy()
	if err := interruptEngine.InterruptBatch(batch.ID); err != nil {
		return err
	}
	afterInterrupt, _ := interruptEngine.State("o05")
	afterOcc := interruptEngine.Occupancy()
	report.Interrupt = map[string]any{
		"batch_id":                batch.ID,
		"before":                  beforeInterrupt,
		"after":                   afterInterrupt,
		"occupancy_before":        beforeOcc,
		"occupancy_after":         afterOcc,
		"source_content_retained": afterInterrupt.SourceVersion > 0 && afterInterrupt.Tier == tc.TierCold,
		"target_capacity_charged": afterOcc.HotUsed > beforeOcc.HotUsed,
	}
	if afterInterrupt.Tier != tc.TierCold || afterInterrupt.Migrating ||
		afterInterrupt.SourceVersion == 0 || afterOcc.HotUsed != beforeOcc.HotUsed {
		return fmt.Errorf("interrupt rollback invariant failed")
	}

	// Demonstrate durable resume from a partial batch commit point.
	partialProject, _ := tc.LoadProject(fixture.Documents[tc.SchemaV1])
	partialProject.Limits.MaxBatchBytes = 8192
	partialEngine, err := tc.NewEngine(partialProject)
	if err != nil {
		return err
	}
	for i := 0; i < 3; i++ {
		if _, err := partialEngine.RunOne(); err != nil {
			return err
		}
	}
	var partialBatch *tc.BatchResult
	for _, b := range partialEngine.Result().Batches {
		if b.Status == "staged" && len(b.ObjectIDs) == 2 {
			copyBatch := b
			partialBatch = &copyBatch
		}
	}
	if partialBatch == nil {
		return fmt.Errorf("demo expected a two-object migration at tick 200")
	}
	if err := partialEngine.CommitBatchStep(partialBatch.ID, 1); err != nil {
		return err
	}
	partialPath := filepath.Join(outDir, "partial-checkpoint.sqlite")
	if err := partialEngine.Checkpoint(partialPath); err != nil {
		return err
	}
	partialCP, err := tc.LoadCheckpoint(partialPath)
	if err != nil {
		return err
	}
	recovered, err := tc.NewEngineFromCheckpoint(partialProject, partialPath)
	if err != nil {
		return err
	}
	committedID := partialBatch.ObjectIDs[0]
	pendingID := partialBatch.ObjectIDs[1]
	stCommitted, _ := recovered.State(committedID)
	stPending, _ := recovered.State(pendingID)
	if _, err := recovered.Run(); err != nil {
		return err
	}
	stPendingAfter, _ := recovered.State(pendingID)
	finalOcc := recovered.Occupancy()
	if stCommitted.Tier != tc.TierHot || stCommitted.Migrating ||
		stPending.Tier != tc.TierCold || !stPending.Migrating ||
		stPendingAfter.Tier != tc.TierHot || stPendingAfter.Migrating {
		return fmt.Errorf("partial checkpoint resume invariant failed")
	}
	report.Checkpoint["partial_path"] = partialPath
	report.Checkpoint["partial_committed_object"] = committedID
	report.Checkpoint["partial_pending_object"] = pendingID
	report.Checkpoint["partial_resumed_from"] = partialCP.CommittedTick
	report.Checkpoint["partial_committed_state"] = stCommitted
	report.Checkpoint["partial_pending_state"] = stPending
	report.Checkpoint["partial_pending_after_run"] = stPendingAfter
	report.Checkpoint["partial_final_occupancy"] = finalOcc
	report.Checkpoint["no_double_count"] = finalOcc.HotUsed+finalOcc.ColdUsed == objectBytes(partialProject)
	if finalOcc.HotUsed+finalOcc.ColdUsed != objectBytes(partialProject) {
		return fmt.Errorf("recovered capacity double-counts objects")
	}

	// Demonstrate incremental invalidation and independent full-recompute audit.
	sim, err := tc.NewSimulator(v1Project)
	if err != nil {
		return err
	}
	if _, err := sim.Run(); err != nil {
		return err
	}
	edits := []tc.Edit{
		{Kind: tc.EditObjectAccess, ObjectID: "o03", AtMs: 200, Counts: []int64{2, 0, 0, 0, 0, 3, 0}},
		{Kind: tc.EditLayerCapacity, Tier: tc.TierHot, AtMs: 300, CapacityBytes: 20000},
		{Kind: tc.EditAppendEvent, Event: tc.AccessEvent{AtMs: 400, ObjectID: "o04", Count: 10}},
	}
	for i, edit := range edits {
		inc, err := sim.Apply(edit)
		if err != nil {
			return err
		}
		if !inc.FullMatch {
			return fmt.Errorf("incremental edit %d differs from full recompute", i)
		}
		report.Incremental = append(report.Incremental, map[string]any{
			"edit_index":       i,
			"kind":             edit.Kind,
			"full_match":       inc.FullMatch,
			"reused_ticks":     inc.ReusedTicks,
			"recomputed_ticks": inc.RecomputedTicks,
			"invalidations":    inc.Invalidations,
		})
	}

	// Key evidence from the primary run.
	report.Core["batch_count"] = len(full.Batches)
	report.Core["all_batches_within_limits"] = batchesWithinLimits(full, v1Project)
	report.Core["final_occupancy"] = full.Final
	report.Core["hot_cold_bytes_each_object_once"] = full.Final.HotUsed+full.Final.ColdUsed == objectBytes(v1Project)
	if !report.Core["all_batches_within_limits"].(bool) ||
		!report.Core["hot_cold_bytes_each_object_once"].(bool) {
		return fmt.Errorf("primary run invariant failed")
	}

	reportPath := filepath.Join(outDir, "demo_output.json")
	if err := writeJSON(reportPath, report); err != nil {
		return err
	}
	verifyPath := filepath.Join(outDir, "verification.txt")
	if err := os.WriteFile(verifyPath, []byte(verificationText(report, full)), 0o644); err != nil {
		return err
	}

	fmt.Printf("thermalcache seed=%d\n", seed)
	for _, version := range []string{tc.SchemaV1, tc.SchemaV2, tc.SchemaV3} {
		v := report.Versions[version]
		fmt.Printf("  %s: fingerprint=%s ticks=%d batches=%d equal=%v\n",
			version, short(v.Fingerprint), v.Ticks, v.Batches, v.Equal)
	}
	fmt.Printf("  checkpoint: %s (committed_tick=%d, objects=%d)\n",
		checkpointPath, cp.CommittedTick, len(cp.Objects))
	fmt.Printf("  partial resume: %v, no_double_count=%v\n",
		report.Checkpoint["partial_pending_after_run"].(tc.ObjectState).Tier == tc.TierHot,
		report.Checkpoint["no_double_count"])
	fmt.Printf("  interrupt rollback: source_retained=%v\n", report.Interrupt["source_content_retained"])
	for _, item := range report.Incremental {
		fmt.Printf("  incremental %-16s full_match=%v reused=%d recomputed=%d\n",
			item["kind"], item["full_match"], len(item["reused_ticks"].([]int64)),
			len(item["recomputed_ticks"].([]int64)))
	}
	fmt.Printf("  wrote %s, %s\n", reportPath, verifyPath)
	return nil
}

func normalized(res *tc.SimulationResult) (string, error) {
	cp := *res
	cp.SchemaVersion = ""
	cp.Fingerprint = ""
	cp.Ticks = append([]tc.TickResult(nil), res.Ticks...)
	for i := range cp.Ticks {
		cp.Ticks[i].LayerMetrics = nil
	}
	b, err := json.Marshal(cp)
	return string(b), err
}

func coreObservations(res *tc.SimulationResult) map[string]any {
	out := map[string]any{"timeline": res.Timeline}
	for _, tick := range res.Ticks {
		if tick.AtMs == 100 || tick.AtMs == 200 || tick.AtMs == 500 || tick.AtMs == 600 {
			for _, obj := range tick.Objects {
				if obj.ObjectID == "o05" || obj.ObjectID == "o10" {
					key := fmt.Sprintf("%s@%d", obj.ObjectID, tick.AtMs)
					out[key] = obj
				}
			}
		}
		if len(tick.Backpressure) > 0 {
			out[fmt.Sprintf("backpressure@%d", tick.AtMs)] = tick.Backpressure
		}
	}
	return out
}

func batchesWithinLimits(res *tc.SimulationResult, p *tc.Project) bool {
	for _, batch := range res.Batches {
		if len(batch.ObjectIDs) > p.Limits.MaxBatchObjects {
			return false
		}
		var bytes int64
		for _, id := range batch.ObjectIDs {
			for _, object := range p.Objects {
				if object.ID == id {
					bytes += object.SizeBytes
					break
				}
			}
		}
		if bytes > p.Limits.MaxBatchBytes {
			return false
		}
	}
	return true
}

func objectBytes(p *tc.Project) int64 {
	var total int64
	for _, o := range p.Objects {
		total += o.SizeBytes
	}
	return total
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func verificationText(report demoReport, res *tc.SimulationResult) string {
	var versions []string
	for v := range report.Versions {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	text := "thermalcache deterministic verification\n"
	text += fmt.Sprintf("seed=%d\n", report.Seed)
	for _, v := range versions {
		item := report.Versions[v]
		text += fmt.Sprintf("%s fingerprint=%s ticks=%d batches=%d equal_to_v1=%v\n",
			v, item.Fingerprint, item.Ticks, item.Batches, item.Equal)
	}
	text += fmt.Sprintf("timeline=%v\n", res.Timeline)
	text += fmt.Sprintf("normalized_result_digest=%v\n", report.Core["result_digest"])
	text += fmt.Sprintf("all_batches_within_limits=%v\n", report.Core["all_batches_within_limits"])
	text += fmt.Sprintf("hot+cold bytes each object once=%v\n", report.Core["hot_cold_bytes_each_object_once"])
	text += fmt.Sprintf("checkpoint committed_tick=%v objects=%v\n",
		report.Checkpoint["committed_tick"], report.Checkpoint["objects"])
	text += fmt.Sprintf("partial resume no_double_count=%v\n", report.Checkpoint["no_double_count"])
	text += fmt.Sprintf("interrupt source_retained=%v\n", report.Interrupt["source_content_retained"])
	for _, item := range report.Incremental {
		text += fmt.Sprintf("incremental %s full_match=%v reused=%v recomputed=%v\n",
			item["kind"], item["full_match"], item["reused_ticks"], item["recomputed_ticks"])
	}
	return text
}
