package lipsync_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	lipsync "lipsync"
)

func TestQuantizationAndIntegerWeights(t *testing.T) {
	frame, num, den, rem := lipsync.QuantizeSampleToFrame(8100, 16000, 30, lipsync.QuantizeDown)
	if frame != 15 || num != 243000 || den != 16000 || rem != 3000 {
		t.Fatalf("unexpected down quantization: frame=%d num=%d den=%d rem=%d", frame, num, den, rem)
	}
	up, _, _, _ := lipsync.QuantizeSampleToFrame(8100, 16000, 30, lipsync.QuantizeUp)
	nearest, _, _, _ := lipsync.QuantizeSampleToFrame(8100, 16000, 30, lipsync.QuantizeNearest)
	if up != 16 || nearest != 15 {
		t.Fatalf("up=%d nearest=%d, want 16 and 15", up, nearest)
	}
	if got := lipsync.PermilleToPermyriad(123); got != 1230 {
		t.Fatalf("permille conversion=%d", got)
	}
	truncated, remainder := lipsync.PermyriadToPermille(1234)
	if truncated != 123 || remainder != 4 {
		t.Fatalf("myriad reverse conversion=(%d,%d)", truncated, remainder)
	}

	project := lipsync.GenerateProject(lipsync.DefaultGenerateOptions())
	result, err := lipsync.BuildFullResult(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, dr := range result.Dialogues {
		if len(dr.Truncations) != 2*len(dr.Dialogue.Phonemes) {
			t.Fatalf("truncation count=%d for %d phonemes", len(dr.Truncations), len(dr.Dialogue.Phonemes))
		}
		for _, qp := range dr.Quantized {
			if qp.EndFrame < qp.StartFrame {
				t.Fatalf("negative frame span for %s", qp.ID)
			}
		}
	}
}

func TestGeneratedPacksAndDispatchInvariants(t *testing.T) {
	project := lipsync.GenerateProject(lipsync.DefaultGenerateOptions())
	result, err := lipsync.BuildFullResult(project)
	if err != nil {
		t.Fatal(err)
	}

	var allPacks []lipsync.Pack
	tieBreaks := 0
	for _, dr := range result.Dialogues {
		for _, pack := range dr.Packs {
			if len(pack.PhonemeIDs) > project.Recipe.MaxPhonemesPerPack {
				t.Fatalf("pack too large: %d", len(pack.PhonemeIDs))
			}
			if pack.VisibleFrames < project.Recipe.MinVisibleFrames {
				t.Fatalf("pack %s visible=%d min=%d", pack.PackID, pack.VisibleFrames, project.Recipe.MinVisibleFrames)
			}
			classes := map[lipsync.PhonemeClass]bool{}
			for _, detail := range pack.PhonemeDetails {
				for _, qp := range dr.Quantized {
					if qp.ID == detail.PhonemeID {
						classes[qp.Class] = true
					}
				}
			}
			if classes[lipsync.ClassSilence] && len(pack.PhonemeIDs) != 1 {
				t.Fatalf("pack %s crosses silence boundary", pack.PackID)
			}
			for _, boundary := range pack.Decision.Boundaries {
				if boundary.TieBreakApplied {
					tieBreaks++
				}
			}
			allPacks = append(allPacks, pack)
		}
	}
	if tieBreaks == 0 {
		t.Fatal("expected deterministic affinity tie-break records")
	}

	shuffled := append([]lipsync.Pack(nil), allPacks...)
	for i := 0; i < len(shuffled); i += 2 {
		j := (i + 1) % len(shuffled)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	shuffledDispatch := lipsync.DispatchPacks(shuffled, project.Recipe)
	if diff := lipsync.DiffDispatch(result.Dispatch, shuffledDispatch); diff != nil {
		t.Fatalf("dispatch order drift: %v", diff)
	}

	for _, frame := range result.Dispatch.Frames {
		if frame.BudgetUsed > project.Recipe.FramePackBudget {
			t.Fatalf("frame %d budget %d > %d", frame.Frame, frame.BudgetUsed, project.Recipe.FramePackBudget)
		}
		active := 0
		for _, channel := range frame.Channels {
			if len(channel.PackIDs) > 1 {
				t.Fatalf("channel %d has multiple active packs at frame %d", channel.Channel, frame.Frame)
			}
			active += len(channel.PackIDs)
		}
		if active > project.Recipe.SpeakerChannels {
			t.Fatalf("frame %d active %d > channels %d", frame.Frame, active, project.Recipe.SpeakerChannels)
		}
	}
}

func TestDispatchBudgetDeferAndReject(t *testing.T) {
	recipe := lipsync.GenerateProject(lipsync.DefaultGenerateOptions()).Recipe
	recipe.SpeakerChannels = 2
	recipe.FramePackBudget = 1
	recipe.MaxDeferFrames = 1
	packs := []lipsync.Pack{
		{DialogueID: "a", ShardIndex: 0, Index: 0, PackID: "a0", StartFrame: 0, EndFrame: 1},
		{DialogueID: "b", ShardIndex: 1, Index: 0, PackID: "b0", StartFrame: 0, EndFrame: 1},
	}
	result := lipsync.DispatchPacks(packs, recipe)
	if len(result.Deferrals) == 0 || result.Deferrals[0].Reason != "budget_exhausted" {
		t.Fatalf("expected budget deferral, got %+v", result.Deferrals)
	}
	if len(result.Assignments) != 2 || result.Assignments[1].DelayFrames != 1 {
		t.Fatalf("expected delayed second assignment, got %+v", result.Assignments)
	}

	recipe.SpeakerChannels = 1
	recipe.MaxDeferFrames = 2
	longPacks := []lipsync.Pack{
		{DialogueID: "a", ShardIndex: 0, Index: 0, PackID: "a-long", StartFrame: 0, EndFrame: 10},
		{DialogueID: "b", ShardIndex: 1, Index: 0, PackID: "b-long", StartFrame: 0, EndFrame: 10},
		{DialogueID: "c", ShardIndex: 2, Index: 0, PackID: "c-long", StartFrame: 0, EndFrame: 10},
	}
	rejectResult := lipsync.DispatchPacks(longPacks, recipe)
	if len(rejectResult.Rejections) != 2 {
		t.Fatalf("expected two explicit rejections, got %+v", rejectResult.Rejections)
	}
	for _, rejection := range rejectResult.Rejections {
		if rejection.Reason != "channel_busy" || rejection.Detail == "" {
			t.Fatalf("rejection lacks explainable reason: %+v", rejection)
		}
	}
}

func TestSchemaLoadAndExceptionCodes(t *testing.T) {
	if _, err := lipsync.LoadProjectJSON([]byte(`{"id":"x"}`)); err == nil {
		t.Fatal("expected E_SCHEMA for missing fields")
	} else if domain, ok := lipsync.AsError(err); !ok || domain.Code != lipsync.ECSchema || domain.FieldPath == "" {
		t.Fatalf("expected E_SCHEMA with path, got %v", err)
	}

	invalid := lipsync.GenerateProject(lipsync.DefaultGenerateOptions())
	invalid.Recipe.FPS = 0
	err := lipsync.ValidateProject(invalid)
	if domain, ok := lipsync.AsError(err); !ok || domain.Code != lipsync.ECSchema || domain.FieldPath != "recipe.fps" {
		t.Fatalf("expected recipe.fps E_SCHEMA, got %v", err)
	}

	tests := []struct {
		kind lipsync.InjectKind
		code lipsync.ErrorCode
	}{
		{lipsync.InjectOrder, lipsync.ECOrder},
		{lipsync.InjectLabel, lipsync.ECLabel},
		{lipsync.InjectVis, lipsync.ECVis},
	}
	for _, tc := range tests {
		project := lipsync.GenerateProject(lipsync.GenerateOptions{Inject: tc.kind})
		_, err := lipsync.BuildFullResult(project)
		var domain *lipsync.Error
		if !errors.As(err, &domain) || domain.Code != tc.code {
			t.Fatalf("%s: expected %s, got %v", tc.kind, tc.code, err)
		}
		if domain.DialogueID == "" || !domain.HasShard || domain.FieldPath == "" {
			t.Fatalf("%s error lacks location: %+v", tc.kind, domain)
		}
	}

	fail := 3
	poolRun, err := lipsync.RunShardPool(context.Background(),
		lipsync.PoolOptions{MaxParallelism: 2, PerWorkerBuffer: 4, QueueBudget: 2},
		lipsync.ShardWorksForProject(lipsync.GenerateProject(lipsync.DefaultGenerateOptions()), &fail))
	if err != nil {
		t.Fatal(err)
	}
	stageErr := lipsync.FirstOutcomeError(poolRun.Outcomes)
	var stageDomain *lipsync.Error
	if !errors.As(stageErr, &stageDomain) || stageDomain.Code != lipsync.ECStage ||
		stageDomain.DialogueID != "dlg-003" || stageDomain.ShardIndex != 3 {
		t.Fatalf("expected located E_STAGE, got %v", stageErr)
	}
}

func TestIncrementalPrecisionAndFullDiff(t *testing.T) {
	store, err := lipsync.OpenStore(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := lipsync.NewEngine(lipsync.GenerateProject(lipsync.DefaultGenerateOptions()), store)
	if err != nil {
		t.Fatal(err)
	}

	weightEdit := lipsync.Edit{DialogueID: "dlg-000", Field: "weight", PhonemeIndex: 1, IntValue: 777}
	result, manifest, err := engine.ApplyEdit(weightEdit)
	if err != nil {
		t.Fatal(err)
	}
	full, err := lipsync.BuildFullResult(engine.Project)
	if err != nil {
		t.Fatal(err)
	}
	if diff := lipsync.DiffResults(full, result); diff != nil {
		t.Fatal(diff)
	}
	if len(manifest.Packs) != 1 || manifest.Packs[0].DialogueID != "dlg-000" {
		t.Fatalf("expected exactly one invalid pack, got %+v", manifest.Packs)
	}
	if len(manifest.DownstreamFrames) == 0 {
		t.Fatal("expected downstream frames from dependency graph")
	}

	budgetEdit := lipsync.Edit{Field: "recipe.framePackBudget", IntValue: 4}
	budgetResult, budgetManifest, err := engine.ApplyEdit(budgetEdit)
	if err != nil {
		t.Fatal(err)
	}
	budgetFull, err := lipsync.BuildFullResult(engine.Project)
	if err != nil {
		t.Fatal(err)
	}
	if diff := lipsync.DiffResults(budgetFull, budgetResult); diff != nil {
		t.Fatal(diff)
	}
	if len(budgetManifest.Packs) != 0 {
		t.Fatalf("dispatch-only budget edit should reuse all packs, invalidated=%+v", budgetManifest.Packs)
	}
	if len(budgetManifest.DispatchFrames) == 0 {
		t.Fatal("expected actual dispatch frame changes")
	}

	entries, err := store.LoadCacheEntries(engine.Project.ID)
	if err != nil || len(entries) == 0 {
		t.Fatalf("persistent cache entries missing: n=%d err=%v", len(entries), err)
	}
}

func TestAcceptanceThreePathsAndRecovery(t *testing.T) {
	workDir := t.TempDir()
	report, err := lipsync.RunAcceptance(context.Background(), workDir)
	data, _ := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("acceptance failed: %v\n%s", err, string(data))
	}
	if !report.OK || !report.ResultsEqual || !report.CommitOrdersEqual {
		t.Fatalf("acceptance report not OK:\n%s", string(data))
	}
	if report.RecoveryInfo == nil || !report.RecoveryInfo.ForcedTermination {
		t.Fatalf("expected force-kill recovery, got %+v", report.RecoveryInfo)
	}
	for _, check := range report.EditChecks {
		if !check.Equal {
			t.Fatalf("edit mismatch: %+v", check.Diff)
		}
	}
	if _, err := os.Stat(filepath.Join(workDir, "data", "acceptance-recovery.db")); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointVersionMismatchInvalidatesShard(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "version.db")
	project := lipsync.GenerateProject(lipsync.DefaultGenerateOptions())
	svc, err := lipsync.NewService(project, lipsync.ServiceOptions{DBPath: dbPath, RunID: "version-run"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.ProcessAndCommitAll(ctx, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	changed := lipsync.GenerateProject(lipsync.DefaultGenerateOptions())
	d := lipsync.FindDialogue(changed, "dlg-002")
	d.Phonemes[1].Weight += 11
	lipsync.EnsureDialogueHashes(changed)
	store, err := lipsync.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	info, _, err := lipsync.InspectAndInvalidateCheckpoints(store, changed)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.InvalidatedShards) != 1 || info.InvalidatedShards[0].ShardIndex != 2 {
		t.Fatalf("expected only shard 2 invalidated, got %+v", info.InvalidatedShards)
	}
}

func TestUserCancellationDistinguishedFromForceKill(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "runs.db")
	project := lipsync.GenerateProject(lipsync.DefaultGenerateOptions())
	svc, err := lipsync.NewService(project, lipsync.ServiceOptions{DBPath: dbPath, RunID: "cancel-run"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkCancelled(map[string]string{"reason": "user_pressed_cancel"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := lipsync.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	info, _, err := lipsync.InspectAndInvalidateCheckpoints(store, project)
	if err != nil {
		t.Fatal(err)
	}
	if !info.UserCancelled || info.ForcedTermination {
		t.Fatalf("expected user cancellation, got %+v", info)
	}

	if err := store.StartRun("killed-run", project.ID); err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestRun(project.ID)
	if err != nil || latest.Status != "running" {
		t.Fatalf("latest run=%+v err=%v", latest, err)
	}
}

func TestPoolParallelismQueueBudgetAndBuffer(t *testing.T) {
	var active int32
	var maxActive int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	opts := lipsync.PoolOptions{MaxParallelism: 2, PerWorkerBuffer: 2, QueueBudget: 1}
	works := make([]lipsync.ShardWork, 0, 3)
	for i := 0; i < 3; i++ {
		works = append(works, lipsync.ShardWork{
			ShardIndex: i, DialogueID: "parallel",
			Task: func(ctx context.Context, buffer *lipsync.StateBuffer) (any, error) {
				cur := atomic.AddInt32(&active, 1)
				for {
					old := atomic.LoadInt32(&maxActive)
					if cur <= old || atomic.CompareAndSwapInt32(&maxActive, old, cur) {
						break
					}
				}
				entered <- struct{}{}
				<-release
				atomic.AddInt32(&active, -1)
				return nil, buffer.Push("ok")
			},
		})
	}
	done := make(chan *lipsync.PoolRun, 1)
	go func() {
		run, _ := lipsync.RunShardPool(context.Background(), opts, works)
		done <- run
	}()
	<-entered
	<-entered
	close(release)
	run := <-done
	if maxActive != 2 {
		t.Fatalf("max parallelism=%d, want 2", maxActive)
	}
	if len(run.Outcomes) != 3 {
		t.Fatalf("outcomes=%d", len(run.Outcomes))
	}

	queueEntered := make(chan struct{})
	queueRelease := make(chan struct{})
	queueWorks := []lipsync.ShardWork{{
		ShardIndex: 0, DialogueID: "queue-block",
		Task: func(ctx context.Context, buffer *lipsync.StateBuffer) (any, error) {
			close(queueEntered)
			<-queueRelease
			return nil, nil
		},
	}}
	for i := 1; i < 4; i++ {
		queueWorks = append(queueWorks, lipsync.ShardWork{
			ShardIndex: i, DialogueID: "queue",
			Task: func(ctx context.Context, buffer *lipsync.StateBuffer) (any, error) { return nil, nil },
		})
	}
	queueDone := make(chan *lipsync.PoolRun, 1)
	go func() {
		run, _ := lipsync.RunShardPool(context.Background(),
			lipsync.PoolOptions{MaxParallelism: 1, PerWorkerBuffer: 2, QueueBudget: 1}, queueWorks)
		queueDone <- run
	}()
	<-queueEntered
	time.Sleep(50 * time.Millisecond)
	close(queueRelease)
	queueRun := <-queueDone
	foundQueueEvent := false
	foundMeasuredWait := false
	for _, event := range queueRun.Events {
		if event.Reason == "queue_budget_exhausted" && event.QueueBudget == 1 {
			foundQueueEvent = true
			if event.WaitedMillis < 10 {
				continue
			}
			foundMeasuredWait = true
		}
	}
	if !foundQueueEvent {
		t.Fatalf("expected queue budget event, got %+v", queueRun.Events)
	}
	if !foundMeasuredWait {
		t.Fatalf("expected at least one measured blocking wait, got %+v", queueRun.Events)
	}

	buffer := &lipsync.StateBuffer{WorkerID: 7, Capacity: 1}
	if err := buffer.Push("first"); err != nil {
		t.Fatal(err)
	}
	err := buffer.Push("second")
	var domain *lipsync.Error
	if !errors.As(err, &domain) || domain.Code != lipsync.ECStage {
		t.Fatalf("expected E_STAGE buffer limit, got %v", err)
	}
}
