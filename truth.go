package locklab

import (
	"encoding/json"
	"fmt"
	"sort"
)

const TruthFormat = "locklab-truth/v1"

// Truth is the deterministic oracle emitted from the canonical parsed project.
// It contains no wall-clock diagnostics and is stable across all three schemas.
type Truth struct {
	Format            string                         `json:"format"`
	ProjectID         string                         `json:"project_id"`
	ProjectDigest     string                         `json:"project_digest"`
	Seed              int64                          `json:"seed"`
	LockCompatibility map[LockMode]map[LockMode]bool `json:"lock_compatibility"`
	Steps             []StepResult                   `json:"steps"`
	QueueEvents       []QueueEvent                   `json:"queue_events"`
	Reads             []ReadObservation              `json:"reads"`
	Writes            []WriteObservation             `json:"writes"`
	Commits           []CommitEvent                  `json:"commits"`
	Rollbacks         []RollbackEvent                `json:"rollbacks"`
	FinalWrites       map[string]VersionRecord       `json:"final_writes"`
	Anomalies         []Anomaly                      `json:"anomalies"`
	DeadlockVictims   []string                       `json:"deadlock_victims"`
	SemanticDigest    string                         `json:"semantic_digest"`
}

func GenerateTruth(project *Project) (*Truth, error) {
	result, err := Run(project, RunOptions{})
	if err != nil {
		return nil, err
	}
	return TruthFromResult(result), nil
}

func TruthFromResult(result *RunResult) *Truth {
	compat := make(map[LockMode]map[LockMode]bool, len(CanonicalLockCompatibility))
	for mode, row := range CanonicalLockCompatibility {
		compat[mode] = make(map[LockMode]bool, len(row))
		for other, allowed := range row {
			compat[mode][other] = allowed
		}
	}
	truth := &Truth{
		Format: TruthFormat, ProjectID: result.ProjectID,
		ProjectDigest: result.ProjectDigest, Seed: result.Seed, LockCompatibility: compat,
		Steps: result.Steps, QueueEvents: result.QueueEvents,
		Reads: result.Reads, Writes: result.Writes, Commits: result.Commits,
		Rollbacks: result.Rollbacks, FinalWrites: result.FinalWrites,
		Anomalies: result.Anomalies, DeadlockVictims: result.DeadlockVictims,
		SemanticDigest: result.SemanticDigest(),
	}
	// The generator's canonical project carries the seed even after schema
	// decoding; keep it in the truth for independent regeneration.
	return truth
}

func GenerateTruthForProject(project *Project) (*Truth, error) {
	truth, err := GenerateTruth(project)
	if err != nil {
		return nil, err
	}
	return truth, nil
}

func (t *Truth) AsRunResult() *RunResult {
	return &RunResult{
		ProjectID: t.ProjectID, ProjectDigest: t.ProjectDigest, Seed: t.Seed,
		Steps: t.Steps, QueueEvents: t.QueueEvents, Reads: t.Reads,
		Writes: t.Writes, Commits: t.Commits, Rollbacks: t.Rollbacks,
		FinalWrites: t.FinalWrites, Anomalies: t.Anomalies,
		DeadlockVictims: t.DeadlockVictims,
	}
}

func VerifyTruth(project *Project, truth *Truth) error {
	if truth == nil {
		return NewError(ETruthMismatch, "$.truth", "truth is nil")
	}
	if truth.Format != TruthFormat {
		return NewError(ETruthMismatch, "$.truth.format", "unsupported truth format %q", truth.Format)
	}
	if truth.ProjectDigest != project.CanonicalDigest() {
		return NewError(ETruthMismatch, "$.truth.project_digest",
			"truth project digest does not match loaded project")
	}
	if truth.Seed != project.Seed {
		return NewError(ETruthMismatch, "$.truth.seed",
			"truth seed %d does not match project seed %d", truth.Seed, project.Seed)
	}
	result, err := Run(project, RunOptions{})
	if err != nil {
		return err
	}
	if len(result.Steps) != len(truth.Steps) {
		return NewError(ETruthMismatch, "$.truth.steps",
			"step count mismatch: truth=%d actual=%d", len(truth.Steps), len(result.Steps))
	}
	for i := range result.Steps {
		if !semanticStepEqual(result.Steps[i], truth.Steps[i]) {
			return NewError(ETruthMismatch, fmt.Sprintf("$.truth.steps[%d]", i),
				"logical step differs for %s/%d", truth.Steps[i].TxID, truth.Steps[i].LocalSeq)
		}
	}
	if !semanticJSONEqual(result.Reads, truth.Reads) {
		return NewError(ETruthMismatch, "$.truth.reads", "read visibility differs")
	}
	if !semanticJSONEqual(result.FinalWrites, truth.FinalWrites) {
		return NewError(ETruthMismatch, "$.truth.final_writes", "final committed write set differs")
	}
	if !semanticJSONEqual(result.Rollbacks, truth.Rollbacks) {
		return NewError(ETruthMismatch, "$.truth.rollbacks", "rollback set differs")
	}
	if !semanticJSONEqual(result.DeadlockVictims, truth.DeadlockVictims) {
		return NewError(ETruthMismatch, "$.truth.deadlock_victims", "deadlock victim set differs")
	}
	if result.SemanticDigest() != truth.SemanticDigest {
		return NewError(ETruthMismatch, "$.truth.semantic_digest",
			"semantic digest mismatch: truth=%s actual=%s", truth.SemanticDigest, result.SemanticDigest())
	}
	return nil
}

func semanticJSONEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func EncodeTruth(truth *Truth) ([]byte, error) {
	data, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, WrapError(EInternal, "$.truth", err, "cannot encode truth")
	}
	return append(data, '\n'), nil
}

func DecodeTruth(data []byte) (*Truth, error) {
	var truth Truth
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, WrapError(EJSONInvalid, "$.truth", err, "cannot decode truth")
	}
	return &truth, nil
}

func (t *Truth) RollbackReasons() map[string]string {
	out := make(map[string]string)
	for _, rollback := range t.Rollbacks {
		out[rollback.TxID] = rollback.Reason
	}
	return out
}

func SortedTxIDs(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
