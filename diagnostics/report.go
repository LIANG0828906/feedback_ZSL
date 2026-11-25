package diagnostics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"skillbudget"
	"skillbudget/checkpoint"
	"skillbudget/samplegen"
)

type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

type VersionReport struct {
	Version       int    `json:"version"`
	RuleHash      string `json:"ruleHash"`
	SemanticMatch bool   `json:"semanticMatch"`
	TruthMatch    bool   `json:"truthMatch"`
	Decisions     int    `json:"decisions"`
	Detail        string `json:"detail"`
}

type RoundTripReport struct {
	Name       string `json:"name"`
	HashMatch  bool   `json:"hashMatch"`
	Losses     int    `json:"losses"`
	FillTraces int    `json:"fillTraces"`
	Detail     string `json:"detail"`
}

type PathReport struct {
	Name           string `json:"name"`
	Complete       bool   `json:"complete"`
	ResumedTick    int64  `json:"resumedTick"`
	ResumedWindows []int  `json:"resumedWindows"`
	CacheHits      []int  `json:"cacheHits,omitempty"`
	MatchTruth     bool   `json:"matchTruth"`
	Detail         string `json:"detail"`
}

type Report struct {
	Seed          int64                       `json:"seed"`
	RuleHash      string                      `json:"ruleHash"`
	TotalTicks    int64                       `json:"totalTicks"`
	WindowSize    int64                       `json:"windowSize"`
	WindowCount   int                         `json:"windowCount"`
	IntentCount   int                         `json:"intentCount"`
	DecisionCount int                         `json:"decisionCount"`
	Versions      []VersionReport             `json:"versions"`
	RoundTrips    []RoundTripReport           `json:"roundTrips"`
	Paths         []PathReport                `json:"paths"`
	Checks        []Check                     `json:"checks"`
	Conflicts     []skillcore.Decision        `json:"conflicts"`
	Invalidation  *skillcore.InvalidationPlan `json:"invalidation,omitempty"`
}

func (r *Report) Pass() bool {
	if r == nil {
		return false
	}
	for _, check := range r.Checks {
		if !check.Pass {
			return false
		}
	}
	return len(r.Checks) > 0
}

func (r *Report) FailedChecks() []Check {
	var failed []Check
	for _, check := range r.Checks {
		if !check.Pass {
			failed = append(failed, check)
		}
	}
	return failed
}

func BuildReport(seed int64) (*Report, error) {
	ctx := context.Background()
	bundle, err := samplegen.Generate(seed)
	if err != nil {
		return nil, err
	}
	report := &Report{
		Seed: seed, RuleHash: bundle.RuleHash, TotalTicks: bundle.TotalTicks,
		WindowSize:  bundle.WindowSize,
		WindowCount: int((bundle.TotalTicks + bundle.WindowSize - 1) / bundle.WindowSize),
		IntentCount: len(bundle.Intents),
	}
	for _, tick := range bundle.Truth {
		report.DecisionCount += len(tick.Decisions)
		for _, decision := range tick.Decisions {
			if decision.ConflictType != "none" && len(report.Conflicts) < 12 {
				report.Conflicts = append(report.Conflicts, decision)
			}
		}
	}
	var loaded = make(map[int]*skillcore.Project)
	for _, version := range []int{1, 2, 3} {
		project, err := skillcore.LoadVersion(bundle.Versions[version], version)
		if err != nil {
			return nil, fmt.Errorf("load version %d: %w", version, err)
		}
		loaded[version] = project
		engine, err := skillcore.NewEngine(project)
		if err != nil {
			return nil, err
		}
		result, err := engine.Run(nil, bundle.Intents, bundle.TotalTicks, skillcore.SimOptions{
			SampleEvery: 1, Workers: 4, BufferLimit: 1,
		})
		if err != nil {
			return nil, fmt.Errorf("run version %d: %w", version, err)
		}
		mismatch := skillcore.CompareSnapshots(bundle.Truth, result.Ticks)
		detail := "逐 tick 与生成器真值一致"
		if mismatch != nil {
			detail = mismatch.Error()
		}
		versionReport := VersionReport{
			Version: version, RuleHash: project.ContentHash(),
			SemanticMatch: project.ContentHash() == bundle.RuleHash,
			TruthMatch:    mismatch == nil, Decisions: report.DecisionCount, Detail: detail,
		}
		report.Versions = append(report.Versions, versionReport)
		report.Checks = append(report.Checks,
			Check{Name: fmt.Sprintf("version-%d semantic hash", version), Pass: versionReport.SemanticMatch,
				Detail: versionReport.RuleHash},
			Check{Name: fmt.Sprintf("version-%d truth", version), Pass: versionReport.TruthMatch, Detail: detail},
		)
	}

	source := loaded[3]
	toV2, err := source.Export(2)
	if err != nil {
		return nil, err
	}
	backFromV2, err := toV2.Project.Export(3)
	if err != nil {
		return nil, err
	}
	report.RoundTrips = append(report.RoundTrips, RoundTripReport{
		Name: "v3->v2->v3", HashMatch: backFromV2.Project.ContentHash() == bundle.RuleHash,
		Losses: len(toV2.Losses), FillTraces: len(backFromV2.FillTraces),
		Detail: "ms/permyriad 往返后冷却与资源语义逐项一致",
	})
	toV1, err := source.Export(1)
	if err != nil {
		return nil, err
	}
	backFromV1, err := toV1.Project.Export(3)
	if err != nil {
		return nil, err
	}
	report.RoundTrips = append(report.RoundTrips, RoundTripReport{
		Name: "v3->v1->v3", HashMatch: backFromV1.Project.ContentHash() == bundle.RuleHash,
		Losses: len(toV1.Losses), FillTraces: len(backFromV1.FillTraces),
		Detail: "tick/permille 往返后按声明默认值补齐并保留语义",
	})
	for _, trip := range report.RoundTrips {
		report.Checks = append(report.Checks, Check{
			Name: "roundtrip " + trip.Name, Pass: trip.HashMatch,
			Detail: fmt.Sprintf("losses=%d fillTraces=%d", trip.Losses, trip.FillTraces),
		})
	}

	tempDir, err := os.MkdirTemp("", "skillbudget-diagnostics-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)
	store, err := checkpoint.Open(filepath.Join(tempDir, "checkpoint.db"))
	if err != nil {
		return nil, err
	}
	defer store.Close()
	runner := checkpoint.NewRunner(store)
	base := checkpoint.RunConfig{
		WindowSize: bundle.WindowSize, TotalTicks: bundle.TotalTicks,
		Workers: 4, BufferLimit: 1, SampleEvery: 1, MaxAttempts: 1,
	}
	fullCfg := base
	fullCfg.RunID = "diagnostics-full"
	full, err := runner.Run(ctx, source, bundle.Intents, checkpoint.RunFull, fullCfg)
	if err != nil {
		return nil, err
	}
	report.Paths = append(report.Paths, pathReport("full recompute", full, bundle.Truth))

	realtimeCfg := base
	realtimeCfg.RunID = "diagnostics-realtime"
	realtime, err := runner.Run(ctx, source, bundle.Intents, checkpoint.RunRealtime, realtimeCfg)
	if err != nil {
		return nil, err
	}
	report.Paths = append(report.Paths, pathReport("realtime checkpoints", realtime, bundle.Truth))

	partialCfg := base
	partialCfg.RunID = "diagnostics-resume"
	partialCfg.StopAfterWindows = 2
	if _, err := runner.Run(ctx, source, bundle.Intents, checkpoint.RunRealtime, partialCfg); err != nil {
		return nil, err
	}
	resumeCfg := base
	resumeCfg.RunID = partialCfg.RunID
	resumed, err := runner.Run(ctx, source, bundle.Intents, checkpoint.RunResume, resumeCfg)
	if err != nil {
		return nil, err
	}
	report.Paths = append(report.Paths, pathReport("resume from SQLite", resumed, bundle.Truth))
	for _, path := range report.Paths {
		report.Checks = append(report.Checks, Check{
			Name: "path " + path.Name, Pass: path.MatchTruth && path.Complete,
			Detail: path.Detail,
		})
	}
	report.Checks = append(report.Checks, Check{
		Name: "three paths identical",
		Pass: len(resumed.ResumedWindows) == 2 &&
			skillcore.CompareSnapshots(full.Ticks, realtime.Ticks) == nil &&
			skillcore.CompareSnapshots(full.Ticks, resumed.Ticks) == nil,
		Detail: fmt.Sprintf("resumeTick=%d resumedWindows=%v", resumed.ResumedTick, resumed.ResumedWindows),
	})

	graph, err := skillcore.BuildDependencyGraph(source, bundle.Intents, bundle.TotalTicks)
	if err != nil {
		return nil, err
	}
	plan, err := graph.PlanInvalidation(bundle.WindowSize, []skillcore.Edit{{
		AtTick: 14, Kind: skillcore.EditCooldown, SkillID: "arc_nova",
		Reason: "diagnostic cooldown edit",
	}})
	if err != nil {
		return nil, err
	}
	report.Invalidation = plan
	report.Checks = append(report.Checks, Check{
		Name: "precise dependency invalidation",
		Pass: contains(plan.ReusableWindows, 0) && contains(plan.ReusableWindows, 1) &&
			!contains(plan.ReusableWindows, 2),
		Detail: fmt.Sprintf("reusable=%v affected=%v", plan.ReusableWindows, plan.AffectedWindows),
	})
	return report, nil
}

func pathReport(name string, result *checkpoint.RunResult, truth []skillcore.TickSnapshot) PathReport {
	mismatch := skillcore.CompareSnapshots(truth, result.Ticks)
	detail := "逐 tick 与真值一致"
	if mismatch != nil {
		detail = mismatch.Error()
	}
	return PathReport{
		Name: name, Complete: result.Complete, ResumedTick: result.ResumedTick,
		ResumedWindows: append([]int(nil), result.ResumedWindows...),
		CacheHits:      append([]int(nil), result.CacheHits...),
		MatchTruth:     mismatch == nil, Detail: detail,
	}
}

func contains(values []int, needle int) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
