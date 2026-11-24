// Package demo runs the repository's end-to-end acceptance scenario.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"

	"economycore/checkpoint"
	"economycore/engine"
	"economycore/format"
	"economycore/model"
	"economycore/runner"
	"economycore/sample"
)

// Check is one independently reported acceptance assertion.
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Report is a reproducible validation summary.
type Report struct {
	Seed          int64             `json:"seed"`
	OverallPass   bool              `json:"overallPass"`
	ManifestPath  string            `json:"manifestPath"`
	TruthPath     string            `json:"truthPath"`
	Checks        []Check           `json:"checks"`
	ExpectedCodes []string          `json:"expectedCodes"`
	ObservedCodes []string          `json:"observedCodes"`
	Incremental   IncrementalReport `json:"incremental"`
	Checkpoint    CheckpointReport  `json:"checkpoint"`
	ContentHash   string            `json:"contentHash"`
	TruthCycles   int               `json:"truthCycles"`
}

// IncrementalReport records the incremental/full comparison result.
type IncrementalReport struct {
	Invalidated      []string `json:"invalidated"`
	Unaffected       []string `json:"unaffected"`
	ReusedCycles     int      `json:"reusedCycles"`
	RecomputedCycles int      `json:"recomputedCycles"`
	Equal            bool     `json:"equal"`
}

// CheckpointReport records restart and isolation facts.
type CheckpointReport struct {
	ResumedFromWindow  int  `json:"resumedFromWindow"`
	ResumeComplete     bool `json:"resumeComplete"`
	FailureIsolated    bool `json:"failureIsolated"`
	RetrySucceeded     bool `json:"retrySucceeded"`
	VersionInvalidated bool `json:"versionInvalidated"`
}

// Run generates samples, performs all checks and writes validation-report.json.
func Run(ctx context.Context, dir string, seed int64) (*Report, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.NewError(model.EIO, dir, err.Error()).WithCause(err)
	}
	for _, name := range []string{"resume.db", "failure.db", "invalidate.db"} {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			path := filepath.Join(dir, name+suffix)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, model.NewError(model.EIO, path, err.Error()).WithCause(err)
			}
		}
	}
	sampleDir := filepath.Join(dir, "samples")
	manifest, err := sample.Generate(ctx, sampleDir, seed)
	if err != nil {
		return nil, err
	}
	report := &Report{
		Seed:         seed,
		ManifestPath: filepath.Join(sampleDir, "manifest.json"),
		TruthPath:    filepath.Join(sampleDir, "truth.json"),
		ContentHash:  manifest.ContentHash,
	}
	truth, err := sample.LoadTruth(report.TruthPath)
	if err != nil {
		return nil, err
	}
	report.TruthCycles = len(truth.Cycles)
	addCheck(report, "truth-cycle-count", len(truth.Cycles) == 12, fmt.Sprintf("%d cycles", len(truth.Cycles)))

	versions := []string{model.SchemaV1, model.SchemaV2, model.SchemaV3}
	loaded := make(map[string]*model.Model)
	for _, version := range versions {
		path := filepath.Join(sampleDir, version, "project.json")
		m, err := format.LoadFile(path)
		if err != nil {
			return nil, err
		}
		loaded[version] = m
		eng, err := engine.New(m, engine.Options{})
		if err != nil {
			return nil, err
		}
		run, err := eng.RunFull(ctx)
		if err != nil {
			return nil, err
		}
		equal := reflect.DeepEqual(run.Cycles, truth.Cycles) && reflect.DeepEqual(run.OverflowTotals, truth.OverflowTotals)
		addCheck(report, "load-settle-"+version, equal, fmt.Sprintf("cycles=%d overflowKeys=%d", len(run.Cycles), len(run.OverflowTotals)))
	}

	roundTrip := checkRoundTrip(report, loaded[model.SchemaV3], sampleDir)
	if roundTrip != nil {
		return nil, roundTrip
	}
	checkIncremental(report, loaded[model.SchemaV3])
	if err := checkPersistenceAndIsolation(report, loaded[model.SchemaV3], dir); err != nil {
		return nil, err
	}
	if err := checkErrors(report, loaded[model.SchemaV3], sampleDir); err != nil {
		return nil, err
	}
	report.OverallPass = true
	for _, check := range report.Checks {
		if !check.Pass {
			report.OverallPass = false
			break
		}
	}
	sort.Strings(report.ExpectedCodes)
	sort.Strings(report.ObservedCodes)

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, "validation-report.json"), data, 0o644); err != nil {
		return nil, model.NewError(model.EIO, "validation-report.json", err.Error()).WithCause(err)
	}
	return report, nil
}

func checkRoundTrip(report *Report, v3 *model.Model, sampleDir string) error {
	exported, err := format.Export(v3, model.SchemaV1, format.ExportOptions{})
	if err != nil {
		addCheck(report, "roundtrip-export-v1", false, err.Error())
		return nil
	}
	writePath := filepath.Join(sampleDir, "roundtrip-v1", "project.json")
	if err := os.MkdirAll(filepath.Dir(writePath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(writePath, append(exported.Data, '\n'), 0o644); err != nil {
		return err
	}
	reloaded, err := format.Load(exported.Data)
	if err != nil {
		return err
	}
	upgraded, roundTraces, err := format.Upgrade(reloaded, model.SchemaV3)
	if err != nil {
		return err
	}
	_, firstTraces, err := format.Upgrade(reloaded.Clone(), model.SchemaV3)
	if err != nil {
		return err
	}
	addCheck(report, "roundtrip-trace-sequence", reflect.DeepEqual(roundTraces, firstTraces), fmt.Sprintf("%d trace entries", len(roundTraces)))
	sameSemantics := upgraded.CycleMs == v3.CycleMs && reflect.DeepEqual(upgraded.InitialStocks, v3.InitialStocks)
	for _, node := range v3.Nodes {
		other := upgraded.NodeMap()[node.ID]
		if other == nil {
			sameSemantics = false
			break
		}
		rateA, _, _ := node.ConstantRate()
		rateB, _, _ := other.ConstantRate()
		if rateA != rateB || node.Recipe != other.Recipe {
			sameSemantics = false
			break
		}
	}
	addCheck(report, "roundtrip-semantics", sameSemantics, "period, stocks, rates and recipes exact")
	return nil
}

func checkIncremental(report *Report, m *model.Model) {
	eng, err := engine.New(m, engine.Options{})
	if err != nil {
		addCheck(report, "incremental-setup", false, err.Error())
		return
	}
	if _, err := eng.RunFull(context.Background()); err != nil {
		addCheck(report, "incremental-baseline", false, err.Error())
		return
	}
	edit, err := eng.EditRate("forest", 15000)
	if err != nil {
		addCheck(report, "incremental-edit", false, err.Error())
		return
	}
	incremental, err := eng.RunIncremental(context.Background())
	if err != nil {
		addCheck(report, "incremental-run", false, err.Error())
		return
	}
	fresh, err := engine.New(eng.Model(), engine.Options{})
	if err != nil {
		addCheck(report, "incremental-fresh", false, err.Error())
		return
	}
	full, err := fresh.RunFull(context.Background())
	if err != nil {
		addCheck(report, "incremental-full", false, err.Error())
		return
	}
	var invalidated []string
	for _, item := range edit.Invalidated {
		invalidated = append(invalidated, item.NodeID)
	}
	sort.Strings(invalidated)
	report.Incremental = IncrementalReport{
		Invalidated:      invalidated,
		Unaffected:       append([]string(nil), edit.Unaffected...),
		ReusedCycles:     incremental.ReusedCycles,
		RecomputedCycles: incremental.RecomputedCycles,
		Equal:            engine.CompareRuns(incremental, full) == nil,
	}
	addCheck(report, "incremental-equals-full", report.Incremental.Equal, fmt.Sprintf("reused=%d recomputed=%d", incremental.ReusedCycles, incremental.RecomputedCycles))
	addCheck(report, "incremental-precise-invalidation", containsAll(invalidated, []string{"forest", "sawmill", "workshop"}) && containsAll(edit.Unaffected, []string{"farm", "bakery"}), "forest/sawmill/workshop invalidated; farm/bakery reused")
}

func checkPersistenceAndIsolation(report *Report, m *model.Model, dir string) error {
	resumeDB := filepath.Join(dir, "resume.db")
	store, err := checkpoint.Open(resumeDB)
	if err != nil {
		return err
	}
	r, err := runner.New(m, store, runner.Options{})
	if err != nil {
		return err
	}
	if _, err := r.RunNextWindow(context.Background()); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	partial, cancelErr := r.RunAll(ctx)
	addCheck(report, "cancel-is-not-complete", model.IsCode(cancelErr, model.ECancel) && partial != nil && !partial.Complete && partial.CommittedWindows == 1, fmt.Sprintf("committed=%d complete=%v", partial.CommittedWindows, partial.Complete))
	if err := store.Close(); err != nil {
		return err
	}
	store, err = checkpoint.Open(resumeDB)
	if err != nil {
		return err
	}
	r, err = runner.New(m, store, runner.Options{})
	if err != nil {
		return err
	}
	recovery := r.Recovery()
	report.Checkpoint.ResumedFromWindow = recovery.ValidWindows
	resumed, err := r.RunAll(context.Background())
	if err != nil {
		return err
	}
	report.Checkpoint.ResumeComplete = resumed.Complete
	addCheck(report, "restart-resume", recovery.ValidWindows == 1 && resumed.Complete && resumed.CommittedWindows == 3, fmt.Sprintf("restored=%d committed=%d", recovery.ValidWindows, resumed.CommittedWindows))
	store.Close()

	failureDB := filepath.Join(dir, "failure.db")
	store, err = checkpoint.Open(failureDB)
	if err != nil {
		return err
	}
	var failOnce atomic.Bool
	failOnce.Store(true)
	r, err = runner.New(m, store, runner.Options{Fault: func(window int, cycle int64) error {
		if window == 1 && failOnce.CompareAndSwap(true, false) {
			return model.NewError(model.EUnit, "/injected", "injected internal failure").WithWindow(window).WithNode("sawmill", "")
		}
		return nil
	}})
	if err != nil {
		return err
	}
	if _, err := r.RunNextWindow(context.Background()); err != nil {
		return err
	}
	_, failure := r.RunNextWindow(context.Background())
	countAfterFailure, _ := store.Count(context.Background())
	failures, _ := store.Failures(context.Background())
	report.Checkpoint.FailureIsolated = model.IsCode(failure, model.EUnit) && countAfterFailure == 1 && len(failures) == 1
	_, retryErr := r.RunNextWindow(context.Background())
	countAfterRetry, _ := store.Count(context.Background())
	report.Checkpoint.RetrySucceeded = retryErr == nil && countAfterRetry == 2
	addCheck(report, "failed-window-isolated", report.Checkpoint.FailureIsolated, "checkpoint prefix stayed at window 0")
	addCheck(report, "failed-window-retry", report.Checkpoint.RetrySucceeded, "retry committed window 1")
	store.Close()

	invalidateDB := filepath.Join(dir, "invalidate.db")
	store, err = checkpoint.Open(invalidateDB)
	if err != nil {
		return err
	}
	r, err = runner.New(m, store, runner.Options{})
	if err != nil {
		return err
	}
	if _, err := r.RunAll(context.Background()); err != nil {
		return err
	}
	store.Close()
	changed := m.Clone()
	changed.Name += " (changed)"
	store, err = checkpoint.Open(invalidateDB)
	if err != nil {
		return err
	}
	r, err = runner.New(changed, store, runner.Options{})
	if err != nil {
		return err
	}
	invalidationRecovery := r.Recovery()
	countAfterInvalidation, _ := store.Count(context.Background())
	report.Checkpoint.VersionInvalidated = invalidationRecovery.Invalidated && countAfterInvalidation == 0
	addCheck(report, "global-version-invalidation", report.Checkpoint.VersionInvalidated, invalidationRecovery.InvalidationReason)
	store.Close()
	return nil
}

func checkErrors(report *Report, m *model.Model, sampleDir string) error {
	expected := []string{
		string(model.EVer), string(model.ESchema), string(model.ELossy), string(model.EOvf),
		string(model.EDiff), string(model.EUnit), string(model.ERes),
	}
	report.ExpectedCodes = expected

	_, verErr := format.Load([]byte(`{"schema_version":"economy-v999"}`))
	recordCode(report, "error-E_VER", model.EVer, verErr)

	missing := []byte(`{"schema_version":"economy-v1","project_id":"x"}`)
	_, schemaErr := format.Load(missing)
	recordCode(report, "error-E_SCHEMA", model.ESchema, schemaErr)

	lossy := m.Clone()
	lossy.UpgradeDiscountBasisPoints = 1500
	lossy.OverflowPolicy = "reject"
	_, lossyErr := format.Export(lossy, model.SchemaV1, format.ExportOptions{})
	recordCode(report, "error-E_LOSSY", model.ELossy, lossyErr)

	overflowModel := m.Clone()
	overflowModel.Intents = nil
	overflowModel.Nodes[0].RateExpr = model.ConstExpr(math.MaxInt64 / 2)
	overflowModel.Nodes[0].Workers = 2
	overflowEngine, err := engine.New(overflowModel, engine.Options{})
	if err != nil {
		return err
	}
	_, overflowErr := overflowEngine.RunFull(context.Background())
	recordCode(report, "error-E_OVF", model.EOvf, overflowErr)

	unitModel := m.Clone()
	unitModel.Intents = []model.Intent{{Offset: 0, Window: 0, Cycle: 0, Action: model.IntentUpgrade, NodeID: "ghost"}}
	unitEngine, err := engine.New(unitModel, engine.Options{})
	if err != nil {
		return err
	}
	_, unitErr := unitEngine.RunFull(context.Background())
	recordCode(report, "error-E_UNIT", model.EUnit, unitErr)

	resModel := m.Clone()
	for _, node := range resModel.Nodes {
		if node.ID == "forest" {
			node.Upgrade.Cost = map[string]int64{"wood": 999999}
		}
	}
	resModel.Intents = []model.Intent{{Offset: 0, Window: 0, Cycle: 0, Action: model.IntentUpgrade, NodeID: "forest"}}
	resEngine, err := engine.New(resModel, engine.Options{})
	if err != nil {
		return err
	}
	_, resErr := resEngine.RunFull(context.Background())
	recordCode(report, "error-E_RES", model.ERes, resErr)

	baseEngine, err := engine.New(m, engine.Options{})
	if err != nil {
		return err
	}
	left, err := baseEngine.RunFull(context.Background())
	if err != nil {
		return err
	}
	right := *left
	right.Cycles = append([]engine.CycleSnapshot(nil), left.Cycles...)
	right.Cycles[0].GlobalStocks = cloneIntMap(left.Cycles[0].GlobalStocks)
	right.Cycles[0].GlobalStocks["wood"]++
	diffErr := engine.CompareRuns(left, &right)
	recordCode(report, "error-E_DIFF", model.EDiff, diffErr)

	_ = sampleDir
	return nil
}

func recordCode(report *Report, checkName string, code model.Code, err error) {
	pass := model.IsCode(err, code)
	detail := ""
	if typed, ok := model.AsError(err); ok {
		detail = typed.Error()
	} else if err != nil {
		detail = err.Error()
	}
	addCheck(report, checkName, pass, detail)
	if pass {
		report.ObservedCodes = append(report.ObservedCodes, string(code))
	}
}

func addCheck(report *Report, name string, pass bool, detail string) {
	report.Checks = append(report.Checks, Check{Name: name, Pass: pass, Detail: detail})
}

func containsAll(haystack, needles []string) bool {
	set := make(map[string]bool, len(haystack))
	for _, item := range haystack {
		set[item] = true
	}
	for _, needle := range needles {
		if !set[needle] {
			return false
		}
	}
	return true
}

func cloneIntMap(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// Summary returns a compact human-readable result.
func (r *Report) Summary() string {
	failed := 0
	for _, check := range r.Checks {
		if !check.Pass {
			failed++
		}
	}
	codes := append([]string(nil), r.ObservedCodes...)
	sort.Strings(codes)
	var b strings.Builder
	fmt.Fprintf(&b, "overall=%v checks=%d failed=%d seed=%d\n", r.OverallPass, len(r.Checks), failed, r.Seed)
	fmt.Fprintf(&b, "cycles=%d incremental-reused=%d incremental-recomputed=%d\n", r.TruthCycles, r.Incremental.ReusedCycles, r.Incremental.RecomputedCycles)
	fmt.Fprintf(&b, "errors=%s\n", strings.Join(codes, ","))
	return b.String()
}
