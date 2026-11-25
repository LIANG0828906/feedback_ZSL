// Package audit is the acceptance harness: it generates deterministic local
// samples and verifies normalization, truth, three-path settlement parity,
// precise invalidation, checkpoint resume and conflict/error codes.
package audit

import (
	"fmt"
	"os"
	"path/filepath"

	"replayaudit/internal/engine"
	"replayaudit/internal/formats"
	"replayaudit/internal/gen"
	"replayaudit/internal/model"
	"replayaudit/internal/store"
)

// CheckResult is one acceptance check row.
type CheckResult struct {
	Name     string           `json:"name"`
	OK       bool             `json:"ok"`
	Detail   string           `json:"detail,omitempty"`
	Code     string           `json:"code,omitempty"`
	Mismatch *engine.Mismatch `json:"mismatch,omitempty"`
}

// ReplaySummary summarizes per-replay verification.
type ReplaySummary struct {
	ReplayID    string `json:"replay_id"`
	Events      int    `json:"events"`
	Segments    int    `json:"segments"`
	Winner      string `json:"winner"`
	TruthOK     bool   `json:"truth_ok"`
	ThreePathOK bool   `json:"three_path_ok"`
	PoolPeak    int    `json:"pool_peak_parallel,omitempty"`
	PoolBlocks  int    `json:"pool_block_records,omitempty"`
}

// VerificationReport is the machine-readable acceptance result.
type VerificationReport struct {
	SamplesDir string          `json:"samples_dir"`
	Checks     []CheckResult   `json:"checks"`
	Replays    []ReplaySummary `json:"replays"`
	AllOK      bool            `json:"all_ok"`
}

// RunAll generates samples under samplesDir and runs every acceptance check;
// dbDir holds SQLite checkpoint files.
func RunAll(samplesDir, dbDir string) (*VerificationReport, error) {
	rep := &VerificationReport{SamplesDir: samplesDir, AllOK: true}

	_ = os.RemoveAll(dbDir)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	if _, err := gen.GenerateTree(samplesDir); err != nil {
		return nil, fmt.Errorf("generate samples: %w", err)
	}

	built := gen.BuildAll()
	for _, b := range built {
		name := b.Replay.ReplayID
		dir := filepath.Join(samplesDir, name)
		pkg, err := formats.LoadPackage(dir)
		if err != nil {
			rep.add(CheckResult{Name: "load_package:" + name, OK: false, Detail: err.Error()})
			continue
		}
		p := pkg.Params

		// 1) truth through the multi-format files
		full, err := engine.SettleFull(pkg.Replay, p)
		if err != nil {
			rep.add(CheckResult{Name: "settle:" + name, OK: false, Detail: err.Error()})
			continue
		}
		truthOK := true
		var mm *engine.Mismatch
		if pkg.Truth != nil {
			mm = engine.CompareTruth(pkg.Truth, full, p.Tolerance)
			truthOK = mm == nil
			if !truthOK {
				rep.add(CheckResult{Name: "truth:" + name, OK: false, Mismatch: mm})
			}
		}

		// 2) three-path parity over one shared edit sequence
		edits := standardEdits(pkg.Replay)
		threeOK, mismatch, peak, blocks, detail := verifyThreePaths(pkg.Replay, p, edits, filepath.Join(dbDir, name+".db"))
		if !threeOK {
			rep.add(CheckResult{Name: "three_paths:" + name, OK: false, Mismatch: mismatch, Detail: detail})
		}

		rep.Replays = append(rep.Replays, ReplaySummary{
			ReplayID: name, Events: len(full.EventResults), Segments: len(full.CommitSequence),
			Winner: full.Winner, TruthOK: truthOK, ThreePathOK: threeOK,
			PoolPeak: peak, PoolBlocks: blocks,
		})
		rep.add(CheckResult{
			Name: "replay:" + name, OK: truthOK && threeOK,
			Detail: fmt.Sprintf("events=%d segments=%d winner=%s edits=%d",
				len(full.EventResults), len(full.CommitSequence), full.Winner, len(edits)),
		})
	}

	// 3) standalone capability / error-code scenarios
	runFormatErrorChecks(rep, samplesDir)
	runTimeConversionChecks(rep)
	runConflictChecks(rep)
	runPrecisionCheck(rep)
	runCheckpointInvalidationCheck(rep, dbDir)

	for _, c := range rep.Checks {
		if !c.OK {
			rep.AllOK = false
			break
		}
	}
	return rep, nil
}

func (r *VerificationReport) add(c CheckResult) {
	r.Checks = append(r.Checks, c)
	if !c.OK {
		r.AllOK = false
	}
}

func standardEdits(r *model.Replay) []engine.Edit {
	order := engine.CanonicalOrder(r.Events)
	first := order[0]
	edits := []engine.Edit{
		{Op: "amount", EventID: first.ID, Amount: first.Amount + 7},
	}
	if len(r.Flags) > 0 {
		v := !r.Flags[0].Value
		edits = append(edits, engine.Edit{Op: "set_flag", Flag: r.Flags[0].Name, Bool: &v})
	}
	var maxAt int64
	for _, e := range r.Events {
		if e.At > maxAt {
			maxAt = e.At
		}
	}
	edits = append(edits, engine.Edit{Op: "add_sync", SyncAt: maxAt})
	edits = append(edits, engine.Edit{Op: "time", EventID: first.ID, At: first.At + 3000})
	return edits
}

// verifyThreePaths runs continuous(pooled), incremental and interrupted
// checkpoint-resume paths on independent clones with identical edits.
func verifyThreePaths(base *model.Replay, p model.Params, edits []engine.Edit, dbPath string) (bool, *engine.Mismatch, int, int, string) {
	// Path A: continuous + bounded pool
	cont := base.Clone()
	for _, ed := range edits {
		if _, _, err := engine.ApplyEdit(cont, p, ed); err != nil {
			return false, nil, 0, 0, "continuous edit: " + err.Error()
		}
	}
	contRep, peak, blockRecs, err := engine.SettleFullPooled(cont, p)
	if err != nil {
		return false, nil, peak, len(blockRecs), "continuous settle: " + err.Error()
	}

	// Path B: incremental service
	inc := base.Clone()
	svc := engine.NewService(inc, p)
	if _, err := svc.RunFull(); err != nil {
		return false, nil, 0, 0, "incremental full: " + err.Error()
	}
	var incRep *model.Report
	for _, ed := range edits {
		res, ierr := svc.ApplyEdit(ed)
		if ierr != nil {
			return false, nil, 0, 0, "incremental edit: " + ierr.Error()
		}
		incRep = res.Report
	}
	if incRep == nil {
		incRep = svc.LastReport()
	}
	if mm := engine.CompareReports(contRep, incRep); mm != nil {
		return false, mm, peak, len(blockRecs), "incremental vs continuous"
	}

	// Path C: checkpoint + simulated restart/resume
	cp := base.Clone()
	for _, ed := range edits {
		if _, _, err := engine.ApplyEdit(cp, p, ed); err != nil {
			return false, nil, 0, 0, "checkpoint edit: " + err.Error()
		}
	}
	st, err := store.OpenFile(dbPath)
	if err != nil {
		return false, nil, peak, len(blockRecs), "open checkpoint db: " + err.Error()
	}
	if _, err := engine.RunCheckpointed(cp, p, st, engine.CheckpointOptions{InterruptAfter: 1}); err != nil && err != engine.ErrInterrupted {
		_ = st.Close()
		return false, nil, 0, 0, "checkpoint interrupt run: " + err.Error()
	}
	if err := st.Close(); err != nil {
		return false, nil, 0, 0, "close db: " + err.Error()
	}

	st2, err := store.OpenFile(dbPath)
	if err != nil {
		return false, nil, 0, 0, "reopen checkpoint db: " + err.Error()
	}
	cpRep, err := engine.RunCheckpointed(cp, p, st2, engine.CheckpointOptions{})
	_ = st2.Close()
	if err != nil {
		return false, nil, peak, len(blockRecs), "checkpoint resume: " + err.Error()
	}
	if mm := engine.CompareReports(contRep, cpRep); mm != nil {
		return false, mm, peak, len(blockRecs), "checkpoint vs continuous"
	}
	return true, nil, peak, len(blockRecs), ""
}
