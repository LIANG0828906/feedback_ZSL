package locklab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	CheckpointFormat       = "locklab-checkpoint/v1"
	CheckpointStoreVersion = 1
	checkpointMarkerName   = "checkpoint.commit"
	checkpointBodyPrefix   = "checkpoint-body-"
	checkpointExtension    = ".json"
)

type checkpointMarker struct {
	Format            string `json:"format"`
	StoreVersion      int    `json:"store_version"`
	ProjectID         string `json:"project_id"`
	ProjectDigest     string `json:"project_digest"`
	BodyGeneration    string `json:"body_generation"`
	BodySHA256        string `json:"body_sha256"`
	CommitSeq         uint64 `json:"commit_seq"`
	LastCommittedStep int    `json:"last_committed_step"`
	SavedAtUnixNano   int64  `json:"saved_at_unix_nano"`
}

type checkpointBody struct {
	Format        string              `json:"format"`
	StoreVersion  int                 `json:"store_version"`
	ProjectID     string              `json:"project_id"`
	ProjectDigest string              `json:"project_digest"`
	State         *RuntimeState       `json:"state"`
	WaitGraph     []WaitGraphEdge     `json:"wait_graph"`
	WaitQueues    []WaitQueueSnapshot `json:"wait_queues"`
}

type Checkpoint struct {
	StoreVersion      int                 `json:"store_version"`
	ProjectID         string              `json:"project_id"`
	ProjectDigest     string              `json:"project_digest"`
	CommitSeq         uint64              `json:"commit_seq"`
	LastCommittedStep int                 `json:"last_committed_step"`
	State             *RuntimeState       `json:"state"`
	WaitGraph         []WaitGraphEdge     `json:"wait_graph"`
	WaitQueues        []WaitQueueSnapshot `json:"wait_queues"`
	SavedAt           time.Time           `json:"saved_at"`
}

type CheckpointStore struct {
	Dir     string
	Version int
	Now     func() time.Time
}

func OpenCheckpointStore(dir string) (*CheckpointStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, NewError(EFieldRequired, "$.checkpoint_dir", "checkpoint directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, WrapError(EInternal, "$.checkpoint_dir", err, "cannot create checkpoint directory")
	}
	return &CheckpointStore{Dir: dir, Version: CheckpointStoreVersion, Now: time.Now}, nil
}

func (s *CheckpointStore) markerPath() string {
	return filepath.Join(s.Dir, checkpointMarkerName)
}

func (s *CheckpointStore) bodyPath(generation string) string {
	return filepath.Join(s.Dir, checkpointBodyPrefix+generation+checkpointExtension)
}

// Observer can be passed to RunOptions.OnCommit to checkpoint every commit.
func (s *CheckpointStore) Observer() CommitObserver {
	return func(state *RuntimeState, commits []CommitEvent) error {
		return s.Save(state, commits)
	}
}

type CheckpointSaveOptions struct {
	FailAfterBody bool
}

func (s *CheckpointStore) Save(state *RuntimeState, commits []CommitEvent) error {
	return s.SaveWithOptions(state, commits, CheckpointSaveOptions{})
}

// SaveWithOptions writes transaction/lock state to an immutable generation
// first, then atomically publishes a marker containing the global commit
// sequence. A crash before marker publication leaves the previous checkpoint
// intact.
func (s *CheckpointStore) SaveWithOptions(state *RuntimeState, commits []CommitEvent, options CheckpointSaveOptions) error {
	if s == nil {
		return NewError(EFieldRequired, "$.checkpoint", "checkpoint store is nil")
	}
	if state == nil {
		return NewError(EFieldRequired, "$.checkpoint.state", "runtime state is nil")
	}
	if s.Version == 0 {
		s.Version = CheckpointStoreVersion
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	stateCopy, err := cloneRuntimeState(state)
	if err != nil {
		return err
	}
	graph := WaitGraphFromState(stateCopy)
	if err := ValidateWaitGraphReferences(stateCopy, graph); err != nil {
		return err
	}
	lastStep := 0
	if len(commits) > 0 {
		lastStep = commits[len(commits)-1].GlobalStep
	} else if len(stateCopy.Commits) > 0 {
		lastStep = stateCopy.Commits[len(stateCopy.Commits)-1].GlobalStep
	}
	commitSeq := stateCopy.CommitSeq
	// The body intentionally does not carry the global commit counter. The
	// marker is the commit point and restores that counter after validation.
	stateCopy.CommitSeq = 0
	body := checkpointBody{
		Format: CheckpointFormat, StoreVersion: s.Version,
		ProjectID: stateCopy.ProjectID, ProjectDigest: stateCopy.ProjectDigest,
		State: stateCopy, WaitGraph: graph, WaitQueues: stateCopy.WaitQueuesSnapshot(),
	}
	bodyData, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return WrapError(EInternal, "$.checkpoint.body", err, "cannot encode checkpoint body")
	}
	sum := sha256.Sum256(bodyData)
	bodyHash := hex.EncodeToString(sum[:])
	generation := fmt.Sprintf("%020d-%s", commitSeq, bodyHash[:16])
	bodyPath := s.bodyPath(generation)
	if err := writeFileSync(bodyPath, bodyData); err != nil {
		return WrapError(EInternal, "$.checkpoint.body", err, "cannot write checkpoint body")
	}
	if err := syncDir(s.Dir); err != nil {
		return WrapError(EInternal, "$.checkpoint.body", err, "cannot sync checkpoint directory")
	}
	if options.FailAfterBody {
		return NewError(EInterrupted, "$.checkpoint.marker",
			"simulated termination after body fsync and before commit marker")
	}
	marker := checkpointMarker{
		Format: CheckpointFormat, StoreVersion: s.Version,
		ProjectID: stateCopy.ProjectID, ProjectDigest: stateCopy.ProjectDigest,
		BodyGeneration: generation, BodySHA256: bodyHash,
		CommitSeq: commitSeq, LastCommittedStep: lastStep,
		SavedAtUnixNano: now().UnixNano(),
	}
	markerData, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return WrapError(EInternal, "$.checkpoint.marker", err, "cannot encode checkpoint marker")
	}
	tempMarker := s.markerPath() + ".tmp"
	if err := writeFileSync(tempMarker, markerData); err != nil {
		return WrapError(EInternal, "$.checkpoint.marker", err, "cannot write checkpoint marker")
	}
	if err := os.Rename(tempMarker, s.markerPath()); err != nil {
		return WrapError(EInternal, "$.checkpoint.marker", err, "cannot publish checkpoint marker")
	}
	if err := syncDir(s.Dir); err != nil {
		return WrapError(EInternal, "$.checkpoint.marker", err, "cannot sync checkpoint directory")
	}
	s.cleanupBodies(generation)
	return nil
}

func (s *CheckpointStore) cleanupBodies(keep string) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, checkpointBodyPrefix) || !strings.HasSuffix(name, checkpointExtension) {
			continue
		}
		generation := strings.TrimSuffix(strings.TrimPrefix(name, checkpointBodyPrefix), checkpointExtension)
		if generation == keep {
			continue
		}
		_ = os.Remove(filepath.Join(s.Dir, name))
	}
}

func (s *CheckpointStore) Load(project *Project) (*Checkpoint, error) {
	checkpoint, err := s.LoadRaw()
	if err != nil {
		return nil, err
	}
	if project != nil {
		if checkpoint.ProjectID != project.ID {
			return nil, NewError(EProjectVersion, "$.checkpoint.project_id",
				"checkpoint project id %q does not match %q", checkpoint.ProjectID, project.ID)
		}
		if checkpoint.ProjectDigest != project.CanonicalDigest() {
			return nil, NewError(EProjectVersion, "$.checkpoint.project_digest",
				"checkpoint project snapshot digest does not match project")
		}
	}
	return checkpoint, nil
}

func (s *CheckpointStore) LoadRaw() (*Checkpoint, error) {
	if s == nil {
		return nil, NewError(EFieldRequired, "$.checkpoint", "checkpoint store is nil")
	}
	expectedVersion := s.Version
	if expectedVersion == 0 {
		expectedVersion = CheckpointStoreVersion
	}
	markerData, err := os.ReadFile(s.markerPath())
	if err != nil {
		if os.IsNotExist(err) {
			if s.hasCheckpointFragments() {
				return nil, NewError(ECheckpointTruncated, "$.checkpoint.commit",
					"commit marker is missing; only uncommitted checkpoint fragments exist")
			}
			return nil, WrapError(ECheckpointNotFound, "$.checkpoint.commit", err,
				"no checkpoint found in %s", s.Dir)
		}
		return nil, WrapError(EInternal, "$.checkpoint.commit", err, "cannot read checkpoint marker")
	}
	var marker checkpointMarker
	if err := json.Unmarshal(markerData, &marker); err != nil {
		return nil, WrapError(ECheckpointTruncated, "$.checkpoint.commit", err,
			"checkpoint commit marker is truncated or invalid")
	}
	if marker.Format != CheckpointFormat || marker.StoreVersion != expectedVersion {
		return nil, NewError(ECheckpointVersion, "$.checkpoint.commit.store_version",
			"checkpoint version %d/%q does not match expected %d/%q",
			marker.StoreVersion, marker.Format, expectedVersion, CheckpointFormat)
	}
	bodyPath := s.bodyPath(marker.BodyGeneration)
	bodyData, err := os.ReadFile(bodyPath)
	if err != nil {
		return nil, WrapError(ECheckpointTruncated, "$.checkpoint.body", err,
			"checkpoint body %s is missing or unreadable", marker.BodyGeneration)
	}
	sum := sha256.Sum256(bodyData)
	if hex.EncodeToString(sum[:]) != marker.BodySHA256 {
		return nil, NewError(ECheckpointTruncated, "$.checkpoint.body",
			"checkpoint body checksum mismatch")
	}
	var body checkpointBody
	if err := json.Unmarshal(bodyData, &body); err != nil {
		return nil, WrapError(ECheckpointTruncated, "$.checkpoint.body", err,
			"checkpoint body is truncated or invalid")
	}
	if body.Format != CheckpointFormat || body.StoreVersion != expectedVersion {
		return nil, NewError(ECheckpointVersion, "$.checkpoint.body.store_version",
			"checkpoint body version does not match expected version")
	}
	if body.State == nil {
		return nil, NewError(ECheckpointTruncated, "$.checkpoint.body.state", "checkpoint state is missing")
	}
	if body.ProjectID != marker.ProjectID || body.ProjectDigest != marker.ProjectDigest ||
		body.State.ProjectID != marker.ProjectID || body.State.ProjectDigest != marker.ProjectDigest {
		return nil, NewError(EProjectVersion, "$.checkpoint.project_digest",
			"checkpoint body and marker project snapshots do not match")
	}
	if err := ValidateWaitGraphReferences(body.State, body.WaitGraph); err != nil {
		return nil, err
	}
	body.State.CommitSeq = marker.CommitSeq
	return &Checkpoint{
		StoreVersion: marker.StoreVersion, ProjectID: marker.ProjectID,
		ProjectDigest: marker.ProjectDigest, CommitSeq: marker.CommitSeq,
		LastCommittedStep: marker.LastCommittedStep, State: body.State,
		WaitGraph: body.WaitGraph, WaitQueues: body.WaitQueues,
		SavedAt: time.Unix(0, marker.SavedAtUnixNano),
	}, nil
}

func (s *CheckpointStore) hasCheckpointFragments() bool {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, checkpointBodyPrefix) || strings.Contains(name, ".tmp") {
			return true
		}
	}
	return false
}

func (state *RuntimeState) WaitQueuesSnapshot() []WaitQueueSnapshot {
	if state == nil {
		return nil
	}
	resources := make([]string, 0, len(state.WaitQueues))
	for resource, queue := range state.WaitQueues {
		if len(queue) > 0 {
			resources = append(resources, resource)
		}
	}
	sort.Strings(resources)
	out := make([]WaitQueueSnapshot, 0, len(resources))
	for _, resource := range resources {
		snapshot := WaitQueueSnapshot{Resource: resource}
		for i, wait := range state.WaitQueues[resource] {
			snapshot.Members = append(snapshot.Members, WaitMember{
				Seat: i + 1, WaitID: wait.ID, TxID: wait.TxID, Step: wait.Step,
				Mode: wait.Mode, DeadlineMs: wait.DeadlineMs,
			})
		}
		out = append(out, snapshot)
	}
	return out
}

// WaitGraphFromState reconstructs the wait-for graph from a checkpoint or an
// engine snapshot.
func WaitGraphFromState(state *RuntimeState) []WaitGraphEdge {
	if state == nil {
		return nil
	}
	edges := make([]WaitGraphEdge, 0)
	resources := make([]string, 0, len(state.WaitQueues))
	for resource := range state.WaitQueues {
		resources = append(resources, resource)
	}
	sort.Strings(resources)
	for _, resource := range resources {
		queue := state.WaitQueues[resource]
		for index, wait := range queue {
			requested := HeldLock{
				TxID: wait.TxID, Resource: resource, Granularity: wait.Granularity,
				Table: wait.Table, ItemID: wait.ItemID, Mode: wait.Mode, AcquiredStep: wait.Step,
			}
			if lock, ok := state.Locks[wait.TxID+"|"+resource]; ok {
				requested.Mode = MergeModes(lock.Mode, wait.Mode)
			}
			for _, holder := range conflictingLocks(wait.TxID, requested, state.Locks) {
				edges = append(edges, WaitGraphEdge{
					From: wait.TxID, To: holder, Resource: resource, Step: wait.Step,
				})
			}
			for earlier := 0; earlier < index; earlier++ {
				if queue[earlier].TxID != wait.TxID {
					edges = append(edges, WaitGraphEdge{
						From: wait.TxID, To: queue[earlier].TxID, Resource: resource, Step: wait.Step,
					})
				}
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		if edges[i].Resource != edges[j].Resource {
			return edges[i].Resource < edges[j].Resource
		}
		return edges[i].Step < edges[j].Step
	})
	return edges
}

type RecoveryInfo struct {
	Resumed           bool      `json:"resumed"`
	FromStep          int       `json:"from_step"`
	FromCommitSeq     uint64    `json:"from_commit_seq"`
	LastCommittedStep int       `json:"last_committed_step"`
	FallbackCode      ErrorCode `json:"fallback_code,omitempty"`
	FallbackReason    string    `json:"fallback_reason,omitempty"`
}

// ResumeOrRecompute resumes a matching checkpoint. Version/project mismatch
// falls back to a full deterministic replay; corruption is returned as a hard
// error so an invalid checkpoint cannot silently become authoritative.
func ResumeOrRecompute(project *Project, store *CheckpointStore, options RunOptions) (*RunResult, RecoveryInfo, error) {
	checkpoint, err := store.Load(project)
	if err != nil {
		switch ErrorCodeOf(err) {
		case ECheckpointNotFound:
			result, runErr := Run(project, options)
			return result, RecoveryInfo{Resumed: false, FallbackCode: ECheckpointNotFound,
				FallbackReason: err.Error()}, runErr
		case ECheckpointVersion, EProjectVersion:
			result, runErr := Run(project, options)
			return result, RecoveryInfo{Resumed: false, FallbackCode: ErrorCodeOf(err),
				FallbackReason: err.Error()}, runErr
		default:
			return nil, RecoveryInfo{}, err
		}
	}
	result, err := ResumeRun(project, checkpoint.State, options)
	if err != nil {
		return nil, RecoveryInfo{}, err
	}
	return result, RecoveryInfo{
		Resumed: true, FromStep: checkpoint.State.NextStep,
		FromCommitSeq:     checkpoint.CommitSeq,
		LastCommittedStep: checkpoint.LastCommittedStep,
	}, nil
}

func writeFileSync(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ExportSQL writes a SQLite-importable snapshot. Transaction/lock/queue rows
// are emitted before checkpoint_meta, whose commit_seq is the final INSERT.
// The runtime itself uses the crash-safe file checkpoint above because Go's
// standard library has no SQLite driver.
func (s *CheckpointStore) ExportSQL(path string) error {
	checkpoint, err := s.LoadRaw()
	if err != nil {
		return err
	}
	var builder strings.Builder
	builder.WriteString("PRAGMA foreign_keys=ON;\nBEGIN IMMEDIATE;\n")
	builder.WriteString("DROP TABLE IF EXISTS checkpoint_meta;\n")
	builder.WriteString("DROP TABLE IF EXISTS tx_state;\n")
	builder.WriteString("DROP TABLE IF EXISTS lock_state;\n")
	builder.WriteString("DROP TABLE IF EXISTS wait_graph;\n")
	builder.WriteString("DROP TABLE IF EXISTS wait_queue;\n")
	builder.WriteString("DROP TABLE IF EXISTS committed_step;\n")
	builder.WriteString("CREATE TABLE checkpoint_meta(store_version INTEGER NOT NULL, project_id TEXT NOT NULL, project_digest TEXT NOT NULL, commit_seq INTEGER NOT NULL, last_committed_step INTEGER NOT NULL);\n")
	builder.WriteString("CREATE TABLE tx_state(tx_id TEXT PRIMARY KEY, priority INTEGER NOT NULL, isolation TEXT NOT NULL, snapshot_seq INTEGER NOT NULL, done INTEGER NOT NULL, rolled_back INTEGER NOT NULL, pending_writes_json TEXT NOT NULL);\n")
	builder.WriteString("CREATE TABLE lock_state(tx_id TEXT NOT NULL, resource TEXT NOT NULL, mode TEXT NOT NULL, granularity TEXT NOT NULL, table_name TEXT NOT NULL, item_id TEXT, PRIMARY KEY(tx_id,resource));\n")
	builder.WriteString("CREATE TABLE wait_graph(from_tx TEXT NOT NULL, to_tx TEXT NOT NULL, resource TEXT NOT NULL, step INTEGER NOT NULL);\n")
	builder.WriteString("CREATE TABLE wait_queue(resource TEXT NOT NULL, seat INTEGER NOT NULL, tx_id TEXT NOT NULL, wait_id TEXT NOT NULL, mode TEXT NOT NULL, deadline_ms INTEGER NOT NULL);\n")
	builder.WriteString("CREATE TABLE committed_step(commit_seq INTEGER NOT NULL, tx_id TEXT NOT NULL, global_step INTEGER NOT NULL, executed_step INTEGER NOT NULL);\n")

	txIDs := make([]string, 0, len(checkpoint.State.Transactions))
	for id := range checkpoint.State.Transactions {
		txIDs = append(txIDs, id)
	}
	sort.Strings(txIDs)
	for _, id := range txIDs {
		tx := checkpoint.State.Transactions[id]
		pending, _ := json.Marshal(tx.PendingWrites)
		fmt.Fprintf(&builder,
			"INSERT INTO tx_state VALUES (%s,%d,%s,%d,%d,%d,%s);\n",
			sqlString(id), tx.Priority, sqlString(string(tx.Isolation)), tx.SnapshotSeq,
			boolInt(tx.Done), boolInt(tx.RolledBack), sqlString(string(pending)))
	}
	for _, commit := range checkpoint.State.Commits {
		fmt.Fprintf(&builder,
			"INSERT INTO committed_step VALUES (%d,%s,%d,%d);\n",
			commit.CommitSeq, sqlString(commit.TxID), commit.GlobalStep, commit.ExecutedStep)
	}
	lockKeys := sortedLockKeys(checkpoint.State.Locks, "")
	for _, key := range lockKeys {
		lock := checkpoint.State.Locks[key]
		fmt.Fprintf(&builder,
			"INSERT INTO lock_state VALUES (%s,%s,%s,%s,%s,%s);\n",
			sqlString(lock.TxID), sqlString(lock.Resource), sqlString(string(lock.Mode)),
			sqlString(string(lock.Granularity)), sqlString(lock.Table), sqlNullable(lock.ItemID))
	}
	for _, edge := range checkpoint.WaitGraph {
		fmt.Fprintf(&builder,
			"INSERT INTO wait_graph VALUES (%s,%s,%s,%d);\n",
			sqlString(edge.From), sqlString(edge.To), sqlString(edge.Resource), edge.Step)
	}
	for _, queue := range checkpoint.WaitQueues {
		for _, member := range queue.Members {
			fmt.Fprintf(&builder,
				"INSERT INTO wait_queue VALUES (%s,%d,%s,%s,%s,%d);\n",
				sqlString(queue.Resource), member.Seat, sqlString(member.TxID),
				sqlString(member.WaitID), sqlString(string(member.Mode)), member.DeadlineMs)
		}
	}
	fmt.Fprintf(&builder,
		"INSERT INTO checkpoint_meta VALUES (%d,%s,%s,%d,%d);\nCOMMIT;\n",
		checkpoint.StoreVersion, sqlString(checkpoint.ProjectID),
		sqlString(checkpoint.ProjectDigest), checkpoint.CommitSeq, checkpoint.LastCommittedStep)
	if err := writeFileSync(path, []byte(builder.String())); err != nil {
		return WrapError(EInternal, "$.checkpoint.sql", err, "cannot write SQL checkpoint export")
	}
	return nil
}

func sqlString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func sqlNullable(value string) string {
	if value == "" {
		return "NULL"
	}
	return sqlString(value)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
