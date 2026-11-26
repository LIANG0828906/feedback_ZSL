package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"layerforge/internal/apperr"
	"layerforge/internal/build"
	"layerforge/internal/checkpoint"
	"layerforge/internal/engine"
	"layerforge/internal/generator"
	"layerforge/internal/loader"
	"layerforge/internal/model"
	"layerforge/internal/sqlitefile"
)

type demoReport struct {
	Seed            int64              `json:"seed"`
	Schemas         []versionMatch     `json:"schemas"`
	Build           engine.BuildResult `json:"build"`
	Restart         engine.BuildResult `json:"restart"`
	Incremental     engine.BuildResult `json:"incremental"`
	FreshRebuild    engine.BuildResult `json:"freshRebuild"`
	Cleanup         cleanupReport      `json:"cleanup"`
	Resume          engine.BuildResult `json:"resume"`
	PersistedRounds int                `json:"persistedRounds"`
	Injections      []injectionResult  `json:"injections"`
	SourceUnchanged bool               `json:"sourceUnchanged"`
	Passed          bool               `json:"passed"`
}

type cleanupReport struct {
	ConflictCode apperr.Code        `json:"conflictCode"`
	Removed      []string           `json:"removed"`
	RebuildSteps []int              `json:"rebuildSteps"`
	Rebuilt      engine.BuildResult `json:"rebuilt"`
}

type injectionResult struct {
	Name   string      `json:"name"`
	Code   apperr.Code `json:"code"`
	Path   string      `json:"path,omitempty"`
	Steps  []int       `json:"steps,omitempty"`
	Layers []string    `json:"layers,omitempty"`
	Detail string      `json:"detail,omitempty"`
	Passed bool        `json:"passed"`
}

func runDemo(args []string) error {
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := flags.Int64("seed", generator.DefaultSeed, "deterministic fixture seed")
	workdir := flags.String("workdir", ".layerforge-demo", "demo workspace")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	report, err := executeDemo(*seed, *workdir)
	if *asJSON {
		if encodeErr := writeJSON(report); encodeErr != nil {
			return encodeErr
		}
	} else {
		printDemo(report, *workdir)
	}
	if err != nil {
		return err
	}
	if !report.Passed {
		return apperr.New(apperr.Internal, "demo", "acceptance", "one or more checks failed")
	}
	return nil
}

func executeDemo(seed int64, workdir string) (demoReport, error) {
	report := demoReport{Seed: seed}
	if err := os.RemoveAll(workdir); err != nil {
		return report, err
	}
	generatedDir := filepath.Join(workdir, "generated")
	fixture, err := generator.Generate(seed)
	if err != nil {
		return report, err
	}
	initialHash := model.SemanticHash(fixture.Project)
	if err := fixture.Write(generatedDir); err != nil {
		return report, err
	}
	matches, err := verifyDirectory(generatedDir, fixture)
	if err != nil {
		return report, err
	}
	report.Schemas = matches

	dbPath := filepath.Join(workdir, "checkpoint.db")
	store := checkpoint.Open(dbPath)
	eng := mustEngineDemo(fixture.Project, store)
	report.Build, err = eng.Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		return report, err
	}
	restartEngine := mustEngineDemo(fixture.Project, store)
	report.Restart, err = restartEngine.Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		return report, err
	}

	oldProject := fixture.Project
	edited, err := engine.EditStep(oldProject, 4, func(step *model.Step) {
		step.CommandFingerprint = model.HashFields("demo-edit/v1", "package-command")
	})
	if err != nil {
		return report, err
	}
	editedEngine := mustEngineDemo(edited, store)
	report.Incremental, err = editedEngine.Build(context.Background(), engine.BuildOptions{PreviousProject: &oldProject})
	if err != nil {
		return report, err
	}
	freshStore := checkpoint.Open(filepath.Join(workdir, "edited-fresh.db"))
	report.FreshRebuild, err = mustEngineDemo(edited, freshStore).Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		return report, err
	}
	if !reflect.DeepEqual(report.Incremental.FinalDigests, report.FreshRebuild.FinalDigests) {
		return report, apperr.New(apperr.CheckpointIntegrity, "demo", "incremental", "incremental and fresh rebuild differ")
	}

	_, conflictErr := editedEngine.Cleanup([]string{"step:package"})
	report.Cleanup.ConflictCode = apperr.CodeOf(conflictErr)
	if !apperr.IsCode(conflictErr, apperr.CleanupConflict) {
		return report, fmt.Errorf("shared-layer cleanup conflict: %w", conflictErr)
	}
	clean, err := editedEngine.Cleanup([]string{"step:release"})
	if err != nil {
		return report, err
	}
	report.Cleanup.Removed = clean.RemovedLayers
	report.Cleanup.RebuildSteps = clean.RebuildSteps
	report.Cleanup.Rebuilt, err = mustEngineDemo(edited, store).Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		return report, err
	}

	interruptDB := filepath.Join(workdir, "interrupt.db")
	interruptStore := checkpoint.Open(interruptDB)
	interrupted, interruptErr := mustEngineDemo(fixture.Project, interruptStore).Build(context.Background(), engine.BuildOptions{InterruptAfterRound: 2})
	if !apperr.IsCode(interruptErr, apperr.Interrupted) {
		return report, fmt.Errorf("expected interruption, got %v", interruptErr)
	}
	if len(interrupted.Rounds) != 2 {
		return report, fmt.Errorf("interrupted build persisted %d rounds", len(interrupted.Rounds))
	}
	snapshot, err := interruptStore.Load()
	if err != nil {
		return report, err
	}
	report.PersistedRounds = snapshot.LastRound
	report.Resume, err = mustEngineDemo(fixture.Project, interruptStore).Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		return report, err
	}

	baselineCopy := filepath.Join(workdir, "baseline-copy.db")
	if err := copyFile(dbPath, baselineCopy); err != nil {
		return report, err
	}
	report.Injections = runInjections(fixture, dbPath, baselineCopy, workdir)
	for _, injection := range report.Injections {
		if !injection.Passed {
			return report, fmt.Errorf("injection %s failed: code=%s", injection.Name, injection.Code)
		}
	}
	report.SourceUnchanged = model.SemanticHash(fixture.Project) == initialHash
	report.Passed = report.SourceUnchanged &&
		report.Restart.HitCount == 8 &&
		report.Incremental.BuildCount == 4 &&
		report.Cleanup.Rebuilt.HitCount == 7 &&
		report.Resume.BuildCount == 4
	return report, nil
}

func runInjections(fixture *generator.Fixture, baselineDB, copiedDB, workdir string) []injectionResult {
	var results []injectionResult
	addError := func(name string, want apperr.Code, err error) {
		code := apperr.CodeOf(err)
		item := injectionResult{Name: name, Code: code, Passed: code == want}
		if err != nil {
			item.Detail = err.Error()
		}
		var business *apperr.Error
		if errorsAs(err, &business) {
			item.Path = business.Path
			item.Steps = business.Steps
			item.Layers = business.Layers
		}
		results = append(results, item)
	}

	corrupt := bytes.Replace(fixture.Versions["project.v1.json"], []byte(`"threadLimit": 2`), []byte(`"threadLimit": "two"`), 1)
	_, err := loader.Load(corrupt)
	addError("corrupt-field-type", apperr.SchemaType, err)

	unknown := bytes.Replace(fixture.Versions["project.v1.json"], []byte(`"schema": "layerforge/v1",`), []byte(`"schema": "layerforge/v1", "unexpected": 1,`), 1)
	_, err = loader.Load(unknown)
	addError("unknown-field", apperr.SchemaField, err)

	cycle := fixture.Project.Clone()
	cycle.Steps[0].DependsOn = append(cycle.Steps[0].DependsOn, "step:release")
	_, err = build.BuildGraph(cycle)
	addError("dependency-cycle", apperr.GraphCycle, err)

	missing := fixture.Project.Clone()
	missing.Steps[0].DependsOn = append(missing.Steps[0].DependsOn, "base:absent")
	_, err = build.BuildGraph(missing)
	addError("missing-base", apperr.GraphMissingRef, err)

	conflict := fixture.Project.Clone()
	for index := range conflict.Shares {
		if conflict.Shares[index].LayerID == "step:compile" {
			conflict.Shares[index].Consumers = []int{2, 3}
		}
	}
	_, err = build.BuildGraph(conflict)
	addError("share-conflict", apperr.GraphShareConflict, err)

	invalidThreads := model.Budget{Latency: 17 * time.Millisecond, MaxRounds: 6, ThreadLimit: 0, MemoryPerThread: 65536, BufferPerThread: 2}
	_, err = mustEngineDemo(fixture.Project, checkpoint.Open(filepath.Join(workdir, "invalid-thread.db"))).Build(context.Background(), engine.BuildOptions{
		Budget: &engine.BudgetOverride{Latency: invalidThreads.Latency, MaxRounds: invalidThreads.MaxRounds, ThreadLimit: invalidThreads.ThreadLimit, MemoryPerThread: invalidThreads.MemoryPerThread, BufferPerThread: invalidThreads.BufferPerThread},
	})
	addError("out-of-range-thread", apperr.BudgetThreads, err)

	invalidMemory := model.Budget{Latency: 17 * time.Millisecond, MaxRounds: 6, ThreadLimit: 2, MemoryPerThread: 0, BufferPerThread: 2}
	_, err = mustEngineDemo(fixture.Project, checkpoint.Open(filepath.Join(workdir, "invalid-memory.db"))).Build(context.Background(), engine.BuildOptions{
		Budget: &engine.BudgetOverride{Latency: invalidMemory.Latency, MaxRounds: invalidMemory.MaxRounds, ThreadLimit: invalidMemory.ThreadLimit, MemoryPerThread: invalidMemory.MemoryPerThread, BufferPerThread: invalidMemory.BufferPerThread},
	})
	addError("out-of-range-memory", apperr.BudgetMemory, err)

	_, err = mustEngineDemo(fixture.Project, checkpoint.Open(filepath.Join(workdir, "invalid-buffer.db"))).Build(context.Background(), engine.BuildOptions{
		Budget: &engine.BudgetOverride{Latency: 17 * time.Millisecond, MaxRounds: 6, ThreadLimit: 2, MemoryPerThread: 65536, BufferPerThread: 0},
	})
	addError("buffer-backpressure", apperr.BackpressureQueue, err)

	cleanupStore := checkpoint.Open(copiedDB)
	cleanupEngine := mustEngineDemo(fixture.Project, cleanupStore)
	_, err = cleanupEngine.Cleanup([]string{"step:package"})
	addError("cleanup-conflict", apperr.CleanupConflict, err)

	truncatedPath := filepath.Join(workdir, "truncated.db")
	data, readErr := os.ReadFile(baselineDB)
	if readErr == nil {
		readErr = os.WriteFile(truncatedPath, data[:len(data)/2], 0o600)
	}
	if readErr != nil {
		addError("truncated-checkpoint", apperr.CheckpointTruncated, readErr)
	} else {
		_, err = mustEngineDemo(fixture.Project, checkpoint.Open(truncatedPath)).Build(context.Background(), engine.BuildOptions{})
		addError("truncated-checkpoint", apperr.CheckpointTruncated, err)
	}

	versionPath := filepath.Join(workdir, "version-mismatch.db")
	if err := copyFile(baselineDB, versionPath); err != nil {
		addError("version-mismatch", apperr.CheckpointVersion, err)
	} else if err := overwriteMetaVersion(versionPath, "layerforge-checkpoint/v0"); err != nil {
		addError("version-mismatch", apperr.CheckpointVersion, err)
	} else {
		resetResult, resetErr := mustEngineDemo(fixture.Project, checkpoint.Open(versionPath)).Build(context.Background(), engine.BuildOptions{})
		item := injectionResult{
			Name:   "version-mismatch",
			Code:   resetResult.Recovery.Code,
			Path:   versionPath,
			Detail: resetResult.Recovery.Reason,
			Passed: resetErr == nil && resetResult.Recovery.Reset,
		}
		if resetErr != nil {
			item.Detail = resetErr.Error()
		}
		results = append(results, item)
	}
	return results
}

func overwriteMetaVersion(path, version string) error {
	db, err := sqlitefile.Read(path)
	if err != nil {
		return err
	}
	found := false
	for tableIndex := range db.Tables {
		if db.Tables[tableIndex].Name != "meta" {
			continue
		}
		for rowIndex := range db.Tables[tableIndex].Rows {
			if db.Tables[tableIndex].Rows[rowIndex][0] == "version" {
				db.Tables[tableIndex].Rows[rowIndex][1] = version
				found = true
			}
		}
	}
	if !found {
		return fmt.Errorf("version meta row not found")
	}
	return sqlitefile.WriteAtomic(path, *db, nil)
}

func copyFile(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o600)
}

func mustEngineDemo(project model.Project, store *checkpoint.Store) *engine.Engine {
	eng, err := engine.New(project, store)
	if err != nil {
		panic(err)
	}
	return eng
}

func errorsAs(err error, target **apperr.Error) bool {
	for err != nil {
		if business, ok := err.(*apperr.Error); ok {
			*target = business
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func printDemo(report demoReport, workdir string) {
	fmt.Fprintf(os.Stdout, "LayerForge deterministic demo seed=%d\n", report.Seed)
	for _, match := range report.Schemas {
		fmt.Fprintf(os.Stdout, "schema %s %s hash=%s truth=OK\n", match.File, match.Schema, match.Hash)
	}
	printDemoBuild("cold", report.Build)
	fmt.Fprintf(os.Stdout, "restart hits=%d builds=%d rounds=%d\n", report.Restart.HitCount, report.Restart.BuildCount, len(report.Restart.Rounds))
	fmt.Fprintf(os.Stdout, "incremental edits=step-4 hits=%d builds=%d\n", report.Incremental.HitCount, report.Incremental.BuildCount)
	for _, decision := range report.Incremental.Decisions {
		if !decision.Hit {
			fmt.Fprintf(os.Stdout, "  rebuild step=%d layer=%s source=%s\n", decision.Step, decision.LayerID, decision.Source)
		}
	}
	fmt.Fprintf(os.Stdout, "incremental_vs_full=identical\n")
	fmt.Fprintf(os.Stdout, "cleanup conflict_code=%s removed=%v rebuild_steps=%v rebuilt_hits=%d\n",
		report.Cleanup.ConflictCode, report.Cleanup.Removed, report.Cleanup.RebuildSteps, report.Cleanup.Rebuilt.HitCount)
	fmt.Fprintf(os.Stdout, "interruption persisted_rounds=%d resumed_hits=%d resumed_builds=%d\n",
		report.PersistedRounds, report.Resume.HitCount, report.Resume.BuildCount)
	for _, injection := range report.Injections {
		status := "FAIL"
		if injection.Passed {
			status = "OK"
		}
		fmt.Fprintf(os.Stdout, "injection %-22s %s code=%s path=%s\n", injection.Name, status, injection.Code, injection.Path)
	}
	fmt.Fprintf(os.Stdout, "source_unchanged=%t workspace=%s\n", report.SourceUnchanged, workdir)
	if report.Passed {
		fmt.Fprintln(os.Stdout, "PASS")
	} else {
		fmt.Fprintln(os.Stdout, "FAIL")
	}
}

func printDemoBuild(label string, result engine.BuildResult) {
	fmt.Fprintf(os.Stdout, "%s hits=%d builds=%d rounds=%d codes=%v\n", label, result.HitCount, result.BuildCount, len(result.Rounds), result.Codes)
	for _, round := range result.Rounds {
		fmt.Fprintf(os.Stdout, "  round %d built=%v deferred=%v latency=%d/%dms remaining=%dms\n",
			round.Round, layerIDs(round.Built), deferralSummary(round.Deferred), round.LatencyUsedMS, round.LatencyBudgetMS, round.LatencyRemainingMS)
	}
	for _, degradation := range result.Degradations {
		fmt.Fprintf(os.Stdout, "  degraded %s %s impact=%v\n", degradation.Code, degradation.Strategy, degradation.ImpactLayers)
	}
}

func deferralSummary(deferred []engine.Deferral) []string {
	out := make([]string, 0, len(deferred))
	for _, item := range deferred {
		out = append(out, fmt.Sprintf("%s(%s#%d)", item.LayerID, item.Reason, item.Rank))
	}
	sort.Strings(out)
	return out
}
