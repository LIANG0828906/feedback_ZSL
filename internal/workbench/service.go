package workbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"enginelab.local/internal/checkpoint"
	"enginelab.local/internal/gen"
	"enginelab.local/internal/model"
	"enginelab.local/internal/recovery"
)

type Config struct {
	Seed    int64
	DBPath  string
	ResetDB bool
	Budget  model.Budget
	DataDir string
}

type Moment struct {
	Index       int                   `json:"index"`
	Key         string                `json:"key"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	Edit        *recovery.Edit        `json:"edit,omitempty"`
	Result      *model.RecoveryResult `json:"result"`
	Full        *model.RecoveryResult `json:"full,omitempty"`
	Comparison  recovery.Comparison   `json:"comparison"`
	Graph       *recovery.GraphReport `json:"graph"`
}

type Service struct {
	cfg      Config
	bundle   *gen.Bundle
	engine   *recovery.Engine
	store    *checkpoint.Store
	dbPath   string
	timeline []*Moment
}

func DefaultBudget() model.Budget {
	return model.Budget{Workers: 4, QueueDepth: 16, MaxSegmentMemoryBytes: recovery.DefaultMaxSegmentMemoryBytes}
}

func NewService(cfg Config) (*Service, error) {
	if cfg.Seed == 0 {
		cfg.Seed = gen.DefaultSeed
	}
	if cfg.Budget.Workers == 0 && cfg.Budget.QueueDepth == 0 && cfg.Budget.MaxSegmentMemoryBytes == 0 {
		cfg.Budget = DefaultBudget()
	}
	budget, err := recovery.NormalizeBudget(cfg.Budget)
	if err != nil {
		return nil, err
	}
	cfg.Budget = budget
	dbPath := cfg.DBPath
	if dbPath == "" {
		dataDir := cfg.DataDir
		if dataDir == "" {
			dataDir = filepath.Join("artifacts")
		}
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, err
		}
		dbPath = filepath.Join(dataDir, "workbench.sqlite")
	}
	if cfg.ResetDB {
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	} else if dir := filepath.Dir(dbPath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	bundle := gen.NewBundle(cfg.Seed)
	engine, err := recovery.NewEngine(bundle.Project, budget, bundle.Truth.CheckpointItems)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	s := &Service{cfg: cfg, bundle: bundle, engine: engine, store: store, dbPath: dbPath}
	if err := s.buildTimeline(); err != nil {
		_ = store.Close()
		return nil, err
	}
	if _, err := s.SaveCheckpoint(context.Background(), 0); err != nil {
		_ = store.Close()
		return nil, err
	}
	return s, nil
}

func (s *Service) Close() error {
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func (s *Service) MarkGracefulShutdown(ctx context.Context, signalName string) error {
	if s.store == nil {
		return nil
	}
	return s.store.SetMeta(ctx, "last_shutdown", "graceful:"+signalName)
}

func (s *Service) Bundle() *gen.Bundle { return s.bundle }
func (s *Service) DBPath() string      { return s.dbPath }
func (s *Service) Timeline() []*Moment { return s.timeline }

func (s *Service) Moment(index int) (*Moment, error) {
	if index < 0 || index >= len(s.timeline) {
		return nil, model.NewError(model.EFieldRequired, "$.moment", "moment %d out of range [0,%d]", index, len(s.timeline)-1)
	}
	return s.timeline[index], nil
}

func (s *Service) buildTimeline() error {
	baseline := s.engine.LastResult
	full0, err := recovery.RecoverOneShot(s.bundle.Project, recovery.Options{Budget: s.cfg.Budget, BaselineItems: s.bundle.Truth.CheckpointItems})
	if err != nil {
		return err
	}
	s.timeline = []*Moment{{
		Index:       0,
		Key:         "baseline",
		Title:       "固定种子基线恢复",
		Description: "三版 schema 已归一；cp-1 之前段跳过，seg-003/004/005 并行重放。",
		Result:      baseline,
		Full:        full0,
		Comparison:  recovery.CompareResults(baseline, full0),
		Graph:       recovery.BuildGraph(s.bundle.Project),
	}}
	edits := []recovery.Edit{
		{ID: "m1-truncate", Type: recovery.EditTruncateSegment, SegmentID: "seg-004", ToRecordCount: 5, Description: "截断 seg-004 到第 5 条完整记录，移除 tx-b 提交与尾部。"},
		{ID: "m2-overwrite", Type: recovery.EditOverwriteRecord, SegmentID: "seg-003", RecordID: "rec-seg-003-a-put", NewValue: "overwritten-delta", NewVersion: 9001, Description: "覆盖 seg-003 已提交事务的 put 值。"},
		{ID: "m3-checkpoint", Type: recovery.EditAddCheckpoint, SegmentID: "seg-004", CheckpointID: "cp-2", Description: "在截断后的 seg-004 尾新增 cp-2。"},
		{ID: "m4-dependency", Type: recovery.EditRewriteDependency, SegmentID: "seg-005", NewPredecessors: []string{"seg-003"}, Description: "seg-005 前驱由 seg-004 改写为 seg-003。"},
	}
	for i, edit := range edits {
		result, err := s.engine.Apply(edit)
		if err != nil {
			return err
		}
		full, err := recovery.RecoverOneShot(s.engine.Project, recovery.Options{Budget: s.cfg.Budget})
		if err != nil {
			return err
		}
		s.timeline = append(s.timeline, &Moment{
			Index:       i + 1,
			Key:         edit.ID,
			Title:       edit.Description,
			Description: edit.Description,
			Edit:        &edit,
			Result:      result,
			Full:        full,
			Comparison:  recovery.CompareResults(result, full),
			Graph:       recovery.BuildGraph(s.engine.Project),
		})
	}
	return nil
}

func (s *Service) SaveCheckpoint(ctx context.Context, momentIndex int) (string, error) {
	moment, err := s.Moment(momentIndex)
	if err != nil {
		return "", err
	}
	cpID := moment.Result.CheckpointID
	if cpID == "" {
		cpID = "cp-moment"
	}
	if momentIndex > 0 {
		cpID = cpID + "-m" + itoa(momentIndex)
	}
	return s.store.SaveRecovery(ctx, checkpoint.SaveRequest{
		CheckpointID:  cpID,
		ProjectID:     s.bundle.Project.ID,
		SchemaVersion: model.CheckpointSchemaVersion,
		Result:        moment.Result,
	})
}

type RestoreReport struct {
	Checkpoint  *checkpoint.PersistedCheckpoint `json:"checkpoint"`
	Parity      bool                            `json:"parity"`
	Differences []string                        `json:"differences,omitempty"`
	Current     *model.RecoveryResult           `json:"current,omitempty"`
}

type ResumeReport struct {
	Mode         string                `json:"mode"`
	Restore      *RestoreReport        `json:"restore,omitempty"`
	VersionError *model.Error          `json:"version_error,omitempty"`
	Full         *model.RecoveryResult `json:"full,omitempty"`
}

func (s *Service) RestoreCheckpoint(ctx context.Context) (*RestoreReport, error) {
	persisted, err := s.store.LoadCommitted(ctx, model.CheckpointSchemaVersion)
	if err != nil {
		return nil, err
	}
	moment := s.timeline[0]
	report := &RestoreReport{Checkpoint: persisted, Parity: true, Current: moment.Result}
	if len(persisted.Segments) != len(moment.Result.Segments) {
		report.Parity = false
		report.Differences = append(report.Differences, "segment count mismatch")
	}
	for _, ps := range persisted.Segments {
		current := moment.Result.SegmentMap[ps.SegmentID]
		if current == nil {
			report.Parity = false
			report.Differences = append(report.Differences, "missing current segment "+ps.SegmentID)
			continue
		}
		if ps.Progress.AppliedCount != current.AppliedRecords || ps.Progress.LastLSN != current.LastRecovered.LSN || ps.Progress.LastRecordID != current.LastRecovered.RecordID {
			report.Parity = false
			report.Differences = append(report.Differences, "progress mismatch at "+ps.SegmentID)
		}
	}
	return report, nil
}

func (s *Service) Resume(ctx context.Context) (*ResumeReport, error) {
	restore, err := s.RestoreCheckpoint(ctx)
	if err == nil {
		return &ResumeReport{Mode: "committed_checkpoint", Restore: restore}, nil
	}
	var be *model.Error
	if errors.As(err, &be) && be.Code == model.EVersionMismatch {
		p, cloneErr := recovery.CloneProject(s.bundle.Project)
		if cloneErr != nil {
			return nil, cloneErr
		}
		p.Checkpoints = nil
		p.LatestCheckpoint = nil
		full, replayErr := recovery.RecoverOneShot(p, recovery.Options{Budget: s.cfg.Budget, BaselineItems: map[string]model.DataItem{}})
		if replayErr != nil {
			return nil, replayErr
		}
		return &ResumeReport{Mode: "full_replay_version_mismatch", VersionError: be, Full: full}, nil
	}
	if errors.Is(err, checkpoint.ErrNoCommittedCheckpoint) {
		full, replayErr := recovery.RecoverOneShot(s.bundle.Project, recovery.Options{Budget: s.cfg.Budget})
		if replayErr != nil {
			return nil, replayErr
		}
		return &ResumeReport{Mode: "full_replay_no_checkpoint", Full: full}, nil
	}
	return nil, err
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
