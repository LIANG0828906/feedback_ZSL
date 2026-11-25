package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"maskqa/internal/checkpoint"
	"maskqa/internal/deps"
	"maskqa/internal/model"
	"maskqa/internal/runner"
	"maskqa/internal/sample"
	"maskqa/internal/schemaio"
	"maskqa/internal/topology"
)

type Report struct {
	OK           bool              `json:"ok"`
	GoVersion    string            `json:"go_version"`
	Seed         int64             `json:"seed"`
	DataDir      string            `json:"data_dir"`
	SchemaChecks []SchemaCheck     `json:"schema_checks"`
	PathChecks   []PathCheck       `json:"path_checks"`
	Incremental  *IncrementalCheck `json:"incremental,omitempty"`
	Errors       []string          `json:"errors,omitempty"`
}

type SchemaCheck struct {
	ProjectID         string `json:"project_id"`
	Version           string `json:"version"`
	StrokeCount       int    `json:"stroke_count"`
	LoadedCount       int    `json:"loaded_count"`
	OK                bool   `json:"ok"`
	MaxWeightResidual int    `json:"max_weight_residual_bp"`
	Error             string `json:"error,omitempty"`
}

type PathCheck struct {
	ProjectID string `json:"project_id"`
	model.PathComparison
	Width          int      `json:"width"`
	Height         int      `json:"height"`
	StrokeCount    int      `json:"stroke_count"`
	DomainCount    int      `json:"domain_count"`
	TruthPassed    bool     `json:"truth_passed"`
	KilledAfter    int      `json:"killed_after"`
	CommitSequence []string `json:"commit_sequence"`
	QueueEvents    int      `json:"queue_events"`
	QueueWaitMs    int64    `json:"queue_wait_ms"`
	QueueWaitNanos int64    `json:"queue_wait_nanos"`
}

type IncrementalCheck struct {
	ProjectID      string                 `json:"project_id"`
	EditedStrokeID string                 `json:"edited_stroke_id"`
	Plan           model.InvalidationPlan `json:"plan"`
	ActualReused   []string               `json:"actual_reused_chunks"`
	ExpectedReused []string               `json:"expected_reused_chunks"`
	OK             bool                   `json:"ok"`
	Error          string                 `json:"error,omitempty"`
}

func RunAll(ctx context.Context, dataDir string, seed int64, parallel int) (*Report, error) {
	if parallel <= 0 {
		parallel = 4
	}
	rep := &Report{OK: true, GoVersion: runtime.Version(), Seed: seed, DataDir: dataDir}
	assets := sample.GenerateAll(seed)
	if _, err := sample.WriteWorkspace(dataDir, seed, assets); err != nil {
		return nil, err
	}

	for _, asset := range assets {
		p := asset.Project
		eng := &schemaio.Engineering{ProjectID: p.ID, Seed: p.Seed, Width: p.Width, Height: p.Height, ChunkSize: p.ChunkSize, Strokes: p.Strokes}
		for _, version := range []string{schemaio.V1, schemaio.V2, schemaio.V3} {
			check := SchemaCheck{ProjectID: p.ID, Version: version, StrokeCount: len(p.Strokes), OK: true}
			raw, err := schemaio.MarshalDocument(eng, version)
			if err != nil {
				check.OK = false
				check.Error = err.Error()
				rep.addError("schema marshal %s/%s: %v", p.ID, version, err)
				rep.SchemaChecks = append(rep.SchemaChecks, check)
				continue
			}
			loaded, err := schemaio.LoadEngineering(raw)
			if err != nil {
				check.OK = false
				check.Error = err.Error()
				rep.addError("schema load %s/%s: %v", p.ID, version, err)
				rep.SchemaChecks = append(rep.SchemaChecks, check)
				continue
			}
			check.LoadedCount = len(loaded.Strokes)
			for _, st := range p.Strokes {
				rt := schemaio.VerifyRoundTrip(st, version, *eng)
				if rt.WeightResidualBP > check.MaxWeightResidual {
					check.MaxWeightResidual = rt.WeightResidualBP
				}
				if !rt.OK {
					check.OK = false
					if rt.Error != nil {
						check.Error = rt.Error.Error()
					}
					rep.addError("roundtrip %s/%s/%s: %s", p.ID, version, st.ID, check.Error)
				}
			}
			rep.SchemaChecks = append(rep.SchemaChecks, check)
		}

		cfg := runner.DefaultConfig("verify")
		cfg.MaxParallel = parallel
		cfg.QueueBudget = 2
		cfg.KillAfterCommits = 7
		three, err := runner.ExecuteThreePaths(ctx, p, filepath.Join(dataDir, "checkpoints", "three-path"), cfg)
		pc := PathCheck{ProjectID: p.ID, Width: p.Width, Height: p.Height, StrokeCount: len(p.Strokes), KilledAfter: 7}
		if err != nil {
			rep.addError("three paths %s: %v", p.ID, err)
			pc.PathComparison = model.PathComparison{Equal: false, Message: err.Error()}
			var me *model.Error
			if errors.As(err, &me) && me.Code == model.CodeTop {
				pc.TruthPassed = false
			}
		} else {
			pc.PathComparison = three.Comparison
			pc.DomainCount = three.Resume.Topology.DomainCount
			pc.TruthPassed = three.Resume.FinalMask.Equal(p.Reference) && topology.VerifyAgainstTruth(three.Resume.Topology, p.Truth, topology.DefaultTolerances()) == nil
			pc.CommitSequence = three.Resume.CommitSequence
			pc.QueueEvents = len(three.Resume.QueueEvents)
			for _, ev := range three.Resume.QueueEvents {
				pc.QueueWaitMs += ev.WaitMillis
				pc.QueueWaitNanos += ev.WaitNanos
			}
			if !three.Comparison.Equal || !pc.TruthPassed {
				rep.addError("pixel/topology verification failed for %s: %s", p.ID, three.Comparison.Message)
			}
		}
		rep.PathChecks = append(rep.PathChecks, pc)
	}

	inc, err := runIncrementalCheck(ctx, assets[0].Project, filepath.Join(dataDir, "checkpoints", "verify-incremental.db"))
	rep.Incremental = inc
	if err != nil {
		rep.addError("incremental harness: %v", err)
	}
	if inc != nil && !inc.OK {
		rep.addError("incremental invalidation failed for %s: %s", inc.ProjectID, inc.Error)
	}
	rep.OK = len(rep.Errors) == 0
	return rep, nil
}

func runIncrementalCheck(ctx context.Context, p *model.Project, dbPath string) (*IncrementalCheck, error) {
	removeDB(dbPath)
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	cfg := runner.DefaultConfig("verify-incremental")
	if _, err := runner.Run(ctx, p, store, cfg, nil); err != nil {
		_ = store.Close()
		return nil, err
	}
	edited := *p
	edited.Strokes = append([]model.Stroke(nil), p.Strokes...)
	edited.Strokes[0].WeightBP += 50
	edited.Strokes[0].ContentHash = schemaio.StrokeHash(edited.Strokes[0])
	plan := deps.PlanEdit(p, &edited)
	result, runErr := runner.Run(ctx, &edited, store, cfg, &plan)
	check := &IncrementalCheck{ProjectID: p.ID, EditedStrokeID: edited.Strokes[0].ID, Plan: plan, ExpectedReused: plan.ReusedChunks, OK: true}
	if runErr != nil {
		check.OK = false
		check.Error = runErr.Error()
	}
	if result != nil {
		for _, c := range result.Chunks {
			if c.Reused {
				check.ActualReused = append(check.ActualReused, c.ChunkID)
			}
		}
	}
	if len(check.ActualReused) != len(check.ExpectedReused) {
		check.OK = false
		if check.Error == "" {
			check.Error = "reused chunk count does not match plan"
		}
	}
	_ = store.Close()
	return check, nil
}

func (r *Report) addError(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	r.OK = false
}

func (r *Report) WriteJSON(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func (r *Report) Markdown() string {
	var b strings.Builder
	b.WriteString("# 实际验证结果\n\n")
	b.WriteString("- Go: `" + r.GoVersion + "`\n")
	b.WriteString("- 固定种子: `")
	b.WriteString(fmt.Sprintf("%d", r.Seed))
	b.WriteString("`\n- 数据目录: `" + r.DataDir + "`\n- 总结论: ")
	if r.OK {
		b.WriteString("**PASS**\n\n")
	} else {
		b.WriteString("**FAIL**\n\n")
	}
	b.WriteString("## Schema 三版本装载与往返\n\n| 图标 | 版本 | 笔画 | 装载 | 最大权重残差(bp) | 结果 |\n|---|---:|---:|---:|---:|---|\n")
	for _, c := range r.SchemaChecks {
		status := "PASS"
		if !c.OK {
			status = "FAIL: " + c.Error
		}
		b.WriteString(fmt.Sprintf("| `%s` | `%s` | %d | %d | %d | %s |\n", c.ProjectID, c.Version, c.StrokeCount, c.LoadedCount, c.MaxWeightResidual, status))
	}
	b.WriteString("\n## 三路径与拓扑\n\n| 图标 | 分辨率 | 笔画/域 | 击杀边界 | 比对 | 真值 | 排队事件/ms/ns | 提交序列 |\n|---|---:|---:|---:|---|---|---:|---:|\n")
	for _, c := range r.PathChecks {
		status := "PASS"
		if !c.Equal {
			status = "FAIL: " + c.Message
		}
		truth := "PASS"
		if !c.TruthPassed {
			truth = "FAIL"
		}
		b.WriteString(fmt.Sprintf("| `%s` | %dx%d | %d/%d | %d | %s | %s | %d/%dms/%dns | %d |\n", c.ProjectID, c.Width, c.Height, c.StrokeCount, c.DomainCount, c.KilledAfter, status, truth, c.QueueEvents, c.QueueWaitMs, c.QueueWaitNanos, len(c.CommitSequence)))
	}
	if r.Incremental != nil {
		c := r.Incremental
		b.WriteString("\n## 增量失效\n\n")
		b.WriteString(fmt.Sprintf("- 编辑笔画: `%s`\n- 受影响笔画: `%s`\n- 受影响分块: `%s`\n- 计划复用: `%s`\n- 实际复用: `%s`\n- 结果: **%v** %s\n", c.EditedStrokeID, strings.Join(c.Plan.AffectedStrokes, ","), strings.Join(c.Plan.AffectedChunks, ","), strings.Join(c.ExpectedReused, ","), strings.Join(c.ActualReused, ","), c.OK, c.Error))
	}
	if len(r.Errors) > 0 {
		b.WriteString("\n## 错误\n\n")
		for _, e := range r.Errors {
			b.WriteString("- " + e + "\n")
		}
	}
	return b.String()
}

func removeDB(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
