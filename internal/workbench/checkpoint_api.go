package workbench

import (
	"meshpolicy/internal/engine"
	"meshpolicy/internal/store"
	"meshpolicy/internal/xerr"
)

type RestoreResult struct {
	Code           string            `json:"code,omitempty"`
	Recovery       string            `json:"recovery"`
	VersionMatch   bool              `json:"version_match"`
	Checkpoint     *store.Checkpoint `json:"checkpoint"`
	ReplayedReport *engine.RunReport `json:"replayed_report,omitempty"`
	FullReport     *engine.RunReport `json:"full_report,omitempty"`
}

func (s *Service) SaveCheckpoint(seq int64) (*store.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.report == nil {
		r, err := engine.Run(s.project, s.DefaultRunOptions())
		if err != nil {
			return nil, err
		}
		s.report = r
	}
	cp := store.BuildCheckpoint(s.project, s.report, seq)
	st, err := store.Open(s.dbPath)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if err := st.SaveCheckpoint(cp); err != nil {
		return nil, err
	}
	cp.Path = s.dbPath
	return &cp, nil
}

func (s *Service) RestoreCheckpoint() (*RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := store.Open(s.dbPath)
	if err != nil {
		return nil, err
	}
	cp, err := st.LoadCheckpoint()
	_ = st.Close()
	if err != nil {
		return nil, err
	}
	currentVersion := store.CanonicalVersion(s.project)
	if err := store.CheckVersion(cp, currentVersion); err != nil {
		full, runErr := engine.Run(s.project, s.DefaultRunOptions())
		if runErr != nil {
			return nil, runErr
		}
		biz, _ := xerr.As(err)
		code := xerr.ECheckpointVersionMismatch
		if biz != nil {
			code = biz.Code
		}
		return &RestoreResult{
			Code: code, Recovery: "full_recompute_from_start", VersionMatch: false,
			Checkpoint: cp, FullReport: full,
		}, nil
	}
	replayed, err := engine.Run(s.project, engine.RunOptions{Workers: 4, BufferPerNS: 64, StopAtSeq: cp.CommittedSeq})
	if err != nil {
		return nil, err
	}
	return &RestoreResult{
		Recovery: "checkpoint_accepted", VersionMatch: true, Checkpoint: cp, ReplayedReport: replayed,
	}, nil
}
