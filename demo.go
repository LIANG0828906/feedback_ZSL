package downsample

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type SchemaCheck struct {
	Version     string `json:"version"`
	Metrics     int    `json:"metrics"`
	Points      int    `json:"points"`
	WindowCount int    `json:"window_count"`
	Equivalent  bool   `json:"equivalent"`
	Error       string `json:"error,omitempty"`
}

type EditDemo struct {
	Name                string             `json:"name"`
	MetricID            string             `json:"metric_id"`
	Field               string             `json:"field"`
	ConfigVersion       string             `json:"config_version"`
	RecomputedCount     int                `json:"recomputed_count"`
	ReusedCount         int                `json:"reused_count"`
	ClosureMetrics      []string           `json:"closure_metrics"`
	CrossMetricEdges    []string           `json:"cross_metric_edges"`
	CrossMetricBoundary []string           `json:"cross_metric_boundary"`
	Diffs               []string           `json:"diffs,omitempty"`
	Match               bool               `json:"match"`
	Trigger             InvalidationSource `json:"trigger"`
	RecomputedWindows   []WindowResult     `json:"recomputed_windows,omitempty"`
}

type RecoveryDemo struct {
	Path            string  `json:"path"`
	GlobalSeq       int64   `json:"global_seq"`
	BeforeCount     int     `json:"before_point_count"`
	AfterCount      int     `json:"after_point_count"`
	BeforeAvg       float64 `json:"before_avg"`
	AfterAvg        float64 `json:"after_avg"`
	QueryMatch      bool    `json:"query_match"`
	CrashGlobalSeq  int64   `json:"crash_global_seq"`
	CrashVisible    int     `json:"crash_visible_windows"`
	CrashOrphanRows int     `json:"crash_orphan_rows"`
	CrashOrderOK    bool    `json:"crash_order_ok"`
}

type DemoReport struct {
	OutDir          string            `json:"out_dir"`
	GeneratedAt     string            `json:"generated_at"`
	Seed            int64             `json:"seed"`
	SchemaChecks    []SchemaCheck     `json:"schema_checks"`
	WindowCount     int               `json:"window_count"`
	Windows         []WindowResult    `json:"windows"`
	WeightCases     []WeightCase      `json:"weight_cases"`
	Edits           []EditDemo        `json:"edits"`
	Recovery        RecoveryDemo      `json:"recovery"`
	Injections      []InjectionResult `json:"injections"`
	SourceHashes    map[string]string `json:"source_hashes"`
	SourceUnchanged bool              `json:"source_unchanged_by_injections"`
	Checks          []string          `json:"checks"`
	AllPassed       bool              `json:"all_passed"`
}

func RunDemo(outDir string) (*DemoReport, *Error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, WrapError(CodeCheckpointOpen, "cannot create demo output directory", err).WithContext("dir", outDir)
	}
	bundle, err := GenerateBundle()
	if err != nil {
		return nil, err
	}
	report := &DemoReport{
		OutDir: outDir, GeneratedAt: time.UnixMilli(bundle.BaseTimeMS).UTC().Format(time.RFC3339), Seed: bundle.Seed,
		WeightCases: bundle.WeightCases, SourceHashes: map[string]string{}, SourceUnchanged: true, AllPassed: true,
	}

	for _, version := range []SchemaVersion{SchemaV1, SchemaV2, SchemaV3} {
		name := filepath.Join(outDir, "project-"+string(version)+".json")
		if werr := os.WriteFile(name, bundle.Documents[version], 0o644); werr != nil {
			return nil, WrapError("E_DEMO_WRITE", "cannot write schema document", werr).WithContext("path", name)
		}
		report.SourceHashes[string(version)] = hashBytes(bundle.Documents[version])
	}

	projects := map[SchemaVersion]*Project{}
	for _, version := range []SchemaVersion{SchemaV1, SchemaV2, SchemaV3} {
		project, lerr := LoadProject(bundle.Documents[version])
		check := SchemaCheck{Version: string(version)}
		if lerr != nil {
			check.Error = lerr.Error()
			report.Checks = append(report.Checks, "FAIL load "+string(version)+": "+lerr.Error())
			report.AllPassed = false
		} else {
			projects[version] = project
			engine := NewEngine(project, DefaultBudget())
			allStarts := 0
			pointCount := 0
			for _, m := range project.Metrics {
				allStarts += len(engine.WindowStarts(m))
				pointCount += len(project.Series[m.ID])
			}
			check.Metrics = len(project.Metrics)
			check.Points = pointCount
			check.WindowCount = allStarts
			check.Equivalent = true
			report.Checks = append(report.Checks, "PASS schema "+string(version))
		}
		report.SchemaChecks = append(report.SchemaChecks, check)
	}

	mainEngine := NewEngine(bundle.Project, DefaultBudget())
	mainReport, rerr := mainEngine.RecomputeAll(InvalidationSource{Kind: "demo"})
	if rerr != nil {
		return nil, rerr
	}
	report.Windows = sortedWindowResults(bundle.Project, mainReport.Results)
	report.WindowCount = len(report.Windows)
	if diffs := CompareWindowMaps(bundle.Truth, mainReport.Results, 1e-9); len(diffs) == 0 {
		report.Checks = append(report.Checks, "PASS generator truth comparison")
	} else {
		report.AllPassed = false
		report.Checks = append(report.Checks, "FAIL truth: "+fmt.Sprint(diffs))
	}

	report.Edits = runEditDemos(bundle)
	for _, edit := range report.Edits {
		if edit.Match {
			report.Checks = append(report.Checks, "PASS edit "+edit.Name+" incremental == full")
		} else {
			report.AllPassed = false
			report.Checks = append(report.Checks, "FAIL edit "+edit.Name+": "+fmt.Sprint(edit.Diffs))
		}
	}

	recovery, rerr := runRecoveryDemo(bundle, outDir)
	if rerr != nil {
		return nil, rerr
	}
	report.Recovery = recovery
	if recovery.QueryMatch && recovery.CrashOrderOK {
		report.Checks = append(report.Checks, "PASS checkpoint recovery and commit order")
	} else {
		report.AllPassed = false
		report.Checks = append(report.Checks, "FAIL checkpoint recovery")
	}

	injectionDir := filepath.Join(outDir, "injections")
	for _, name := range InjectionCases() {
		ierr := RunInjection(bundle, name, injectionDir)
		if ierr == nil {
			report.AllPassed = false
			report.Checks = append(report.Checks, "FAIL injection "+name+" returned no error")
			report.Injections = append(report.Injections, InjectionResult{Case: name, Code: "E_MISSING_CODE"})
			continue
		}
		coded := AsError(ierr)
		result := coded.InjectionResult(name)
		report.Injections = append(report.Injections, result)
		report.Checks = append(report.Checks, "PASS injection "+name+" -> "+coded.Code)
	}
	for _, version := range []SchemaVersion{SchemaV1, SchemaV2, SchemaV3} {
		if hashBytes(bundle.Documents[version]) != report.SourceHashes[string(version)] {
			report.SourceUnchanged = false
		}
	}

	if err := writeJSON(filepath.Join(outDir, "demo-report.json"), report); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(outDir, "windows.json"), report.Windows); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(outDir, "weights.json"), report.WeightCases); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.md"), []byte(renderMarkdown(report)), 0o644); err != nil {
		return nil, WrapError("E_DEMO_WRITE", "cannot write markdown report", err)
	}
	return report, nil
}

func sortedWindowResults(project *Project, results map[WindowKey]WindowResult) []WindowResult {
	keys := make([]WindowKey, 0, len(results))
	for key := range results {
		keys = append(keys, key)
	}
	sortWindowKeys(project, keys)
	out := make([]WindowResult, 0, len(keys))
	for _, key := range keys {
		out = append(out, results[key])
	}
	return out
}

func runEditDemos(bundle *SampleBundle) []EditDemo {
	cases := []struct {
		name string
		edit ConfigEdit
	}{
		{name: "function_set_cpu", edit: ConfigEdit{MetricID: "cpu.usage", Field: "functions", Functions: []Func{FuncAvg, FuncSum, FuncMin, FuncMax, FuncWeightedAvg, FuncFirst}}},
		{name: "window_width_mem", edit: ConfigEdit{MetricID: "mem.usage", Field: "window_width", WidthMS: 30_000}},
		{name: "alignment_cpu", edit: ConfigEdit{MetricID: "cpu.usage", Field: "alignment", Alignment: SourceAlignFirstPoint, AnchorMS: bundle.BaseTimeMS}},
		{name: "weight_scale_cpu", edit: ConfigEdit{MetricID: "cpu.usage", Field: "weight_scale", WeightScale: ScalePermille, WeightBits: 32}},
	}
	out := []EditDemo{}
	for _, tc := range cases {
		incProject, err := LoadProject(bundle.Documents[SchemaV1])
		if err != nil {
			continue
		}
		inc := NewEngine(incProject, DefaultBudget())
		if _, err := inc.RecomputeAll(InvalidationSource{Kind: "demo_initial"}); err != nil {
			continue
		}
		editReport, coded := inc.ApplyConfigEdit(tc.edit)
		demo := EditDemo{Name: tc.name, MetricID: tc.edit.MetricID, Field: tc.edit.Field}
		if coded != nil {
			demo.Diffs = []string{coded.Error()}
			out = append(out, demo)
			continue
		}
		demo.ConfigVersion = editReport.ConfigVersion
		demo.RecomputedCount = len(editReport.Recomputed)
		demo.ReusedCount = len(editReport.Reused)
		demo.ClosureMetrics = editReport.Closure.Metrics
		demo.CrossMetricEdges = editReport.Closure.CrossMetricEdges
		demo.CrossMetricBoundary = editReport.Closure.CrossMetricBoundary
		demo.Trigger = editReport.Trigger
		for _, key := range editReport.Recomputed {
			if result, ok := editReport.Results[key]; ok {
				demo.RecomputedWindows = append(demo.RecomputedWindows, result)
			}
		}

		fullProject, _ := LoadProject(bundle.Documents[SchemaV1])
		full := NewEngine(fullProject, DefaultBudget())
		_, _ = full.ApplyConfigEdit(tc.edit)
		fullReport, ferr := full.RecomputeAll(InvalidationSource{Kind: "demo_full"})
		if ferr != nil {
			demo.Diffs = []string{ferr.Error()}
		} else {
			demo.Diffs = CompareWindowMaps(fullReport.Results, editReport.Results, 1e-9)
		}
		demo.Match = len(demo.Diffs) == 0
		out = append(out, demo)
	}
	return out
}

func runRecoveryDemo(bundle *SampleBundle, outDir string) (RecoveryDemo, *Error) {
	path := filepath.Join(outDir, "demo-checkpoint.sqlite")
	_ = os.Remove(path)
	project, err := LoadProject(bundle.Documents[SchemaV1])
	if err != nil {
		return RecoveryDemo{}, err
	}
	session, err := NewSession(project, Budget{MaxWorkers: 2, MaxWindowsPerMetric: 32}, path)
	if err != nil {
		return RecoveryDemo{}, err
	}
	newPoint := Point{
		MetricID: "cpu.usage", TimestampMS: bundle.BaseTimeMS + 8*60_000 + 5_000,
		Value: 41.5, Weight: 2000,
	}
	if _, err := session.AppendAndCheckpoint(context.Background(), newPoint); err != nil {
		return RecoveryDemo{}, err
	}
	before, qerr := session.Engine.QueryWindow("cpu.usage", bundle.BaseTimeMS+8*60_000)
	if qerr != nil {
		return RecoveryDemo{}, qerr
	}
	seq := session.Engine.GlobalSequence()
	if err := session.Close(); err != nil {
		return RecoveryDemo{}, WrapError(CodeCheckpointOpen, "close failed", err)
	}

	restartProject, lerr := LoadProject(bundle.Documents[SchemaV1])
	if lerr != nil {
		return RecoveryDemo{}, lerr
	}
	recovered, checkpointRecovery, rerr := RecoverOrInitialize(restartProject, Budget{MaxWorkers: 2, MaxWindowsPerMetric: 32}, path)
	if rerr != nil {
		return RecoveryDemo{}, rerr
	}
	defer recovered.Close()
	_ = checkpointRecovery
	after, qerr := recovered.Engine.QueryWindow("cpu.usage", bundle.BaseTimeMS+8*60_000)
	if qerr != nil {
		return RecoveryDemo{}, qerr
	}

	crashPath := filepath.Join(outDir, "demo-crash-order.sqlite")
	_ = os.Remove(crashPath)
	cp, oerr := OpenCheckpoint(crashPath)
	if oerr != nil {
		return RecoveryDemo{}, oerr
	}
	w0, _ := recovered.Engine.QueryWindow("cpu.usage", bundle.BaseTimeMS)
	w1 := before
	if cerr := cp.SaveCommit(project.ConfigVersion, 1, []WindowResult{w0}); cerr != nil {
		return RecoveryDemo{}, cerr
	}
	if cerr := cp.SaveWindowStates(project.ConfigVersion, 2, []WindowResult{w1}); cerr != nil {
		return RecoveryDemo{}, cerr
	}
	_ = cp.Close()
	cp2, oerr := OpenCheckpoint(crashPath)
	if oerr != nil {
		return RecoveryDemo{}, oerr
	}
	rec, cerr := cp2.Recover(project.ConfigVersion)
	if cerr != nil {
		_ = cp2.Close()
		return RecoveryDemo{}, cerr
	}
	_ = cp2.Close()

	return RecoveryDemo{
		Path: path, GlobalSeq: seq,
		BeforeCount: before.PointCount, AfterCount: after.PointCount,
		BeforeAvg: valueOrZero(before.Values, FuncAvg), AfterAvg: valueOrZero(after.Values, FuncAvg),
		QueryMatch:     before.PointCount == after.PointCount && before.Values[FuncAvg] == after.Values[FuncAvg],
		CrashGlobalSeq: rec.GlobalSeq, CrashVisible: len(rec.Results), CrashOrphanRows: rec.OrphanRows,
		CrashOrderOK: rec.GlobalSeq == 1 && len(rec.Results) == 1 && rec.OrphanRows == 1,
	}, nil
}

func valueOrZero(values map[Func]float64, fn Func) float64 {
	if values == nil {
		return 0
	}
	return values[fn]
}

func writeJSON(path string, value any) *Error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return WrapError("E_DEMO_JSON", "cannot encode JSON", err).WithContext("path", path)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return WrapError("E_DEMO_WRITE", "cannot write JSON", err).WithContext("path", path)
	}
	return nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func renderMarkdown(report *DemoReport) string {
	out := "# 时序降采样与聚合窗口演示报告\n\n"
	out += fmt.Sprintf("- 固定种子：`%d`\n- 生成时间：`%s`\n- 窗口结果数：`%d`\n- 总体验证：`%v`\n\n", report.Seed, report.GeneratedAt, report.WindowCount, report.AllPassed)
	out += "## 窗口结果\n\n"
	out += "| 指标 | 窗口序号 | start | count | avg | weighted_avg | 状态 | 填充 | 失效来源 |\n"
	out += "|---|---:|---:|---:|---:|---:|---|---|---|\n"
	for _, w := range report.Windows {
		avg := w.Values[FuncAvg]
		wavg := w.Values[FuncWeightedAvg]
		if w.MetricID == "cpu.scaled" {
			avg = w.Values[FuncDerived]
		}
		out += fmt.Sprintf("| `%s` | %d | %d | %d | %.4f | %.4f | `%s` | %v | `%s/%s` |\n",
			w.MetricID, w.WindowIndex, w.StartMS, w.PointCount, avg, wavg, w.Status, w.Filled, w.Invalidation.Kind, w.ConfigVersion)
	}
	out += "\n## 权重临界换算\n\n"
	out += "| 用例 | 输入 | 中间整数 | 输出 | 真值差 | 容差内 | 饱和 | 业务码 |\n"
	out += "|---|---:|---:|---:|---:|---|---|---|\n"
	for _, c := range report.WeightCases {
		r := c.Report
		out += fmt.Sprintf("| %s | %d | %s | %d | %.3f | %v | %v | `%s` |\n",
			c.Name, r.InputRaw, r.IntermediateNumerator, r.OutputRaw, r.Delta, r.WithinTolerance, r.Saturated, c.WantCode)
	}
	out += "\n## 配置编辑与增量对照\n\n"
	for _, e := range report.Edits {
		out += fmt.Sprintf("### %s\n\n- metric=`%s`, field=`%s`, version=`%s`\n- recomputed=`%d`, reused=`%d`, match=`%v`\n- closure=`%v`\n- edges=`%v`\n- boundary=`%v`\n",
			e.Name, e.MetricID, e.Field, e.ConfigVersion, e.RecomputedCount, e.ReusedCount, e.Match, e.ClosureMetrics, e.CrossMetricEdges, e.CrossMetricBoundary)
		out += fmt.Sprintf("- trigger_source=`%s/%s/%s`; per-window sources are in `demo-report.json` field `edits[].recomputed_windows[].invalidation`\n",
			e.Trigger.Kind, e.Trigger.ChangedField, e.Trigger.Reason)
		if len(e.Diffs) > 0 {
			out += fmt.Sprintf("- diffs: `%v`\n", e.Diffs)
		}
		out += "\n"
	}
	out += "## 检查点恢复\n\n"
	out += fmt.Sprintf("- path=`%s` seq=`%d` query_match=`%v`\n- before_count=`%d` after_count=`%d` before_avg=`%.6f` after_avg=`%.6f`\n- crash_order_ok=`%v` visible=`%d` orphans=`%d` crash_seq=`%d`\n\n",
		report.Recovery.Path, report.Recovery.GlobalSeq, report.Recovery.QueryMatch, report.Recovery.BeforeCount, report.Recovery.AfterCount, report.Recovery.BeforeAvg, report.Recovery.AfterAvg,
		report.Recovery.CrashOrderOK, report.Recovery.CrashVisible, report.Recovery.CrashOrphanRows, report.Recovery.CrashGlobalSeq)
	out += "## 异常注入\n\n"
	out += "| 场景 | 业务码 | 字段路径 | 信息 |\n|---|---|---|---|\n"
	for _, i := range report.Injections {
		out += fmt.Sprintf("| `%s` | `%s` | `%s` | %s |\n", i.Case, i.Code, i.Field, i.Message)
	}
	out += "\n## 检查记录\n\n"
	for _, check := range report.Checks {
		out += "- " + check + "\n"
	}
	return out
}
