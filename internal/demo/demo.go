package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"samplingkernel/internal/engine"
	"samplingkernel/internal/errs"
	"samplingkernel/internal/exceptions"
	"samplingkernel/internal/model"
	"samplingkernel/internal/runtime"
	"samplingkernel/internal/sample"
	"samplingkernel/internal/store"
	"samplingkernel/internal/verify"
)

func Run(stdout io.Writer, outDir, dbPath string) error {
	ctx := context.Background()
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if dbPath == "" {
		dbPath = filepath.Join(outDir, "checkpoint.db")
	}

	verification, err := verify.Run()
	if err != nil {
		return err
	}
	bundle := verification.Bundle
	printVerification(stdout, verification)
	if !verification.OK() {
		return errors.New("built-in verification failed")
	}

	for version, data := range bundle.Docs {
		filename := map[string]string{
			model.SchemaV1: "project-v1.json",
			model.SchemaV2: "project-v2.json",
			model.SchemaV3: "project-v3.json",
		}[version]
		if err := os.WriteFile(filepath.Join(outDir, filename), data, 0o644); err != nil {
			return err
		}
	}
	truthJSON, _ := json.MarshalIndent(bundle.Truth, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "truth-report.json"), truthJSON, 0o644); err != nil {
		return err
	}

	report := engine.RunFull(bundle.Project)
	printEventTable(stdout, report)
	printMerges(stdout, report)
	printIncremental(stdout, bundle.Project)
	if err := printCheckpointRecovery(ctx, stdout, bundle.Project, bundle.Truth, dbPath); err != nil {
		return err
	}
	printExceptions(ctx, stdout, bundle, filepath.Join(outDir, "exceptions"))

	fmt.Fprintf(stdout, "\n演示产物已写入 %s\n", outDir)
	return nil
}

func printVerification(w io.Writer, report *verify.Report) {
	fmt.Fprintln(w, "== 内置验收 ==")
	for _, check := range report.Checks {
		mark := "PASS"
		if !check.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "[%s] %s %s\n", mark, check.Name, truncate(check.Detail, 160))
	}
}

func printEventTable(w io.Writer, report *model.Report) {
	fmt.Fprintln(w, "\n== 逐事件采样 / 预算 / 归并 ==")
	for _, event := range report.Events {
		parts := []string{fmt.Sprintf("#%02d %-14s", event.Seq, event.Kind)}
		if event.Head != nil {
			parts = append(parts, fmt.Sprintf("head=%t/%s bucket=%d rate=%dbp edge=%s#%d",
				event.Head.Keep, event.Head.DecisionType, event.Head.Bucket, event.Head.RatePermyriad, event.Head.EdgeID, event.Head.EdgeNo))
		}
		if event.Tail != nil {
			parts = append(parts, fmt.Sprintf("tail=%s selected=%s matched=[%s]",
				event.Tail.DecisionType, event.Tail.SelectedRuleID, strings.Join(event.Tail.MatchedRuleIDs, ",")))
		}
		if event.Budget != nil {
			parts = append(parts, fmt.Sprintf("budget accepted=%t cost=%d globalRem=%d used=%d %s",
				event.Budget.Accepted, event.Budget.CostSpans, event.Budget.GlobalRemaining, event.Budget.GlobalUsedSpans, event.Budget.RejectCode))
		}
		if event.Merge != nil {
			parts = append(parts, "merge=["+strings.Join(event.Merge.OrderedSpanIDs, " < ")+"]")
			if len(event.Merge.UnmergedSpans) > 0 {
				parts = append(parts, fmt.Sprintf("orphans=%d", len(event.Merge.UnmergedSpans)))
			}
		}
		fmt.Fprintln(w, strings.Join(parts, " | "))
	}
}

func printMerges(w io.Writer, report *model.Report) {
	fmt.Fprintln(w, "\n== 最终归并与保留集合 ==")
	for traceID, merged := range report.Merge {
		fmt.Fprintf(w, "%s status=%s order=[%s] retained=[%s] duplicates=%d unmerged=%d\n",
			traceID, merged.Status, strings.Join(merged.OrderedSpanIDs, ","),
			strings.Join(report.RetainedSpans[traceID], ","), len(merged.DuplicateSpanIDs), len(merged.UnmergedSpans))
		for _, orphan := range merged.UnmergedSpans {
			fmt.Fprintf(w, "  orphan %s parent=%s code=%s action=%s\n", orphan.SpanID, orphan.ParentSpanID, orphan.Code, orphan.Action)
		}
	}
}

func printIncremental(w io.Writer, project *model.Project) {
	fmt.Fprintln(w, "\n== 增量编辑：e-checkout 3500bp -> 0bp ==")
	cache := engine.NewCache(project)
	result, err := cache.Apply(engine.Edit{Type: "edge_rate", EdgeID: "e-checkout", RatePermyriad: 0})
	if err != nil {
		fmt.Fprintln(w, "incremental failed:", err)
		return
	}
	full := engine.RunFull(result.Project)
	fmt.Fprintf(w, "semantic_equal_full_recompute=%t closure_nodes=%d recomputed=%d reused_cache_nodes=%d\n",
		engine.ReportsSemanticEqual(result.Report, full), len(result.Closure.Nodes), len(result.Closure.Recomputed), len(result.Closure.Reused))
	fmt.Fprintf(w, "event_ranges=%v\n", result.Closure.EventRanges)
	for _, node := range result.Closure.Nodes {
		if node.Kind == "head_decision" || node.Kind == "tail_decision" {
			fmt.Fprintf(w, "  invalid %s <- %s (%s)\n", node.NodeID, node.ImmediateSource, node.Reason)
		}
	}

	oldHeads := map[string]model.HeadDecision{}
	for _, decision := range cache.Base.Head {
		oldHeads[decision.RequestID] = decision
	}
	for _, decision := range result.Report.Head {
		if old := oldHeads[decision.RequestID]; old.Keep != decision.Keep {
			fmt.Fprintf(w, "  diff head %s: %t -> %t\n", decision.RequestID, old.Keep, decision.Keep)
		}
	}
	oldBudget := map[int]model.BudgetDecision{}
	for _, decision := range cache.Base.Budget {
		oldBudget[decision.EventSeq] = decision
	}
	for _, decision := range result.Report.Budget {
		if old, ok := oldBudget[decision.EventSeq]; !ok || old.Accepted != decision.Accepted || old.GlobalUsedSpans != decision.GlobalUsedSpans {
			fmt.Fprintf(w, "  diff budget seq=%d accepted=%t used=%d (old accepted=%t used=%d)\n",
				decision.EventSeq, decision.Accepted, decision.GlobalUsedSpans, old.Accepted, old.GlobalUsedSpans)
		}
	}
}

func printCheckpointRecovery(ctx context.Context, w io.Writer, project *model.Project, truth *model.Report, dbPath string) error {
	fmt.Fprintln(w, "\n== SQLite 检查点恢复对照 ==")
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	_, err = runtime.RunCheckpointed(ctx, project, st, runtime.Options{Workers: 4, BufferCap: 64}, 20)
	if err == nil || errs.CodeOf(err) != runtime.CrashAfterCommitCode {
		_ = st.Close()
		return fmt.Errorf("expected simulated crash code %s, got %v", runtime.CrashAfterCommitCode, err)
	}
	fmt.Fprintf(w, "crash_injection code=%s at_seq=20; trace/service/global writes already committed in ordered transaction\n", errs.CodeOf(err))
	if err := st.Close(); err != nil {
		return err
	}

	st, err = store.Open(dbPath)
	if err != nil {
		return err
	}
	result, err := runtime.RunCheckpointed(ctx, project, st, runtime.Options{Workers: 4, BufferCap: 64}, 0)
	if err != nil {
		_ = st.Close()
		return err
	}
	fmt.Fprintf(w, "recovery action=%s loaded_seq=%d checkpoint_completed_traces=%d final_events=%d truth_match=%t\n",
		result.Recovery.Action, result.Recovery.LoadedSeq, result.Recovery.CompletedInCheckpoint,
		len(result.Report.Events), engine.ReportsSemanticEqual(result.Report, truth))

	mismatchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, mismatchErr := st.Load(mismatchCtx, model.SchemaV9Unsupported)
	fmt.Fprintf(w, "version probe expected=%s code=%s\n", model.SchemaV9Unsupported, errs.CodeOf(mismatchErr))
	if err := st.Close(); err != nil {
		return err
	}
	return nil
}

func printExceptions(ctx context.Context, w io.Writer, bundle *sample.Bundle, stateDir string) {
	fmt.Fprintln(w, "\n== 异常注入业务码 ==")
	results, err := exceptions.All(ctx, bundle.Project, bundle.Docs[model.SchemaV1], stateDir)
	if err != nil {
		fmt.Fprintln(w, "exception setup failed:", err)
		return
	}
	for _, result := range results {
		fmt.Fprintf(w, "%-28s %-32s path=%s\n", result.Name, result.Code, result.Path)
	}
}

func truncate(value string, limit int) string {
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) <= limit {
		return value
	}
	return value[:limit-3] + "..."
}
