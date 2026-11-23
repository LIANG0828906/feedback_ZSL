package tactical

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type CheckpointKey struct {
	MapHash          string
	SlotVersion      string
	FixedVersion     string
	AlgorithmVersion string
}

type CheckpointStore struct {
	db      *sql.DB
	writeMu sync.Mutex
}

type memberPartition struct {
	Ordinal    int               `json:"ordinal"`
	MemberID   string            `json:"member_id"`
	Path       *MemberPath       `json:"path,omitempty"`
	Smooth     *SmoothedPath     `json:"smooth,omitempty"`
	Trace      *Trace            `json:"trace,omitempty"`
	Dependency *MemberDependency `json:"dependency,omitempty"`
}

func OpenCheckpointStore(path string) (*CheckpointStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &CheckpointStore{db: db}
	for _, pragma := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.ExecContext(context.Background(), pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoints (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			map_hash TEXT NOT NULL,
			slot_version TEXT NOT NULL,
			fixed_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			commit_seq INTEGER NOT NULL,
			saved_at TEXT NOT NULL,
			payload BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS member_partitions (
			checkpoint_id INTEGER NOT NULL,
			commit_seq INTEGER NOT NULL,
			ordinal INTEGER NOT NULL,
			member_id TEXT NOT NULL,
			payload BLOB NOT NULL,
			PRIMARY KEY (checkpoint_id, member_id),
			FOREIGN KEY (checkpoint_id) REFERENCES checkpoints(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS commit_log (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			checkpoint_id INTEGER NOT NULL,
			commit_seq INTEGER NOT NULL,
			ordinal INTEGER NOT NULL,
			member_id TEXT NOT NULL,
			committed_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *CheckpointStore) Close() error {
	return s.db.Close()
}

// Save commits the full snapshot and every member partition in one serialized
// transaction. Member rows are inserted in the snapshot's stable member order.
func (s *CheckpointStore) Save(ctx context.Context, snapshot *Snapshot) (CheckpointMeta, error) {
	if snapshot == nil {
		return CheckpointMeta{}, fmt.Errorf("snapshot is nil")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	saved := cloneSnapshot(snapshot)
	if saved.SavedAt.IsZero() {
		saved.SavedAt = time.Now().UTC()
	}
	nextSeq, err := s.nextCommitSeq(ctx)
	if err != nil {
		return CheckpointMeta{}, err
	}
	if saved.CommitSeq <= 0 {
		saved.CommitSeq = nextSeq
	} else if saved.CommitSeq < nextSeq {
		saved.CommitSeq = nextSeq
	}
	order := saved.MemberOrder
	if len(order) == 0 {
		order = SortedKeys(saved.Paths)
		saved.MemberOrder = append([]string(nil), order...)
	}
	payload, err := json.Marshal(saved)
	if err != nil {
		return CheckpointMeta{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckpointMeta{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM member_partitions WHERE checkpoint_id = 1`); err != nil {
		return CheckpointMeta{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE id = 1`); err != nil {
		return CheckpointMeta{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints
		(id, map_hash, slot_version, fixed_version, algorithm_version, commit_seq, saved_at, payload)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)`,
		saved.MapHash, saved.SlotVersion, saved.FixedVersion, saved.AlgorithmVersion,
		saved.CommitSeq, saved.SavedAt.Format(time.RFC3339Nano), payload); err != nil {
		return CheckpointMeta{}, err
	}
	dependencyByID := make(map[string]MemberDependency, len(saved.Dependencies.Members))
	for _, dep := range saved.Dependencies.Members {
		dependencyByID[dep.MemberID] = dep
	}
	for ordinal, memberID := range order {
		partition := memberPartition{Ordinal: ordinal, MemberID: memberID}
		if path, ok := saved.Paths[memberID]; ok {
			copyPath := path
			partition.Path = &copyPath
		}
		if smooth, ok := saved.Smooth[memberID]; ok {
			copySmooth := smooth
			partition.Smooth = &copySmooth
		}
		if trace, ok := saved.Traces[memberID]; ok {
			copyTrace := trace
			partition.Trace = &copyTrace
		}
		if dep, ok := dependencyByID[memberID]; ok {
			copyDep := dep
			partition.Dependency = &copyDep
		}
		partitionPayload, err := json.Marshal(partition)
		if err != nil {
			return CheckpointMeta{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO member_partitions
			(checkpoint_id, commit_seq, ordinal, member_id, payload) VALUES (1, ?, ?, ?, ?)`,
			saved.CommitSeq, ordinal, memberID, partitionPayload); err != nil {
			return CheckpointMeta{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO commit_log
			(checkpoint_id, commit_seq, ordinal, member_id, committed_at) VALUES (1, ?, ?, ?, ?)`,
			saved.CommitSeq, ordinal, memberID, saved.SavedAt.Format(time.RFC3339Nano)); err != nil {
			return CheckpointMeta{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CheckpointMeta{}, err
	}
	return metaFromSnapshot(saved, true, ""), nil
}

func (s *CheckpointStore) nextCommitSeq(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(commit_seq) FROM checkpoints`).Scan(&seq); err != nil {
		return 0, err
	}
	if !seq.Valid {
		return 1, nil
	}
	return seq.Int64 + 1, nil
}

// Load returns a snapshot only when every validity key matches. On any
// mismatch the whole checkpoint is deleted and a stale metadata record is
// returned with a nil snapshot.
func (s *CheckpointStore) Load(ctx context.Context, expected CheckpointKey) (*Snapshot, CheckpointMeta, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var (
		meta    CheckpointMeta
		payload []byte
		savedAt string
	)
	err := s.db.QueryRowContext(ctx, `SELECT map_hash, slot_version, fixed_version,
		algorithm_version, commit_seq, saved_at, payload FROM checkpoints WHERE id = 1`).
		Scan(&meta.MapHash, &meta.SlotVersion, &meta.FixedVersion, &meta.AlgorithmVersion,
			&meta.CommitSeq, &savedAt, &payload)
	if err == sql.ErrNoRows {
		return nil, CheckpointMeta{Valid: false, StaleReason: "checkpoint not found"}, nil
	}
	if err != nil {
		return nil, CheckpointMeta{}, err
	}
	meta.SavedAt, _ = time.Parse(time.RFC3339Nano, savedAt)
	if reason := checkpointMismatch(meta, expected); reason != "" {
		if err := s.clearLocked(ctx); err != nil {
			return nil, meta, err
		}
		meta.Valid = false
		meta.StaleReason = reason
		return nil, meta, nil
	}
	var snapshot Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		if clearErr := s.clearLocked(ctx); clearErr != nil {
			return nil, meta, clearErr
		}
		meta.Valid = false
		meta.StaleReason = "checkpoint payload is corrupt"
		return nil, meta, nil
	}
	if reason := s.verifyPartitionsLocked(ctx, &snapshot); reason != "" {
		if err := s.clearLocked(ctx); err != nil {
			return nil, meta, err
		}
		meta.Valid = false
		meta.StaleReason = reason
		return nil, meta, nil
	}
	meta.Valid = true
	return &snapshot, meta, nil
}

func (s *CheckpointStore) verifyPartitionsLocked(ctx context.Context, snapshot *Snapshot) string {
	rows, err := s.db.QueryContext(ctx, `SELECT ordinal, member_id FROM member_partitions
		WHERE checkpoint_id = 1 ORDER BY ordinal ASC`)
	if err != nil {
		return "checkpoint member partitions cannot be read"
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var ordinal int
		var memberID string
		if err := rows.Scan(&ordinal, &memberID); err != nil {
			return "checkpoint member partition is corrupt"
		}
		if index >= len(snapshot.MemberOrder) || ordinal != index || snapshot.MemberOrder[index] != memberID {
			return "checkpoint member partition order does not match snapshot"
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return "checkpoint member partitions cannot be read"
	}
	if index != len(snapshot.MemberOrder) {
		return "checkpoint member partition count does not match snapshot"
	}
	return ""
}

func checkpointMismatch(meta CheckpointMeta, expected CheckpointKey) string {
	switch {
	case meta.MapHash != expected.MapHash:
		return "map content hash changed"
	case meta.SlotVersion != expected.SlotVersion:
		return "formation slot version changed"
	case meta.FixedVersion != expected.FixedVersion:
		return "fixed-point parameter version changed"
	case meta.AlgorithmVersion != expected.AlgorithmVersion:
		return "algorithm version changed"
	default:
		return ""
	}
}

func (s *CheckpointStore) Clear(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.clearLocked(ctx)
}

func (s *CheckpointStore) clearLocked(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM member_partitions`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commit_log`); err != nil {
		return err
	}
	return tx.Commit()
}

// CommitOrder exposes the durable, stable member write order for diagnostics
// and acceptance checks.
func (s *CheckpointStore) CommitOrder(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT member_id FROM commit_log ORDER BY seq ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		order = append(order, id)
	}
	return order, rows.Err()
}

func metaFromSnapshot(snapshot Snapshot, valid bool, reason string) CheckpointMeta {
	return CheckpointMeta{
		MapHash: snapshot.MapHash, SlotVersion: snapshot.SlotVersion,
		FixedVersion: snapshot.FixedVersion, AlgorithmVersion: snapshot.AlgorithmVersion,
		CommitSeq: snapshot.CommitSeq, SavedAt: snapshot.SavedAt,
		Valid: valid, StaleReason: reason,
	}
}

func cloneSnapshot(snapshot *Snapshot) Snapshot {
	payload, _ := json.Marshal(snapshot)
	var out Snapshot
	_ = json.Unmarshal(payload, &out)
	return out
}
