package experiment

import (
	"context"
	"path/filepath"

	"screenverify/internal/dataset"
	"screenverify/internal/rverr"
	"screenverify/internal/store"
	"screenverify/internal/verify"
)

type PathResult struct {
	Name     string               `json:"name"`
	DBPath   string               `json:"db_path"`
	Report   *verify.Report       `json:"report"`
	Recovery *verify.RecoveryInfo `json:"recovery,omitempty"`
}

type ThreeWayReport struct {
	Clip        string             `json:"clip"`
	PlanHash    string             `json:"plan_hash"`
	CancelAfter int                `json:"cancel_after_checkpoints"`
	Realtime    PathResult         `json:"realtime"`
	Resume      PathResult         `json:"resume"`
	Full        PathResult         `json:"full"`
	Consistent  bool               `json:"consistent"`
	Diff        *verify.Difference `json:"diff,omitempty"`
	CommitDiff  *verify.Difference `json:"commit_diff,omitempty"`
}

func RunThreeWay(ctx context.Context, clipDir, workDB string, p verify.Params, rt verify.RuntimeConfig, cancelAfter int) (*ThreeWayReport, error) {
	planHash := verify.ParamVersion(p) + "-b" + itoa(p.BatchFrames) + "-p" + itoa(rt.Parallelism)
	out := &ThreeWayReport{PlanHash: planHash, CancelAfter: cancelAfter, Consistent: true}

	{
		set, err := dataset.Load(clipDir)
		if err != nil {
			return nil, err
		}
		out.Clip = set.Truth.Clip
		dbPath := filepath.Join(workDB, "realtime.db")
		st, err := store.Open(dbPath)
		if err != nil {
			return nil, err
		}
		e, err := verify.NewEngine(set, p, rt, st)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		if _, err := e.EnsureIdentity(ctx, planHash); err != nil {
			_ = st.Close()
			return nil, err
		}
		if err := runAll(ctx, e); err != nil {
			_ = st.Close()
			return nil, err
		}
		out.Realtime = PathResult{Name: "realtime", DBPath: dbPath, Report: e.BuildReport(nil)}
		_ = st.Close()
	}

	{
		dbPath := filepath.Join(workDB, "resume.db")
		prefixSet, err := dataset.Load(clipDir)
		if err != nil {
			return nil, err
		}
		st, err := store.Open(dbPath)
		if err != nil {
			return nil, err
		}
		e, err := verify.NewEngine(prefixSet, p, rt, st)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		info, err := e.EnsureIdentity(ctx, planHash)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		if cancelAfter < 1 {
			cancelAfter = e.BatchCount() / 2
		}
		if cancelAfter > e.BatchCount() {
			cancelAfter = e.BatchCount()
		}
		for b := 0; b < cancelAfter; b++ {
			if _, err := e.RunBatch(ctx, b); err != nil {
				_ = st.Close()
				return nil, err
			}
		}
		_ = info
		_ = st.Close()

		set, err := dataset.Load(clipDir)
		if err != nil {
			return nil, err
		}
		st2, err := store.Open(dbPath)
		if err != nil {
			return nil, err
		}
		e2, err := verify.NewEngine(set, p, rt, st2)
		if err != nil {
			_ = st2.Close()
			return nil, err
		}
		recovery, err := e2.Recover(ctx, planHash)
		if err != nil {
			_ = st2.Close()
			return nil, err
		}
		start := recovery.CompletedCheckpoints
		for b := start; b < e2.BatchCount(); b++ {
			if _, err := e2.RunBatch(ctx, b); err != nil {
				_ = st2.Close()
				return nil, err
			}
		}
		out.Resume = PathResult{Name: "resume", DBPath: dbPath, Report: e2.BuildReport(recovery), Recovery: recovery}
		_ = st2.Close()
	}

	{
		set, err := dataset.Load(clipDir)
		if err != nil {
			return nil, err
		}
		dbPath := filepath.Join(workDB, "full.db")
		st, err := store.Open(dbPath)
		if err != nil {
			return nil, err
		}
		e, err := verify.NewEngine(set, p, rt, st)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		if _, err := e.EnsureIdentity(ctx, planHash); err != nil {
			_ = st.Close()
			return nil, err
		}
		if err := runAll(ctx, e); err != nil {
			_ = st.Close()
			return nil, err
		}
		out.Full = PathResult{Name: "full", DBPath: dbPath, Report: e.BuildReport(nil)}
		_ = st.Close()
	}

	a := out.Realtime.Report.Snapshot()
	b := out.Resume.Report.Snapshot()
	c := out.Full.Report.Snapshot()
	if d := verify.CompareSnapshots(a, b); d != nil {
		out.Consistent = false
		out.Diff = d
	} else if d := verify.CompareSnapshots(a, c); d != nil {
		out.Consistent = false
		out.Diff = d
	}
	if d := verify.CompareCommitSequences(out.Realtime.Report.CommitSequence, out.Resume.Report.CommitSequence); d != nil {
		out.Consistent = false
		out.CommitDiff = d
	} else if d := verify.CompareCommitSequences(out.Realtime.Report.CommitSequence, out.Full.Report.CommitSequence); d != nil {
		out.Consistent = false
		out.CommitDiff = d
	}
	if !out.Consistent {
		if out.Diff != nil {
			return out, rverr.New(rverr.EDiff, out.Diff.Message)
		}
		return out, rverr.New(rverr.EDiff, out.CommitDiff.Message)
	}
	return out, nil
}

func runAll(ctx context.Context, e *verify.Engine) error {
	for b := 0; b < e.BatchCount(); b++ {
		if _, err := e.RunBatch(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
