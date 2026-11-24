package workbench

import (
	"context"
	"os"
	"path/filepath"

	"performanceexpressions/workspace/checkpoint"
	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
	"performanceexpressions/workspace/session"
)

type Trace struct {
	Name       string          `json:"name"`
	Operations []domain.EditOp `json:"operations"`
}

type PathReport struct {
	Name        string           `json:"name"`
	ProjectHash string           `json:"project_hash"`
	ResultHash  string           `json:"result_hash"`
	Commits     int              `json:"commits"`
	Samples     int              `json:"samples"`
	Decisions   int              `json:"decisions"`
	Recovery    *RecoveryReport  `json:"recovery,omitempty"`
	Result      domain.ResultSet `json:"-"`
}

type PathComparison struct {
	Left        string       `json:"left"`
	Right       string       `json:"right"`
	Equal       bool         `json:"equal"`
	First       *engine.Diff `json:"first_difference,omitempty"`
	Differences int          `json:"differences"`
}

type ThreePathReport struct {
	Trace       string           `json:"trace"`
	Paths       []PathReport     `json:"paths"`
	Comparisons []PathComparison `json:"comparisons"`
	Equal       bool             `json:"equal"`
}

func RunThreePaths(ctx context.Context, base domain.Project, params domain.Parameters, trace Trace) (ThreePathReport, error) {
	report := ThreePathReport{Trace: trace.Name}
	if len(trace.Operations) == 0 {
		return report, domain.NewError(domain.CodeSchema, "trace contains no operations")
	}
	for _, op := range trace.Operations {
		if op.BatchID == "" {
			return report, domain.NewError(domain.CodeSchema, "consistency trace operations must declare a batch id").WithSegment(op.SegmentID)
		}
	}

	realtimeSession, err := session.New(base, params)
	if err != nil {
		return report, err
	}
	realtimeCommits, err := ApplyTraceToSession(ctx, realtimeSession, trace.Operations)
	if err != nil {
		return report, err
	}
	realtimeProject := realtimeSession.CommittedProject()
	realtimeResult := realtimeSession.CommittedResult()
	realtimeReport, err := makePathReport("realtime", realtimeProject, realtimeResult, realtimeCommits, nil)
	if err != nil {
		return report, err
	}

	fullProject, fullResult, err := session.ApplyProjectOps(base, trace.Operations, params)
	if err != nil {
		return report, err
	}
	fullReport, err := makePathReport("full-recompute", fullProject, fullResult, len(uniqueBatches(trace.Operations)), nil)
	if err != nil {
		return report, err
	}

	tempDir, err := os.MkdirTemp("", "expression-consistency-*")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(tempDir)
	dbPath := filepath.Join(tempDir, "checkpoints.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return report, err
	}
	if err := store.EnsureProject(ctx, base); err != nil {
		_ = store.Close()
		return report, err
	}
	commitSession, err := session.New(base, params, session.Options{Sink: store})
	if err != nil {
		_ = store.Close()
		return report, err
	}
	commitCount, err := ApplyTraceToSession(ctx, commitSession, trace.Operations)
	if err != nil {
		_ = store.Close()
		return report, err
	}
	if err := store.Close(); err != nil {
		return report, err
	}
	reopened, err := checkpoint.Open(dbPath)
	if err != nil {
		return report, err
	}
	defer reopened.Close()
	resumeSession, recovery, err := Recover(ctx, reopened, base, params)
	if err != nil {
		return report, err
	}
	resumeProject := resumeSession.CommittedProject()
	resumeResult := resumeSession.CommittedResult()
	resumeReport, err := makePathReport("checkpoint-resume", resumeProject, resumeResult, commitCount, &recovery)
	if err != nil {
		return report, err
	}

	report.Paths = []PathReport{realtimeReport, fullReport, resumeReport}
	pairs := [][2]int{{0, 1}, {0, 2}, {1, 2}}
	for _, pair := range pairs {
		left, right := report.Paths[pair[0]], report.Paths[pair[1]]
		comparison, cmpErr := engine.CompareResultSets(left.Result, right.Result)
		pc := PathComparison{
			Left:        left.Name,
			Right:       right.Name,
			Equal:       comparison.Equal,
			First:       comparison.First,
			Differences: comparison.Differences,
		}
		if left.ProjectHash != right.ProjectHash {
			pc.Equal = false
			pc.Differences++
			if pc.First == nil {
				pc.First = &engine.Diff{Field: "project_hash", Left: left.ProjectHash, Right: right.ProjectHash, Message: "committed project hashes differ"}
			}
		}
		report.Comparisons = append(report.Comparisons, pc)
		if !pc.Equal {
			if cmpErr == nil {
				cmpErr = domain.NewError(domain.CodeDiff, "three-path comparison failed: %s vs %s", left.Name, right.Name)
			}
			report.Equal = false
			return report, cmpErr
		}
	}
	report.Equal = true
	return report, nil
}

func ApplyTraceToSession(ctx context.Context, sess *session.Session, ops []domain.EditOp) (int, error) {
	commits := 0
	for i := 0; i < len(ops); {
		batchID := ops[i].BatchID
		if batchID == "" {
			return 0, domain.NewError(domain.CodeSchema, "trace operation has no batch id").WithSegment(ops[i].SegmentID)
		}
		var segmentIDs []string
		j := i
		for ; j < len(ops) && ops[j].BatchID == batchID; j++ {
			if err := sess.ApplyEdit(ops[j]); err != nil {
				return 0, err
			}
			segmentIDs = appendUnique(segmentIDs, ops[j].SegmentID)
		}
		if _, err := sess.CommitBatch(ctx, batchID, segmentIDs); err != nil {
			return 0, err
		}
		commits++
		i = j
	}
	return commits, nil
}

func makePathReport(name string, project domain.Project, result domain.ResultSet, commits int, recovery *RecoveryReport) (PathReport, error) {
	projectHash, err := domain.HashJSON(project)
	if err != nil {
		return PathReport{}, err
	}
	resultHash, err := domain.HashJSON(result)
	if err != nil {
		return PathReport{}, err
	}
	samples, decisions := 0, 0
	for _, seg := range result.Segments {
		samples += len(seg.Samples)
		for _, sample := range seg.Samples {
			decisions += len(sample.Decisions)
		}
	}
	return PathReport{
		Name:        name,
		ProjectHash: projectHash,
		ResultHash:  resultHash,
		Commits:     commits,
		Samples:     samples,
		Decisions:   decisions,
		Recovery:    recovery,
		Result:      result,
	}, nil
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func uniqueBatches(ops []domain.EditOp) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, op := range ops {
		if !seen[op.BatchID] {
			seen[op.BatchID] = true
			ids = append(ids, op.BatchID)
		}
	}
	return ids
}
