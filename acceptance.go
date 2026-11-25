package lipsync

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
)

type PathSummary struct {
	Name             string  `json:"name"`
	DBPath           string  `json:"dbPath"`
	CommitOrder      []int   `json:"commitOrder"`
	CommitSeqOrder   []int64 `json:"commitSeqOrder"`
	FrameCount       int     `json:"frameCount"`
	PackCount        int     `json:"packCount"`
	DeferralCount    int     `json:"deferralCount"`
	RejectionCount   int     `json:"rejectionCount"`
	StreamFrameCount int     `json:"streamFrameCount"`
}

type EditCheck struct {
	Revision         int                 `json:"revision"`
	Edit             Edit                `json:"edit"`
	Equal            bool                `json:"equal"`
	Diff             *Error              `json:"diff,omitempty"`
	InvalidPacks     []PackInvalidation  `json:"invalidPacks"`
	DownstreamFrames []FrameInvalidation `json:"downstreamFrames"`
	ReusedPacks      []string            `json:"reusedPacks"`
	DispatchChanges  int                 `json:"dispatchChanges"`
}

type ExceptionCheck struct {
	Kind       InjectKind `json:"kind"`
	Expected   ErrorCode  `json:"expected"`
	Actual     ErrorCode  `json:"actual,omitempty"`
	DialogueID string     `json:"dialogueId,omitempty"`
	ShardIndex int        `json:"shardIndex,omitempty"`
	HasShard   bool       `json:"hasShard"`
	FieldPath  string     `json:"fieldPath,omitempty"`
	OK         bool       `json:"ok"`
}

type AcceptanceReport struct {
	OK                bool             `json:"ok"`
	WorkDir           string           `json:"workDir"`
	DialogueCount     int              `json:"dialogueCount"`
	PhonemeCount      int              `json:"phonemeCount"`
	PackCount         int              `json:"packCount"`
	FrameCount        int              `json:"frameCount"`
	Realtime          PathSummary      `json:"realtime"`
	Recovery          PathSummary      `json:"recovery"`
	Full              PathSummary      `json:"full"`
	RecoveryInfo      *RecoveryInfo    `json:"recoveryInfo,omitempty"`
	ResultsEqual      bool             `json:"resultsEqual"`
	CommitOrdersEqual bool             `json:"commitOrdersEqual"`
	FinalCommitOrder  []int            `json:"finalCommitOrder"`
	EditChecks        []EditCheck      `json:"editChecks"`
	Exceptions        []ExceptionCheck `json:"exceptions"`
}

func RunAcceptance(ctx context.Context, workDir string) (*AcceptanceReport, error) {
	dataDir := filepath.Join(workDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	report := &AcceptanceReport{WorkDir: workDir, EditChecks: []EditCheck{}, Exceptions: []ExceptionCheck{}}
	base := GenerateProject(DefaultGenerateOptions())
	EnsureDialogueHashes(base)

	full, err := BuildFullResult(CloneProject(base))
	if err != nil {
		return report, err
	}
	report.DialogueCount = len(full.Dialogues)
	for _, dr := range full.Dialogues {
		report.PhonemeCount += len(dr.Quantized)
		report.PackCount += len(dr.Packs)
	}
	if full.Dispatch != nil {
		report.FrameCount = full.Dispatch.FrameCount
	}

	poolOpts := PoolOptions{MaxParallelism: 3, PerWorkerBuffer: 32, QueueBudget: 6}

	realtimeDB := filepath.Join(dataDir, "acceptance-realtime.db")
	resetDBFiles(realtimeDB)
	rtSvc, err := NewService(GenerateProject(DefaultGenerateOptions()), ServiceOptions{
		DBPath: realtimeDB, RunID: "acceptance-realtime", Pool: poolOpts,
	})
	if err != nil {
		return report, err
	}
	defer rtSvc.Close()
	rtResult, rtCommits, _, err := rtSvc.ProcessAndCommitAll(ctx, 0, nil)
	if err != nil {
		return report, err
	}
	rtStream := DispatchPacksStream(allPacks(rtResult), rtResult.Recipe, nil)
	report.Realtime = summarizePath("realtime", realtimeDB, rtCommits, rtResult, len(rtStream.Frames))

	fullDB := filepath.Join(dataDir, "acceptance-full.db")
	resetDBFiles(fullDB)
	fullSvc, err := NewService(GenerateProject(DefaultGenerateOptions()), ServiceOptions{
		DBPath: fullDB, RunID: "acceptance-full", Pool: poolOpts,
	})
	if err != nil {
		return report, err
	}
	defer fullSvc.Close()
	fullPathResult, fullCommits, _, err := fullSvc.ProcessAndCommitAll(ctx, 0, nil)
	if err != nil {
		return report, err
	}
	report.Full = summarizePath("full", fullDB, fullCommits, fullPathResult, len(fullPathResult.Dispatch.Frames))

	recoveryDB := filepath.Join(dataDir, "acceptance-recovery.db")
	resetDBFiles(recoveryDB)
	recSvc, err := NewService(GenerateProject(DefaultGenerateOptions()), ServiceOptions{
		DBPath: recoveryDB, RunID: "acceptance-crashed-run", Pool: poolOpts,
	})
	if err != nil {
		return report, err
	}
	if _, _, _, err := recSvc.ProcessAllAndCommitPrefix(ctx, 0, 2); err != nil {
		_ = recSvc.Close()
		return report, err
	}
	if err := recSvc.SimulateForceKill(); err != nil {
		return report, err
	}

	recSvc2, recoveryInfo, recResult, recRecords, _, err := RecoverAndComplete(
		ctx, GenerateProject(DefaultGenerateOptions()),
		ServiceOptions{DBPath: recoveryDB, RunID: "acceptance-recovered-run", Pool: poolOpts},
	)
	if err != nil {
		return report, err
	}
	defer recSvc2.Close()
	report.RecoveryInfo = recoveryInfo
	report.Recovery = summarizePath("recovery", recoveryDB, recRecords, recResult, len(recResult.Dispatch.Frames))

	if diff := DiffResults(fullPathResult, rtResult); diff != nil {
		report.ResultsEqual = false
		return report, diff
	}
	if diff := DiffResults(fullPathResult, recResult); diff != nil {
		report.ResultsEqual = false
		return report, diff
	}
	report.ResultsEqual = true
	report.CommitOrdersEqual = reflect.DeepEqual(report.Realtime.CommitOrder, report.Full.CommitOrder) &&
		reflect.DeepEqual(report.Recovery.CommitOrder, report.Full.CommitOrder)
	if !report.CommitOrdersEqual {
		return report, NewError(ECDiff, "", 0, false, "commitSequence",
			"commit orders realtime=%v recovery=%v full=%v",
			report.Realtime.CommitOrder, report.Recovery.CommitOrder, report.Full.CommitOrder)
	}

	edits := acceptanceEdits()
	for _, edit := range edits {
		rtNew, manifest, rtDiff, err := rtSvc.ApplyEdit(edit)
		if err != nil {
			return report, err
		}
		check := EditCheck{
			Revision: manifest.Revision, Edit: edit, Equal: rtDiff == nil, Diff: rtDiff,
			InvalidPacks: manifest.Packs, DownstreamFrames: manifest.DownstreamFrames,
			ReusedPacks:     manifest.ReusedPacks,
			DispatchChanges: len(manifest.DispatchFrames),
		}
		report.EditChecks = append(report.EditChecks, check)
		if rtDiff != nil {
			return report, rtDiff
		}

		recNew, recManifest, recDiff, err := recSvc2.ApplyEdit(edit)
		if err != nil {
			return report, err
		}
		if recDiff != nil {
			return report, recDiff
		}
		if diff := DiffResults(rtNew, recNew); diff != nil {
			return report, diff
		}
		_ = recManifest
	}

	rtEdited, rtEditedCommits, _, err := rtSvc.ProcessAndCommitAll(ctx, 1, nil)
	if err != nil {
		return report, err
	}
	recEdited, recEditedCommits, _, err := recSvc2.ProcessAndCommitAll(ctx, 1, nil)
	if err != nil {
		return report, err
	}
	fullSvc.Project = CloneProject(rtSvc.Project)
	fullEdited, fullEditedCommits, _, err := fullSvc.ProcessAndCommitAll(ctx, 1, nil)
	if err != nil {
		return report, err
	}
	if diff := DiffResults(fullEdited, rtEdited); diff != nil {
		return report, diff
	}
	if diff := DiffResults(fullEdited, recEdited); diff != nil {
		return report, diff
	}
	rtOrder := CommitOrder(rtEditedCommits)
	recOrder := CommitOrder(recEditedCommits)
	fullOrder := CommitOrder(fullEditedCommits)
	if !reflect.DeepEqual(rtOrder, fullOrder) || !reflect.DeepEqual(recOrder, fullOrder) {
		return report, NewError(ECDiff, "", 0, false, "editedCommitSequence",
			"edited commit orders realtime=%v recovery=%v full=%v", rtOrder, recOrder, fullOrder)
	}
	report.FinalCommitOrder = fullOrder

	report.Exceptions = []ExceptionCheck{
		checkGeneratedException(InjectOrder, ECOrder),
		checkGeneratedException(InjectLabel, ECLabel),
		checkGeneratedException(InjectVis, ECVis),
		checkStageException(ctx),
	}
	for _, check := range report.Exceptions {
		if !check.OK {
			report.OK = false
			return report, NewError(check.Actual, check.DialogueID, check.ShardIndex, check.HasShard,
				check.FieldPath, "exception verification failed for %s", check.Kind)
		}
	}
	report.OK = report.ResultsEqual && report.CommitOrdersEqual
	for _, check := range report.EditChecks {
		report.OK = report.OK && check.Equal
	}
	return report, nil
}

func acceptanceEdits() []Edit {
	probe := GenerateProject(DefaultGenerateOptions())
	d := findDialogue(probe, "dlg-001")
	boundary := 1
	mid := (d.Phonemes[boundary].StartSample + d.Phonemes[boundary+1].EndSample) / 2
	return []Edit{
		{DialogueID: "dlg-000", Field: "weight", PhonemeIndex: 1, IntValue: 420, Description: "change phoneme weight"},
		{DialogueID: "dlg-001", Field: "sharedBoundary", BoundaryIndex: boundary, IntValue: mid, Description: "move shared boundary"},
		{DialogueID: "dlg-002", Field: "affinityToNext", PhonemeIndex: 0, IntValue: 9999, Description: "change transition affinity"},
		{Field: "recipe.framePackBudget", IntValue: 4, Description: "adjust per-frame pack budget"},
	}
}

func checkGeneratedException(kind InjectKind, expected ErrorCode) ExceptionCheck {
	project := GenerateProject(GenerateOptions{Inject: kind})
	_, err := BuildFullResult(project)
	check := ExceptionCheck{Kind: kind, Expected: expected}
	domainErr, ok := AsError(err)
	if ok {
		check.Actual = domainErr.Code
		check.DialogueID = domainErr.DialogueID
		check.ShardIndex = domainErr.ShardIndex
		check.HasShard = domainErr.HasShard
		check.FieldPath = domainErr.FieldPath
	}
	check.OK = ok && domainErr.Code == expected && check.DialogueID != "" && check.HasShard && check.FieldPath != ""
	return check
}

func checkStageException(ctx context.Context) ExceptionCheck {
	project := GenerateProject(DefaultGenerateOptions())
	failShard := 3
	poolRun, err := RunShardPool(ctx, PoolOptions{MaxParallelism: 2, PerWorkerBuffer: 8, QueueBudget: 4},
		ShardWorksForProject(project, &failShard))
	check := ExceptionCheck{Kind: InjectStage, Expected: ECStage}
	if err != nil {
		if domainErr, ok := AsError(err); ok {
			check.Actual = domainErr.Code
			check.DialogueID = domainErr.DialogueID
			check.ShardIndex = domainErr.ShardIndex
			check.HasShard = domainErr.HasShard
			check.FieldPath = domainErr.FieldPath
		}
	}
	if stageErr := FirstOutcomeError(poolRun.Outcomes); stageErr != nil {
		if domainErr, ok := AsError(stageErr); ok {
			check.Actual = domainErr.Code
			check.DialogueID = domainErr.DialogueID
			check.ShardIndex = domainErr.ShardIndex
			check.HasShard = domainErr.HasShard
			check.FieldPath = domainErr.FieldPath
		}
	}
	check.OK = check.Actual == ECStage && check.DialogueID == "dlg-003" && check.HasShard && check.ShardIndex == 3
	return check
}

func summarizePath(name, dbPath string, records []ShardCommitRecord, result *FullResult, streamFrames int) PathSummary {
	summary := PathSummary{
		Name: name, DBPath: dbPath, CommitOrder: CommitOrder(records),
		CommitSeqOrder: []int64{}, StreamFrameCount: streamFrames,
	}
	for _, record := range records {
		summary.CommitSeqOrder = append(summary.CommitSeqOrder, record.CommitSeq)
	}
	if result != nil {
		if result.Dispatch != nil {
			summary.FrameCount = result.Dispatch.FrameCount
			summary.DeferralCount = len(result.Dispatch.Deferrals)
			summary.RejectionCount = len(result.Dispatch.Rejections)
		}
		for _, dr := range result.Dialogues {
			summary.PackCount += len(dr.Packs)
		}
	}
	return summary
}

func allPacks(result *FullResult) []Pack {
	packs := []Pack{}
	if result == nil {
		return packs
	}
	for _, dr := range result.Dialogues {
		packs = append(packs, dr.Packs...)
	}
	return packs
}

func resetDBFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
