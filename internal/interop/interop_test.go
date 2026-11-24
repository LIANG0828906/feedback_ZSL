package interop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"saveinterop/internal/checkpoint"
	"saveinterop/internal/core"
	"saveinterop/internal/generator"
)

type fixture struct {
	dir      string
	table    core.MappingTable
	catalog  generator.Catalog
	provider *Middleware
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	catalog, err := generator.Generate(dir, 20261124, 5)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	provider, err := New(catalog.Mappings)
	if err != nil {
		t.Fatalf("new middleware: %v", err)
	}
	return fixture{dir: dir, table: catalog.Mappings, catalog: catalog, provider: provider}
}

func (f fixture) archive(t *testing.T, branch, sampleID string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, branch, sampleID+".json"))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	return data
}

func (f fixture) truth(t *testing.T, branch, sampleID string) core.SemanticModel {
	t.Helper()
	for _, sample := range f.catalog.Samples {
		if sample.ID == sampleID {
			model, ok := sample.Branches[branch]
			if !ok {
				t.Fatalf("missing truth for %s/%s", branch, sampleID)
			}
			return model
		}
	}
	t.Fatalf("missing sample %s", sampleID)
	return core.SemanticModel{}
}

func TestLoadAllBranchesMatchesTruth(t *testing.T) {
	f := newFixture(t)
	for _, sample := range f.catalog.Samples {
		var fingerprints []string
		for _, branch := range f.catalog.Branches {
			loaded, err := f.provider.LoadBytes(f.archive(t, branch, sample.ID), LoadOptions{ExpectedBranch: branch})
			if err != nil {
				t.Fatalf("load %s/%s: %v", branch, sample.ID, err)
			}
			if loaded.SourceBranch != branch {
				t.Fatalf("source branch = %s, want %s", loaded.SourceBranch, branch)
			}
			if err := core.CompareSemantics(f.truth(t, branch, sample.ID), loaded.Model, branch, "truth"); err != nil {
				t.Fatalf("truth mismatch %s/%s: %v", branch, sample.ID, err)
			}
			fp, err := core.Fingerprint(loaded.Model, f.table.Version)
			if err != nil {
				t.Fatal(err)
			}
			fingerprints = append(fingerprints, fp)
			if len(loaded.Traces) == 0 {
				t.Fatalf("no mapping traces for %s/%s", branch, sample.ID)
			}
		}
		for i := 1; i < len(fingerprints); i++ {
			if fingerprints[i] != fingerprints[0] {
				t.Fatalf("branch-dependent fingerprint for %s: %v", sample.ID, fingerprints)
			}
		}
	}
}

func TestConditionFlagInversionIsNormalized(t *testing.T) {
	f := newFixture(t)
	data := f.archive(t, "ember", "sample-01")
	var archive generator.Archive
	if err := decodeInto(data, &archive); err != nil {
		t.Fatal(err)
	}
	raw, ok := core.Lookup(archive.Save, "flags.eventOn")
	if !ok {
		t.Fatalf("missing physical flag")
	}
	physical, ok := core.DecodeBool(raw)
	if !ok || physical {
		t.Fatalf("ember physical flag = %v, want false for an active semantic event", raw)
	}
	loaded, err := f.provider.LoadBytes(data, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Model.Flags["event_enabled"] {
		t.Fatalf("semantic event flag was not normalized through inversion")
	}
}

func TestCrossExportSemanticEquivalenceAndDeterminism(t *testing.T) {
	f := newFixture(t)
	for _, sample := range f.catalog.Samples {
		for _, source := range f.catalog.Branches {
			loaded, err := f.provider.LoadBytes(f.archive(t, source, sample.ID), LoadOptions{ExpectedBranch: source})
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range f.catalog.Branches {
				options := ExportOptions{
					SourceBranch: source, TargetBranch: target,
					RuleVersion: f.table.Version, PreserveConditionals: true,
				}
				first, err := f.provider.ExportAndVerify(loaded.Model, options)
				if err != nil {
					t.Fatalf("cross export %s/%s -> %s: %v", sample.ID, source, target, err)
				}
				second, err := f.provider.Export(loaded.Model, options)
				if err != nil {
					t.Fatal(err)
				}
				if string(first.Export.Archive) != string(second.Archive) {
					t.Fatalf("non-deterministic export bytes for %s -> %s", source, target)
				}
				if err := core.CompareSemantics(loaded.Model, first.Loaded.Model, source, target); err != nil {
					t.Fatalf("readback mismatch %s -> %s: %v", source, target, err)
				}
				if first.Export.ArchiveHash != core.HashBytes(first.Export.Archive) {
					t.Fatalf("archive hash mismatch")
				}
				for _, trace := range first.Export.Traces {
					if trace.RuleVersion != f.table.Version {
						t.Fatalf("trace rule version = %s", trace.RuleVersion)
					}
				}
			}
		}
	}
}

func TestMappingVersionMismatch(t *testing.T) {
	f := newFixture(t)
	data := f.archive(t, "aurora", "sample-01")
	var archive generator.Archive
	if err := json.Unmarshal(data, &archive); err != nil {
		t.Fatal(err)
	}
	archive.MappingVersion = "mapping-v0"
	changed, _ := json.Marshal(archive)
	_, err := f.provider.LoadBytes(changed, LoadOptions{})
	if core.CodeOf(err) != core.CodeMapping {
		t.Fatalf("code = %s, want %s: %v", core.CodeOf(err), core.CodeMapping, err)
	}
	var coreErr *core.Error
	if !errors.As(err, &coreErr) || coreErr.Entry == "" || coreErr.SourceBranch != "aurora" {
		t.Fatalf("insufficient E_MAP context: %v", err)
	}
	_, err = f.provider.Export(core.SemanticModel{}, ExportOptions{
		SourceBranch: "aurora", TargetBranch: "ember", RuleVersion: "mapping-v0",
	})
	if core.CodeOf(err) != core.CodeMapping {
		t.Fatalf("export code = %s: %v", core.CodeOf(err), err)
	}
}

func TestSemanticConditionalAndTimeErrors(t *testing.T) {
	f := newFixture(t)
	base := f.archive(t, "aurora", "sample-01")

	var archive generator.Archive
	if err := decodeInto(base, &archive); err != nil {
		t.Fatal(err)
	}
	original := core.CloneMap(archive.Save)

	core.Delete(archive.Save, "flags.eventOn")
	changed, _ := json.Marshal(archive)
	_, err := f.provider.LoadBytes(changed, LoadOptions{})
	if core.CodeOf(err) != core.CodeCondition {
		t.Fatalf("missing flag code = %s: %v", core.CodeOf(err), err)
	}
	var coreErr *core.Error
	if !errors.As(err, &coreErr) || coreErr.FieldPath != "flags.eventOn" || coreErr.Measured == nil {
		t.Fatalf("missing flag context: %v", err)
	}

	archive.Save = core.CloneMap(original)
	core.Set(archive.Save, "clock.ticks", int64(math.MaxInt64))
	changed, _ = json.Marshal(archive)
	_, err = f.provider.LoadBytes(changed, LoadOptions{})
	if core.CodeOf(err) != core.CodeTime {
		t.Fatalf("time overflow code = %s: %v", core.CodeOf(err), err)
	}
	if !errors.As(err, &coreErr) || coreErr.FieldPath != "clock.ticks" {
		t.Fatalf("time context: %v", err)
	}

	archive.Save = core.CloneMap(original)
	core.Delete(archive.Save, "stats.valueA")
	changed, _ = json.Marshal(archive)
	_, err = f.provider.LoadBytes(changed, LoadOptions{})
	if core.CodeOf(err) != core.CodeSemantic {
		t.Fatalf("missing semantic code = %s: %v", core.CodeOf(err), err)
	}
	if !errors.As(err, &coreErr) || coreErr.FieldPath != "stats.valueA" || coreErr.Conflict == "" {
		t.Fatalf("semantic context: %v", err)
	}
}

func TestCycleDetection(t *testing.T) {
	table := core.DemoMappingTable()
	for i := range table.Branches {
		if table.Branches[i].ID != "aurora" {
			continue
		}
		table.Branches[i].Conditions = append(table.Branches[i].Conditions,
			core.ConditionRule{ID: "loop_a", Op: "not", Refs: []string{"loop_b"}},
			core.ConditionRule{ID: "loop_b", Op: "not", Refs: []string{"loop_a"}},
		)
		table.Branches[i].Conditionals[0].ConditionID = "loop_a"
	}
	provider, err := New(table)
	if core.CodeOf(err) != core.CodeCycle {
		t.Fatalf("new middleware code = %s, want %s: %v", core.CodeOf(err), core.CodeCycle, err)
	}
	var coreErr *core.Error
	if !errors.As(err, &coreErr) || len(coreErr.Chain) < 3 {
		t.Fatalf("cycle chain missing: %v", err)
	}
	if provider != nil {
		t.Fatalf("middleware should not be constructed with a cyclic condition graph")
	}
}

func TestCheckpointResumeMatchesFullImport(t *testing.T) {
	f := newFixture(t)
	for _, sample := range f.catalog.Samples {
		for _, branch := range f.catalog.Branches {
			data := f.archive(t, branch, sample.ID)
			full, err := f.provider.LoadBytes(data, LoadOptions{ExpectedBranch: branch})
			if err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
			fresh, err := f.provider.ImportCheckpointed(context.Background(), data,
				LoadOptions{ExpectedBranch: branch},
				CheckpointOptions{DBPath: dbPath, WindowSize: 2})
			if err != nil {
				t.Fatalf("fresh checkpoint %s/%s: %v", branch, sample.ID, err)
			}
			if fresh.ResumedWindows != 0 {
				t.Fatalf("fresh resumed windows = %d", fresh.ResumedWindows)
			}
			if err := CompareThreePaths(*full, fresh.LoadResult, 2); err != nil {
				t.Fatalf("fresh vs full %s/%s: %v", branch, sample.ID, err)
			}
			resumed, err := f.provider.ImportCheckpointed(context.Background(), data,
				LoadOptions{ExpectedBranch: branch},
				CheckpointOptions{DBPath: dbPath, WindowSize: 2})
			if err != nil {
				t.Fatalf("resume %s/%s: %v", branch, sample.ID, err)
			}
			if resumed.ResumedWindows != resumed.Windows {
				t.Fatalf("resume count = %d/%d", resumed.ResumedWindows, resumed.Windows)
			}
			if err := CompareThreePaths(*full, resumed.LoadResult, 2); err != nil {
				t.Fatalf("resume vs full %s/%s: %v", branch, sample.ID, err)
			}
		}
	}
}

func TestCheckpointVersionConflictRecoveryAndWindowIsolation(t *testing.T) {
	f := newFixture(t)
	data := f.archive(t, "aurora", "sample-02")
	full, err := f.provider.LoadBytes(data, LoadOptions{ExpectedBranch: "aurora"})
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	call := 0
	f.provider.windowHook = func(window int) error {
		if window == 2 && call == 0 {
			call++
			return &core.Error{Code: core.CodeSemantic, Op: "test-injected", Message: "window failure"}
		}
		return nil
	}
	_, err = f.provider.ImportCheckpointed(context.Background(), data,
		LoadOptions{ExpectedBranch: "aurora"},
		CheckpointOptions{DBPath: dbPath, WindowSize: 2})
	if core.CodeOf(err) != core.CodeSemantic {
		t.Fatalf("injected error code = %s: %v", core.CodeOf(err), err)
	}
	f.provider.windowHook = nil
	retried, err := f.provider.ImportCheckpointed(context.Background(), data,
		LoadOptions{ExpectedBranch: "aurora"},
		CheckpointOptions{DBPath: dbPath, WindowSize: 2})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retried.ResumedWindows != 2 {
		t.Fatalf("resumed after failure = %d, want 2", retried.ResumedWindows)
	}
	if err := CompareThreePaths(*full, retried.LoadResult, 2); err != nil {
		t.Fatalf("retry consistency: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE checkpoint_runs SET mapping_version = 'tampered'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	recovered, err := f.provider.ImportCheckpointed(context.Background(), data,
		LoadOptions{ExpectedBranch: "aurora"},
		CheckpointOptions{DBPath: dbPath, WindowSize: 2, RecomputeOnConflict: true})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.Recovery == nil || recovered.Recovery.Code != core.CodeVersion {
		t.Fatalf("missing recovery notice: %+v", recovered.Recovery)
	}
	if recovered.Recovery.Window < 0 || recovered.Recovery.Expected == "" || recovered.Recovery.Actual == "" {
		t.Fatalf("recovery notice lacks window/version context: %+v", recovered.Recovery)
	}
	if err := CompareThreePaths(*full, recovered.LoadResult, 2); err != nil {
		t.Fatalf("recovered consistency: %v", err)
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE checkpoint_runs SET mapping_version = 'tampered-again'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = f.provider.ImportCheckpointed(context.Background(), data,
		LoadOptions{ExpectedBranch: "aurora"},
		CheckpointOptions{DBPath: dbPath, WindowSize: 2})
	if core.CodeOf(err) != core.CodeVersion {
		t.Fatalf("tamper code = %s: %v", core.CodeOf(err), err)
	}
}

func TestCheckpointHashTamperInvalidatesWholeRun(t *testing.T) {
	f := newFixture(t)
	data := f.archive(t, "ember", "sample-01")
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	_, err := f.provider.ImportCheckpointed(context.Background(), data,
		LoadOptions{ExpectedBranch: "ember"},
		CheckpointOptions{DBPath: dbPath, WindowSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var runID string
	if err := db.QueryRow(`SELECT run_id FROM checkpoint_runs LIMIT 1`).Scan(&runID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE checkpoint_windows SET content_hash = 'tampered' WHERE window_index = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = f.provider.ImportCheckpointed(context.Background(), data,
		LoadOptions{ExpectedBranch: "ember"},
		CheckpointOptions{DBPath: dbPath, WindowSize: 3})
	if core.CodeOf(err) != core.CodeVersion {
		t.Fatalf("hash tamper code = %s: %v", core.CodeOf(err), err)
	}
	var coreErr *core.Error
	if !errors.As(err, &coreErr) || coreErr.Window != 1 || coreErr.Expected == "" || coreErr.Actual == "" {
		t.Fatalf("hash tamper context: %v", err)
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	count, err := store.CountWindows(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("tampered run was not invalidated, windows=%d", count)
	}
}

func TestThreePathDiffLocationAndFingerprintCollision(t *testing.T) {
	f := newFixture(t)
	data := f.archive(t, "aurora", "sample-01")
	loaded, err := f.provider.LoadBytes(data, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	changed := *loaded
	changed.Model = loaded.Model.Clone()
	changed.Model.Coins++
	err = CompareThreePaths(*loaded, changed, 2)
	if core.CodeOf(err) != core.CodeDiff {
		t.Fatalf("diff code = %s: %v", core.CodeOf(err), err)
	}
	var coreErr *core.Error
	if !errors.As(err, &coreErr) || coreErr.Semantic != "coins" || coreErr.Window != 0 {
		t.Fatalf("diff location: %v", err)
	}
	err = core.CheckFingerprintCollisions([]core.FingerprintRecord{
		{Entry: "left", Fingerprint: "same", Canonical: "coins=1"},
		{Entry: "right", Fingerprint: "same", Canonical: "coins=2"},
	})
	if core.CodeOf(err) != core.CodeFinger {
		t.Fatalf("collision code = %s: %v", core.CodeOf(err), err)
	}
	if !errors.As(err, &coreErr) || coreErr.EntryA != "left" || coreErr.EntryB != "right" {
		t.Fatalf("collision context: %v", err)
	}
}

func TestConditionalConflictUsesDeclaredPriority(t *testing.T) {
	f := newFixture(t)
	loaded, err := f.provider.LoadBytes(f.archive(t, "aurora", "sample-01"), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	options := ExportOptions{
		SourceBranch: "aurora", TargetBranch: "nova",
		RuleVersion: f.table.Version, PreserveConditionals: true,
	}
	first, err := f.provider.ExportAndVerify(loaded.Model, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Export.Rulings) == 0 {
		t.Fatalf("expected a conditional conflict ruling")
	}
	if first.Export.Rulings[0].Action != "preserve" || first.Export.Rulings[0].Priority != 10 {
		t.Fatalf("unexpected ruling: %+v", first.Export.Rulings[0])
	}
	second, err := f.provider.Export(loaded.Model, options)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first.Export.Rulings) != fmt.Sprint(second.Rulings) {
		t.Fatalf("ruling order is non-deterministic")
	}
}

func TestMappingQueryAndConditionalOrderIndependentFingerprint(t *testing.T) {
	f := newFixture(t)
	entries := f.provider.MappingTable().Query("aurora", "coins")
	foundBase := false
	for _, entry := range entries {
		if entry.Kind == "int" && entry.PhysicalPath == "stats.valueA" {
			foundBase = true
		}
	}
	if !foundBase {
		t.Fatalf("mapping query = %+v", entries)
	}
	if all := f.provider.MappingTable().QueryAll(); len(all) < 15 {
		t.Fatalf("mapping query all too small: %d", len(all))
	}

	table := core.DemoMappingTable()
	for i := range table.Branches {
		if table.Branches[i].ID == "aurora" {
			conditionals := table.Branches[i].Conditionals
			table.Branches[i].Conditionals = []core.ConditionalMapping{conditionals[1], conditionals[0]}
			conditions := table.Branches[i].Conditions
			table.Branches[i].Conditions = []core.ConditionRule{conditions[0]}
		}
	}
	reordered, err := New(table)
	if err != nil {
		t.Fatal(err)
	}
	data := f.archive(t, "aurora", "sample-01")
	first, err := f.provider.LoadBytes(data, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := reordered.LoadBytes(data, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	firstFP, _ := core.Fingerprint(first.Model, f.table.Version)
	secondFP, _ := core.Fingerprint(second.Model, f.table.Version)
	if firstFP != secondFP {
		t.Fatalf("conditional declaration order changed fingerprint: %s != %s", firstFP, secondFP)
	}
}

func TestDefaultAndRejectConflictPolicies(t *testing.T) {
	f := newFixture(t)
	loaded, err := f.provider.LoadBytes(f.archive(t, "aurora", "sample-01"), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	options := ExportOptions{
		SourceBranch: "aurora", TargetBranch: "nova",
		RuleVersion: f.table.Version, PreserveConditionals: true,
	}

	defaultTable := core.DemoMappingTable()
	for i := range defaultTable.Branches {
		if defaultTable.Branches[i].ID == "nova" {
			defaultTable.Branches[i].ConflictRules = []core.ConflictPolicy{
				{Action: "default", Priority: 5},
				{Action: "preserve", Priority: 10},
			}
		}
	}
	defaultProvider, err := New(defaultTable)
	if err != nil {
		t.Fatal(err)
	}
	_, err = defaultProvider.ExportAndVerify(loaded.Model, options)
	if core.CodeOf(err) != core.CodeSemEq {
		t.Fatalf("default policy code = %s: %v", core.CodeOf(err), err)
	}
	var coreErr *core.Error
	if !errors.As(err, &coreErr) || coreErr.Semantic != "coins" || coreErr.SourceBranch != "aurora" {
		t.Fatalf("default policy mismatch context: %v", err)
	}

	rejectTable := core.DemoMappingTable()
	for i := range rejectTable.Branches {
		if rejectTable.Branches[i].ID == "nova" {
			rejectTable.Branches[i].ConflictRules = []core.ConflictPolicy{
				{Action: "reject", Priority: 1},
			}
		}
	}
	rejectProvider, err := New(rejectTable)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rejectProvider.Export(loaded.Model, options)
	if core.CodeOf(err) != core.CodeConflict {
		t.Fatalf("reject policy code = %s: %v", core.CodeOf(err), err)
	}
	if !errors.As(err, &coreErr) || coreErr.Entry != "conditionals[coins]" {
		t.Fatalf("reject context: %v", err)
	}
}

func TestExportCheckpointResume(t *testing.T) {
	f := newFixture(t)
	loaded, err := f.provider.LoadBytes(f.archive(t, "ember", "sample-01"), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	options := ExportOptions{
		SourceBranch: "ember", TargetBranch: "nova",
		RuleVersion: f.table.Version, PreserveConditionals: true,
	}
	full, err := f.provider.Export(loaded.Model, options)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "export.db")
	fresh, err := f.provider.ExportCheckpointed(context.Background(), loaded.Model, options,
		CheckpointOptions{DBPath: dbPath, WindowSize: 3})
	if err != nil {
		t.Fatalf("checkpoint export: %v", err)
	}
	if string(fresh.Archive) != string(full.Archive) {
		t.Fatalf("checkpoint export differs from full export")
	}
	resumed, err := f.provider.ExportCheckpointed(context.Background(), loaded.Model, options,
		CheckpointOptions{DBPath: dbPath, WindowSize: 3})
	if err != nil {
		t.Fatalf("resume export: %v", err)
	}
	if resumed.ResumedWindows != resumed.Windows || resumed.Windows == 0 {
		t.Fatalf("export resume count = %d/%d", resumed.ResumedWindows, resumed.Windows)
	}
	if string(resumed.Archive) != string(full.Archive) {
		t.Fatalf("resumed export differs from full export")
	}
}

func TestExportCheckpointWindowIsolation(t *testing.T) {
	f := newFixture(t)
	loaded, err := f.provider.LoadBytes(f.archive(t, "aurora", "sample-02"), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	options := ExportOptions{
		SourceBranch: "aurora", TargetBranch: "ember",
		RuleVersion: f.table.Version, PreserveConditionals: true,
	}
	full, err := f.provider.Export(loaded.Model, options)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "export.db")
	failed := false
	f.provider.windowHook = func(window int) error {
		if window == 2 && !failed {
			failed = true
			return &core.Error{Code: core.CodeSemantic, Op: "test-export-failure"}
		}
		return nil
	}
	_, err = f.provider.ExportCheckpointed(context.Background(), loaded.Model, options,
		CheckpointOptions{DBPath: dbPath, WindowSize: 3})
	if core.CodeOf(err) != core.CodeSemantic {
		t.Fatalf("export injected error = %s: %v", core.CodeOf(err), err)
	}
	f.provider.windowHook = nil
	retried, err := f.provider.ExportCheckpointed(context.Background(), loaded.Model, options,
		CheckpointOptions{DBPath: dbPath, WindowSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if retried.ResumedWindows != 2 {
		t.Fatalf("export resumed windows = %d, want 2", retried.ResumedWindows)
	}
	if string(retried.Archive) != string(full.Archive) {
		t.Fatalf("retried export differs from full export")
	}
}

func decodeInto(data []byte, value any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	return decoder.Decode(value)
}
