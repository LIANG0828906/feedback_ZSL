package balance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Checkpoint struct {
	SchemaVersion    string                     `json:"schemaVersion"`
	ProjectHash      string                     `json:"projectHash"`
	BudgetHash       string                     `json:"budgetHash"`
	BudgetVersion    string                     `json:"budgetVersion"`
	AlgorithmVersion string                     `json:"algorithmVersion"`
	CommitSequence   int64                      `json:"commitSequence"`
	Combos           map[string]ComboCheckpoint `json:"combos"`
	Plans            map[string]PlanCheckpoint  `json:"plans"`
}

type ComboCheckpoint struct {
	CommitSeq int64       `json:"commitSeq"`
	Report    ComboReport `json:"report"`
}

type PlanCheckpoint struct {
	CommitSeq        int64              `json:"commitSeq"`
	PlanID           string             `json:"planId"`
	State            PlanState          `json:"state"`
	ComboKeys        []string           `json:"comboKeys"`
	CommittedCombos  int                `json:"committedCombos"`
	RequestedMatches int                `json:"requestedMatches"`
	AllocatedMatches int                `json:"allocatedMatches"`
	CompletedMatches int                `json:"completedMatches"`
	Wins             int                `json:"wins"`
	Losses           int                `json:"losses"`
	Draws            int                `json:"draws"`
	WinRatePPM       int64              `json:"winRatePpm"`
	CI               ConfidenceInterval `json:"confidenceInterval"`
	Damage           DamageSummary      `json:"damage"`
	DamageDigest     string             `json:"damageDigest"`
	RNG              RNGProgress        `json:"rngProgress"`
}

func newCheckpoint(project *Project) *Checkpoint {
	return &Checkpoint{
		SchemaVersion:    SchemaVersion,
		ProjectHash:      ProjectContentHash(project),
		BudgetHash:       BudgetHash(project.Budget),
		BudgetVersion:    project.BudgetVersion,
		AlgorithmVersion: project.AlgorithmVersion,
		Combos:           make(map[string]ComboCheckpoint),
		Plans:            make(map[string]PlanCheckpoint),
	}
}

func (c *Checkpoint) Clone() *Checkpoint {
	if c == nil {
		return nil
	}
	copy := &Checkpoint{
		SchemaVersion:    c.SchemaVersion,
		ProjectHash:      c.ProjectHash,
		BudgetHash:       c.BudgetHash,
		BudgetVersion:    c.BudgetVersion,
		AlgorithmVersion: c.AlgorithmVersion,
		CommitSequence:   c.CommitSequence,
		Combos:           make(map[string]ComboCheckpoint, len(c.Combos)),
		Plans:            make(map[string]PlanCheckpoint, len(c.Plans)),
	}
	for key, value := range c.Combos {
		copy.Combos[key] = value
	}
	for key, value := range c.Plans {
		copy.Plans[key] = value
	}
	return copy
}

func LoadCheckpoint(path string, project *Project) (*Checkpoint, bool, string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, true, "checkpoint file does not exist", nil
	}
	if err != nil {
		return nil, true, "", &AppError{Code: CodeIO, Object: path, Detail: err.Error()}
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return nil, true, "checkpoint JSON is corrupt: " + err.Error(), nil
	}
	if checkpoint.Combos == nil {
		checkpoint.Combos = make(map[string]ComboCheckpoint)
	}
	if checkpoint.Plans == nil {
		checkpoint.Plans = make(map[string]PlanCheckpoint)
	}
	if reason := checkpointMismatchReason(&checkpoint, project); reason != "" {
		return nil, true, reason, nil
	}
	return &checkpoint, false, "", nil
}

func checkpointMismatchReason(checkpoint *Checkpoint, project *Project) string {
	if checkpoint.SchemaVersion != SchemaVersion {
		return fmt.Sprintf("schema version mismatch: checkpoint=%s project=%s", checkpoint.SchemaVersion, SchemaVersion)
	}
	if checkpoint.ProjectHash != ProjectContentHash(project) {
		return "project content hash mismatch"
	}
	if checkpoint.BudgetHash != BudgetHash(project.Budget) {
		return "budget parameter hash mismatch"
	}
	if checkpoint.BudgetVersion != project.BudgetVersion {
		return fmt.Sprintf("budget version mismatch: checkpoint=%s project=%s", checkpoint.BudgetVersion, project.BudgetVersion)
	}
	if checkpoint.AlgorithmVersion != project.AlgorithmVersion {
		return fmt.Sprintf("algorithm version mismatch: checkpoint=%s project=%s", checkpoint.AlgorithmVersion, project.AlgorithmVersion)
	}
	return ""
}

type CheckpointStore struct {
	mu   sync.Mutex
	path string
	cp   *Checkpoint
}

func NewCheckpointStore(path string, initial *Checkpoint) *CheckpointStore {
	if initial == nil {
		initial = &Checkpoint{
			SchemaVersion: SchemaVersion,
			Combos:        make(map[string]ComboCheckpoint),
			Plans:         make(map[string]PlanCheckpoint),
		}
	}
	return &CheckpointStore{path: path, cp: initial.Clone()}
}

func (s *CheckpointStore) CommitCombo(report ComboReport) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := report.Key()
	if existing, ok := s.cp.Combos[key]; ok {
		return existing.CommitSeq, &AppError{
			Code:       CodeState,
			Object:     key,
			PlanID:     report.PlanID,
			ScenarioID: report.ScenarioID,
			Field:      "checkpoint.combo",
			Detail:     "committed combination cannot be overwritten",
		}
	}
	next := s.cp.Clone()
	next.CommitSequence++
	next.Combos[key] = ComboCheckpoint{CommitSeq: next.CommitSequence, Report: report}
	if err := writeCheckpointAtomic(s.path, next); err != nil {
		return 0, err
	}
	s.cp = next
	return next.CommitSequence, nil
}

func (s *CheckpointStore) CommitPlan(report PlanReport) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.cp.Plans[report.PlanID]; ok {
		return existing.CommitSeq, &AppError{
			Code:   CodeState,
			Object: report.PlanID,
			PlanID: report.PlanID,
			Field:  "checkpoint.plan",
			Detail: "committed plan cannot be overwritten",
		}
	}
	next := s.cp.Clone()
	next.CommitSequence++
	keys := make([]string, 0, len(report.Combos))
	for _, combo := range report.Combos {
		keys = append(keys, combo.Key())
	}
	next.Plans[report.PlanID] = PlanCheckpoint{
		CommitSeq:        next.CommitSequence,
		PlanID:           report.PlanID,
		State:            PlanCommitted,
		ComboKeys:        keys,
		CommittedCombos:  report.CommittedCombos,
		RequestedMatches: report.RequestedMatches,
		AllocatedMatches: report.AllocatedMatches,
		CompletedMatches: report.CompletedMatches,
		Wins:             report.Wins,
		Losses:           report.Losses,
		Draws:            report.Draws,
		WinRatePPM:       report.WinRatePPM,
		CI:               report.CI,
		Damage:           report.Damage,
		DamageDigest:     PlanDamageDigest(report),
		RNG:              planRNGProgress(report),
	}
	if err := writeCheckpointAtomic(s.path, next); err != nil {
		return 0, err
	}
	s.cp = next
	return next.CommitSequence, nil
}

func (s *CheckpointStore) Initialize() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cp.Clone()
	if err := writeCheckpointAtomic(s.path, next); err != nil {
		return err
	}
	s.cp = next
	return nil
}

func (s *CheckpointStore) Snapshot() *Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cp.Clone()
}

func writeCheckpointAtomic(path string, checkpoint *Checkpoint) error {
	if path == "" {
		return &AppError{Code: CodeCheckpoint, Object: path, Detail: "checkpoint path is empty"}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &AppError{Code: CodeIO, Object: dir, Detail: err.Error()}
	}
	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return &AppError{Code: CodeCheckpoint, Object: path, Detail: err.Error()}
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return &AppError{Code: CodeIO, Object: path, Detail: err.Error()}
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return &AppError{Code: CodeIO, Object: tmpName, Detail: err.Error()}
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return &AppError{Code: CodeIO, Object: tmpName, Detail: err.Error()}
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return &AppError{Code: CodeIO, Object: tmpName, Detail: err.Error()}
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return &AppError{Code: CodeIO, Object: path, Detail: err.Error()}
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}
