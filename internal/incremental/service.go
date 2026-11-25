package incremental

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"

	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
)

type State string

const (
	StateReused     State = "REUSED_FROM_CACHE"
	StateRecomputed State = "RECOMPUTED_DEPENDENCY"
	StateNew        State = "NEW_OPERATION"
	StateDeleted    State = "DELETED"
)

type ReadResult struct {
	OpID          string              `json:"op_id"`
	State         State               `json:"state"`
	Decision      domain.ReadDecision `json:"decision"`
	InvalidatedBy []string            `json:"invalidated_by"`
	CacheBasis    string              `json:"cache_basis"`
}

type GCResult struct {
	StepID        string            `json:"step_id"`
	State         State             `json:"state"`
	Decision      domain.GCDecision `json:"decision"`
	InvalidatedBy []string          `json:"invalidated_by"`
	CacheBasis    string            `json:"cache_basis"`
}

type Analysis struct {
	EditType        string                `json:"edit_type"`
	ReadResults     map[string]ReadResult `json:"read_results"`
	GCResults       map[string]GCResult   `json:"gc_results"`
	ReusedReads     []string              `json:"reused_reads"`
	RecomputedReads []string              `json:"recomputed_reads"`
	ReusedGC        []string              `json:"reused_gc"`
	RecomputedGC    []string              `json:"recomputed_gc"`
	DeletedOpIDs    []string              `json:"deleted_op_ids,omitempty"`
}

type Service struct {
	original  *domain.Project
	current   *domain.Project
	options   engine.Options
	readCache map[string]domain.ReadDecision
	gcCache   map[string]domain.GCDecision
}

func NewService(project *domain.Project, options engine.Options) (*Service, error) {
	report, err := engine.Run(context.Background(), project, options)
	if err != nil {
		return nil, err
	}
	clone, err := CloneProject(project)
	if err != nil {
		return nil, err
	}
	s := &Service{
		original:  project,
		current:   clone,
		options:   options,
		readCache: map[string]domain.ReadDecision{},
		gcCache:   map[string]domain.GCDecision{},
	}
	for _, read := range report.Reads {
		s.readCache[read.OpID] = read
	}
	for _, gc := range report.GC {
		s.gcCache[gc.StepID] = gc
	}
	return s, nil
}

func CloneProject(project *domain.Project) (*domain.Project, error) {
	data, err := json.Marshal(project)
	if err != nil {
		return nil, domain.NewError(domain.EInputInvalidOperation, "cannot clone project").With("error", err.Error())
	}
	var clone domain.Project
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, domain.NewError(domain.EInputInvalidOperation, "cannot decode project clone").With("error", err.Error())
	}
	return &clone, nil
}

func (s *Service) CurrentProject() *domain.Project {
	clone, _ := CloneProject(s.current)
	return clone
}

func (s *Service) OriginalProjectUnchanged() (bool, error) {
	// The service never mutates s.original. This helper gives diagnostics a
	// stable deep-equality check against the caller's original pointer content
	// only when that pointer was not externally mutated.
	originalClone, err := CloneProject(s.original)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(originalClone, s.original), nil
}

// ChangeTxnStart changes one start logical time and invalidates the dependency
// closure through that transaction.
func (s *Service) ChangeTxnStart(txnID uint64, startMS int64) (*Analysis, error) {
	candidate, err := CloneProject(s.current)
	if err != nil {
		return nil, err
	}
	var oldMS int64
	found := false
	for i := range candidate.Txns {
		if candidate.Txns[i].ID == txnID {
			oldMS = candidate.Txns[i].StartMS
			candidate.Txns[i].StartMS = startMS
			found = true
			break
		}
	}
	if !found {
		return nil, domain.NewError(domain.EInputInvalidRef, "transaction not found").With("txn_id", txnID)
	}
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	sources := map[string][]string{}
	source := fmt.Sprintf("edit:txn:%d:start_ms:%d->%d", txnID, oldMS, startMS)
	s.collectTxnReadDependencies(txnID, source, sources)
	if s.commitOrderChanged(candidate) {
		for id := range s.readCache {
			sources[id] = appendUnique(sources[id], "dependency:commit-order-window")
		}
	}
	return s.commitCandidate(candidate, "change_start_ms", sources, nil, nil)
}

// ChangeIsolation changes one declared isolation level.
func (s *Service) ChangeIsolation(txnID uint64, isolation domain.Isolation) (*Analysis, error) {
	candidate, err := CloneProject(s.current)
	if err != nil {
		return nil, err
	}
	found := false
	for i := range candidate.Txns {
		if candidate.Txns[i].ID == txnID {
			candidate.Txns[i].Isolation = isolation
			found = true
			break
		}
	}
	if !found {
		return nil, domain.NewError(domain.EInputInvalidRef, "transaction not found").With("txn_id", txnID)
	}
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	sources := map[string][]string{}
	s.collectTxnReadDependencies(txnID, "edit:txn:"+uintString(txnID)+":isolation", sources)
	return s.commitCandidate(candidate, "change_isolation", sources, nil, nil)
}

// AppendOp appends a new declared operation. Existing prefix results stay
// cached; only the new operation and its downstream GC are computed.
func (s *Service) AppendOp(op domain.Op) (*Analysis, error) {
	candidate, err := CloneProject(s.current)
	if err != nil {
		return nil, err
	}
	if op.Index == 0 {
		op.Index = maxIndex(candidate.Ops) + 1
	}
	if op.ID == "" {
		op.ID = "OP-" + padOperation(op.Index)
	}
	candidate.Ops = append(candidate.Ops, op)
	candidate.Canonicalize()
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	readSources := map[string][]string{}
	gcSources := map[string][]string{}
	if op.Kind == domain.OpRead {
		readSources[op.ID] = []string{"edit:append_op:" + op.ID}
	}
	if op.Kind == domain.OpGC {
		stepID := op.GCStepID
		if stepID == "" {
			stepID = "GC-STEP-APPEND-" + op.ID
		}
		for i := range candidate.Ops {
			if candidate.Ops[i].ID == op.ID {
				candidate.Ops[i].GCStepID = stepID
			}
		}
		gcSources[stepID] = []string{"edit:append_gc:" + op.ID}
	}
	return s.commitCandidate(candidate, "append_op", readSources, gcSources, nil)
}

// DeleteOp removes one operation and recomputes the logical suffix that can
// depend on it. The prefix remains cache-backed.
func (s *Service) DeleteOp(opID string) (*Analysis, error) {
	candidate, err := CloneProject(s.current)
	if err != nil {
		return nil, err
	}
	var deleted domain.Op
	index := -1
	for i, op := range candidate.Ops {
		if op.ID == opID {
			deleted = op
			index = i
			break
		}
	}
	if index < 0 {
		return nil, domain.NewError(domain.EInputInvalidOperation, "operation not found").With("op_id", opID)
	}
	candidate.Ops = append(candidate.Ops[:index], candidate.Ops[index+1:]...)
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	readSources := map[string][]string{}
	gcSources := map[string][]string{}
	for id, read := range s.readCache {
		if read.OpIndex >= deleted.Index {
			readSources[id] = []string{"edit:delete_op:" + opID, "dependency:logical-suffix"}
		}
	}
	for id, gc := range s.gcCache {
		if gc.AfterOpIndex >= deleted.Index {
			gcSources[id] = []string{"edit:delete_op:" + opID, "dependency:gc-suffix"}
		}
	}
	return s.commitCandidate(candidate, "delete_op", readSources, gcSources, []string{opID})
}

func (s *Service) collectTxnReadDependencies(txnID uint64, source string, sources map[string][]string) {
	for opID, read := range s.readCache {
		reasons := []string{}
		if read.TxnID == txnID {
			reasons = append(reasons, source, "dependency:txn-owned-read")
		}
		if read.VisibleVersionID != "" {
			if version := findVersion(s.current, read.VisibleVersionID); version != nil && version.CreateTxn == txnID {
				reasons = append(reasons, source, "dependency:visible-version:"+version.ID)
			}
		}
		for _, skipped := range read.Skipped {
			if skipped.CreateTxn == txnID {
				reasons = append(reasons, source, "dependency:skipped-version:"+skipped.VersionID)
			}
		}
		if len(reasons) > 0 {
			sources[opID] = appendUnique(sources[opID], reasons...)
		}
	}
}

func (s *Service) commitOrderChanged(candidate *domain.Project) bool {
	oldOrder := engine.DeriveCommitOrder(s.current)
	newOrder := engine.DeriveCommitOrder(candidate)
	return !reflect.DeepEqual(oldOrder, newOrder)
}

func (s *Service) commitCandidate(
	candidate *domain.Project,
	editType string,
	readSources map[string][]string,
	gcSources map[string][]string,
	deletedIDs []string,
) (*Analysis, error) {
	if readSources == nil {
		readSources = map[string][]string{}
	}
	if gcSources == nil {
		gcSources = map[string][]string{}
	}
	oldSnapshotTxns := map[string]uint64{}
	for _, read := range s.readCache {
		oldSnapshotTxns[read.SnapshotID] = read.TxnID
	}
	changedSnapshots := map[string]bool{}
	for opID := range readSources {
		if read, ok := s.readCache[opID]; ok {
			changedSnapshots[read.SnapshotID] = true
		}
	}
	for stepID, gc := range s.gcCache {
		if _, already := gcSources[stepID]; already {
			continue
		}
		for _, snapshotID := range gc.ActiveSnapshotIDs {
			if changedSnapshots[snapshotID] {
				gcSources[stepID] = []string{"dependency:active-snapshot:" + snapshotID}
				break
			}
		}
	}

	analysis := &Analysis{
		EditType:     editType,
		ReadResults:  map[string]ReadResult{},
		GCResults:    map[string]GCResult{},
		DeletedOpIDs: deletedIDs,
	}

	nextReadCache := map[string]domain.ReadDecision{}
	for _, op := range candidate.Ops {
		if op.Kind != domain.OpRead {
			continue
		}
		sources := readSources[op.ID]
		_, dirty := readSources[op.ID]
		_, cached := s.readCache[op.ID]
		if !cached {
			dirty = true
			sources = appendUnique(sources, "cache:miss:new-operation")
		}
		if dirty {
			decision, err := engine.EvaluateRead(candidate, op.ID)
			if err != nil {
				return nil, err
			}
			nextReadCache[op.ID] = *decision
			state := StateRecomputed
			if !cached {
				state = StateNew
			}
			analysis.ReadResults[op.ID] = ReadResult{
				OpID: op.ID, State: state, Decision: *decision,
				InvalidatedBy: appendUnique([]string{}, sources...),
				CacheBasis:    "recomputed-from-declared-stream",
			}
			analysis.RecomputedReads = append(analysis.RecomputedReads, op.ID)
		} else {
			decision := s.readCache[op.ID]
			nextReadCache[op.ID] = decision
			analysis.ReadResults[op.ID] = ReadResult{
				OpID: op.ID, State: StateReused, Decision: decision,
				CacheBasis: "baseline-read-cache:" + op.ID,
			}
			analysis.ReusedReads = append(analysis.ReusedReads, op.ID)
		}
	}

	nextGCCache := map[string]domain.GCDecision{}
	for _, op := range candidate.Ops {
		if op.Kind != domain.OpGC {
			continue
		}
		stepID := op.GCStepID
		sources, dirty := gcSources[stepID]
		_, cached := s.gcCache[stepID]
		if !cached {
			dirty = true
			sources = appendUnique(sources, "cache:miss:new-gc-step")
		}
		if dirty {
			decision, err := engine.EvaluateGCAt(context.Background(), candidate, stepID)
			if err != nil {
				return nil, err
			}
			nextGCCache[stepID] = *decision
			state := StateRecomputed
			if !cached {
				state = StateNew
			}
			analysis.GCResults[stepID] = GCResult{
				StepID: stepID, State: state, Decision: *decision,
				InvalidatedBy: appendUnique([]string{}, sources...),
				CacheBasis:    "recomputed-gc-prefix",
			}
			analysis.RecomputedGC = append(analysis.RecomputedGC, stepID)
		} else {
			decision := s.gcCache[stepID]
			nextGCCache[stepID] = decision
			analysis.GCResults[stepID] = GCResult{
				StepID: stepID, State: StateReused, Decision: decision,
				CacheBasis: "baseline-gc-cache:" + stepID,
			}
			analysis.ReusedGC = append(analysis.ReusedGC, stepID)
		}
	}

	sort.Strings(analysis.ReusedReads)
	sort.Strings(analysis.RecomputedReads)
	sort.Strings(analysis.ReusedGC)
	sort.Strings(analysis.RecomputedGC)
	s.current = candidate
	s.readCache = nextReadCache
	s.gcCache = nextGCCache
	return analysis, nil
}

func findVersion(project *domain.Project, id string) *domain.Version {
	for i := range project.Versions {
		if project.Versions[i].ID == id {
			return &project.Versions[i]
		}
	}
	return nil
}

func appendUnique(existing []string, values ...string) []string {
	seen := map[string]bool{}
	out := append([]string{}, existing...)
	for _, value := range out {
		seen[value] = true
	}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func uintString(v uint64) string {
	return strconv.FormatUint(v, 10)
}

func maxIndex(ops []domain.Op) int {
	max := 0
	for _, op := range ops {
		if op.Index > max {
			max = op.Index
		}
	}
	return max
}

func padOperation(index int) string {
	return fmt.Sprintf("%03d", index)
}
