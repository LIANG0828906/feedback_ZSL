package leasekit

import (
	"os"
	"path/filepath"
)

type ExceptionObservation struct {
	Case    string `json:"case"`
	Code    string `json:"code"`
	Context string `json:"context"`
	Stable  bool   `json:"stable"`
}

func InjectDuplicateHolder(p *Project) *Project {
	cp := p.Clone()
	if len(cp.Holders) > 0 {
		first := cp.Holders[0]
		cp.Holders = append(cp.Holders, Holder{ID: first.ID, ResourceID: first.ResourceID, Priority: first.Priority})
	}
	return cp
}

func InjectRenewalDuration(p *Project, seq int, duration Milli) *Project {
	cp := p.Clone()
	for i := range cp.Operations {
		if cp.Operations[i].Seq == seq {
			cp.Operations[i].Kind = OpRenew
			cp.Operations[i].RenewForMS = duration
		}
	}
	return cp
}

func InjectWindowParameter(p *Project, resourceID string, window Milli) *Project {
	cp := p.Clone()
	for i := range cp.Resources {
		if cp.Resources[i].ID == resourceID {
			cp.Resources[i].RenewWindowMS = window
		}
	}
	return cp
}

func errorObservation(name string, err error) ExceptionObservation {
	obs := ExceptionObservation{Case: name}
	if biz, ok := AsError(err); ok {
		obs.Code = biz.Code
		obs.Context = biz.Error()
		obs.Stable = true
	} else if err != nil {
		obs.Code = "E_UNCLASSIFIED"
		obs.Context = err.Error()
	}
	return obs
}

func DrillExceptions(p *Project, workDir string) ([]ExceptionObservation, error) {
	var out []ExceptionObservation

	if err := ValidateProject(InjectDuplicateHolder(p)); err != nil {
		out = append(out, errorObservation("duplicate_holder_id", err))
	}
	if _, err := Run(InjectRenewalDuration(p, 10, -50)); err != nil {
		out = append(out, errorObservation("negative_renewal_duration", err))
	}
	if _, err := Run(InjectRenewalDuration(p, 10, 1_000_000_000)); err != nil {
		out = append(out, errorObservation("out_of_bounds_renewal_duration", err))
	}

	truth, err := GenerateTruth(p)
	if err != nil {
		return nil, err
	}
	limitResult := truth.Report.ResultBySeq(26)
	if limitResult != nil {
		out = append(out, ExceptionObservation{
			Case: "renewal_request_beyond_max", Code: limitResult.Code,
			Context: limitResult.Source.Reason, Stable: limitResult.Code == CodeRenewLimitReached,
		})
	}

	if _, err := ValidateParallelOptions(ParallelOptions{Workers: MaxWorkerBudget + 64}); err != nil {
		out = append(out, errorObservation("worker_budget_out_of_range", err))
	}
	if _, err := ValidateParallelOptions(ParallelOptions{AttemptBudgetPerResource: -1}); err != nil {
		out = append(out, errorObservation("attempt_budget_out_of_range", err))
	}

	truncDir := filepath.Join(workDir, "truncated-file")
	if err := os.MkdirAll(truncDir, 0o755); err != nil {
		return nil, err
	}
	truncStore := NewFileCheckpointStore(truncDir)
	if err := os.WriteFile(filepath.Join(truncDir, "commit-seq.json"), []byte(`{"committedSeq":12,`), 0o644); err != nil {
		return nil, err
	}
	if _, err := truncStore.LoadCheckpoint(); err != nil {
		out = append(out, errorObservation("truncated_checkpoint", err))
	}

	versionDir := filepath.Join(workDir, "version-mismatch-file")
	versionStore := NewFileCheckpointStore(versionDir)
	cp, err := BuildCheckpoint(p, 12)
	if err != nil {
		return nil, err
	}
	cp.SchemaVersion = "lease-project/v0-snapshot"
	if err := versionStore.WriteHolderStates(*cp); err != nil {
		return nil, err
	}
	if err := versionStore.WriteCommittedSeq(*cp); err != nil {
		return nil, err
	}
	if _, err := versionStore.LoadCheckpoint(); err != nil {
		out = append(out, errorObservation("version_mismatch_snapshot", err))
	}

	sqlitePath := filepath.Join(workDir, "version-mismatch.db")
	sqliteStore := NewSQLiteCheckpointStore(sqlitePath)
	goodCP, err := BuildCheckpoint(p, 12)
	if err != nil {
		return nil, err
	}
	if commitErr := CommitCheckpoint(sqliteStore, goodCP); commitErr != nil {
		if biz, ok := AsError(commitErr); !ok || biz.Code != "E_SQLITE_CGO_DISABLED" {
			return nil, commitErr
		}
	} else {
		if db, err := openSQLite(sqlitePath); err == nil {
			_ = db.exec("UPDATE checkpoint_meta SET value='lease-checkpoint/v0' WHERE key='version'")
			_ = db.close()
		}
		if _, err := sqliteStore.LoadCheckpoint(); err != nil {
			out = append(out, errorObservation("sqlite_version_mismatch_snapshot", err))
		}
	}

	return out, nil
}
