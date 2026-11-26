package locklab

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type FaultKind string

const (
	FaultDuplicateItem             FaultKind = "duplicate_data_item"
	FaultWorkerBudget              FaultKind = "out_of_range_worker_budget"
	FaultMemoryBudget              FaultKind = "out_of_range_memory_budget"
	FaultWaitGraphDangling         FaultKind = "dangling_wait_graph_reference"
	FaultTimeoutReversed           FaultKind = "reversed_timeout_deadline"
	FaultCheckpointTruncated       FaultKind = "truncated_checkpoint"
	FaultProjectSnapshotMismatch   FaultKind = "project_snapshot_mismatch"
	FaultCheckpointVersionMismatch FaultKind = "checkpoint_version_mismatch"
)

type FaultObservation struct {
	Kind               FaultKind `json:"kind"`
	Code               ErrorCode `json:"code"`
	Path               string    `json:"path,omitempty"`
	Message            string    `json:"message"`
	SourceDigestBefore string    `json:"source_digest_before"`
	SourceDigestAfter  string    `json:"source_digest_after"`
	SourceUnchanged    bool      `json:"source_unchanged"`
	BaselineDigest     string    `json:"baseline_semantic_digest"`
}

type faultCase struct {
	kind FaultKind
	code ErrorCode
	run  func() error
}

// RunFaultMatrix injects every required anomaly into copies and returns the
// expected stable business code plus location. The source project and the
// authoritative checkpoint are never modified.
func RunFaultMatrix(project *Project, checkpointDir string) ([]FaultObservation, error) {
	if project == nil {
		return nil, NewError(EFieldRequired, "$", "project is nil")
	}
	sourceBefore := project.CanonicalDigest()
	baseline, err := Run(project, RunOptions{})
	if err != nil {
		return nil, err
	}
	baselineDigest := baseline.SemanticDigest()
	workDir := checkpointDir
	cleanup := func() {}
	if workDir == "" {
		temp, err := os.MkdirTemp("", "locklab-fault-checkpoint-*")
		if err != nil {
			return nil, WrapError(EInternal, "$.fault.temp", err, "cannot create fault checkpoint dir")
		}
		workDir = temp
		cleanup = func() { _ = os.RemoveAll(temp) }
		store, err := OpenCheckpointStore(workDir)
		if err != nil {
			cleanup()
			return nil, err
		}
		if _, err := Run(project, RunOptions{OnCommit: store.Observer(), StopAfterCommits: 2}); err != nil {
			cleanup()
			return nil, err
		}
	}
	defer cleanup()

	cases := []faultCase{
		{kind: FaultDuplicateItem, code: EDuplicateItem, run: func() error {
			copyProject := project.Clone()
			copyProject.Items = append(copyProject.Items, copyProject.Items[0])
			return ValidateProject(copyProject)
		}},
		{kind: FaultWorkerBudget, code: EBackpressureWorkers, run: func() error {
			copyProject := project.Clone()
			copyProject.WorkerBudget = MaxWorkers + 1
			return ValidateProject(copyProject)
		}},
		{kind: FaultMemoryBudget, code: EBackpressureMemory, run: func() error {
			copyProject := project.Clone()
			copyProject.MemoryBudgetBytes = MinWorkerMemoryBytes - 1
			return ValidateProject(copyProject)
		}},
		{kind: FaultTimeoutReversed, code: ETimeoutReversed, run: func() error {
			data, err := EncodeProject(project, 2)
			if err != nil {
				return err
			}
			corrupt, err := reverseFirstDeadline(data)
			if err != nil {
				return err
			}
			_, err = LoadProject(corrupt)
			return err
		}},
		{kind: FaultWaitGraphDangling, code: EWaitGraphDangling, run: func() error {
			store, err := OpenCheckpointStore(workDir)
			if err != nil {
				return err
			}
			checkpoint, err := store.Load(project)
			if err != nil {
				return err
			}
			bad := append([]WaitGraphEdge(nil), checkpoint.WaitGraph...)
			bad = append(bad, WaitGraphEdge{From: "missing-tx", To: "T1", Resource: "row:x", Step: 1})
			return ValidateWaitGraphReferences(checkpoint.State, bad)
		}},
		{kind: FaultCheckpointTruncated, code: ECheckpointTruncated, run: func() error {
			copyDir, err := copyCheckpointDir(workDir)
			if err != nil {
				return err
			}
			defer os.RemoveAll(copyDir)
			marker := filepath.Join(copyDir, checkpointMarkerName)
			data, err := os.ReadFile(marker)
			if err != nil {
				return err
			}
			if len(data) < 2 {
				return fmt.Errorf("checkpoint marker too small")
			}
			if err := os.WriteFile(marker, data[:len(data)/2], 0o644); err != nil {
				return err
			}
			store, err := OpenCheckpointStore(copyDir)
			if err != nil {
				return err
			}
			_, err = store.Load(project)
			return err
		}},
		{kind: FaultProjectSnapshotMismatch, code: EProjectVersion, run: func() error {
			store, err := OpenCheckpointStore(workDir)
			if err != nil {
				return err
			}
			other := project.Clone()
			other.ID = project.ID + "-other-snapshot"
			_, err = store.Load(other)
			return err
		}},
		{kind: FaultCheckpointVersionMismatch, code: ECheckpointVersion, run: func() error {
			copyDir, err := copyCheckpointDir(workDir)
			if err != nil {
				return err
			}
			defer os.RemoveAll(copyDir)
			marker := filepath.Join(copyDir, checkpointMarkerName)
			data, err := os.ReadFile(marker)
			if err != nil {
				return err
			}
			patched := strings.Replace(string(data), `"store_version": 1`, `"store_version": 999`, 1)
			if patched == string(data) {
				return fmt.Errorf("checkpoint version field not found")
			}
			if err := os.WriteFile(marker, []byte(patched), 0o644); err != nil {
				return err
			}
			store, err := OpenCheckpointStore(copyDir)
			if err != nil {
				return err
			}
			_, err = store.Load(project)
			return err
		}},
	}

	observations := make([]FaultObservation, 0, len(cases))
	for _, testCase := range cases {
		err := testCase.run()
		observation := FaultObservation{
			Kind: testCase.kind, SourceDigestBefore: sourceBefore,
			SourceDigestAfter: project.CanonicalDigest(), BaselineDigest: baselineDigest,
		}
		observation.SourceUnchanged = observation.SourceDigestBefore == observation.SourceDigestAfter
		var businessErr *Error
		if err != nil {
			if AsBusinessError(err, &businessErr) {
				observation.Code = businessErr.Code
				observation.Path = businessErr.Path
				observation.Message = businessErr.Message
			} else {
				observation.Code = EInternal
				observation.Message = err.Error()
			}
		} else {
			observation.Code = "E_NONE"
			observation.Message = "fault injection did not produce an error"
		}
		if observation.Code != testCase.code {
			return observations, NewError(EInternal, "$.fault."+string(testCase.kind),
				"expected %s got %s: %s", testCase.code, observation.Code, observation.Message)
		}
		observations = append(observations, observation)
	}
	sourceAfter := project.CanonicalDigest()
	for _, observation := range observations {
		if !observation.SourceUnchanged || observation.SourceDigestAfter != sourceAfter {
			return observations, NewError(EImmutableSource, "$.fault.source",
				"fault injection mutated the source project")
		}
	}
	after, err := Run(project, RunOptions{})
	if err != nil {
		return observations, err
	}
	if after.SemanticDigest() != baselineDigest {
		return observations, NewError(EImmutableSource, "$.fault.baseline",
			"fault injection changed committed lock/visibility conclusions")
	}
	sort.Slice(observations, func(i, j int) bool {
		return observations[i].Kind < observations[j].Kind
	})
	return observations, nil
}

func AsBusinessError(err error, target **Error) bool {
	if err == nil {
		return false
	}
	current := err
	for current != nil {
		if typed, ok := current.(*Error); ok {
			*target = typed
			return true
		}
		type unwrapper interface{ Unwrap() error }
		next, ok := current.(unwrapper)
		if !ok {
			return false
		}
		current = next.Unwrap()
	}
	return false
}

func reverseFirstDeadline(data []byte) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	transactions, _ := document["transactions"].([]any)
	for _, transaction := range transactions {
		txMap, _ := transaction.(map[string]any)
		operations, _ := txMap["operations"].([]any)
		for _, operation := range operations {
			opMap, _ := operation.(map[string]any)
			deadline, hasDeadline := opMap["waitDeadlineSeconds"].(float64)
			at, hasAt := opMap["atSeconds"].(float64)
			if !hasDeadline || !hasAt {
				continue
			}
			opMap["waitDeadlineSeconds"] = at - 0.25
			_ = deadline
			return json.Marshal(document)
		}
	}
	return nil, fmt.Errorf("no deadline field found")
}

func copyCheckpointDir(source string) (string, error) {
	entries, err := os.ReadDir(source)
	if err != nil {
		return "", err
	}
	target, err := os.MkdirTemp("", "locklab-fault-copy-*")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			_ = os.RemoveAll(target)
			return "", err
		}
		if err := os.WriteFile(filepath.Join(target, entry.Name()), data, 0o644); err != nil {
			_ = os.RemoveAll(target)
			return "", err
		}
	}
	return target, nil
}
