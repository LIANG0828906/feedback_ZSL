package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"mvccworkbench/internal/domain"
)

const (
	StateSnapshotVersion = domain.CheckpointFormat + "#snapshot-state/v1"
	DefaultFileName      = "mvcc_checkpoint.db"
)

var ErrNotFound = domain.NewError("E_CHECKPOINT_NOT_FOUND", "checkpoint has no committed global sequence")

type SnapshotState struct {
	Version        string   `json:"version"`
	ID             string   `json:"id"`
	OwnerTxn       uint64   `json:"owner_txn,omitempty"`
	CreatedOpIndex int      `json:"created_op_index"`
	Horizon        bool     `json:"horizon,omitempty"`
	CommittedSet   []uint64 `json:"committed_set"`
	CutoffSeq      int      `json:"cutoff_seq"`
}

type State struct {
	FormatVersion  string                      `json:"format_version"`
	ProjectVersion string                      `json:"project_version"`
	SourceSchema   string                      `json:"source_schema,omitempty"`
	ProjectID      string                      `json:"project_id"`
	BatchID        string                      `json:"batch_id"`
	Seed           int64                       `json:"seed"`
	CommittedOpSeq int                         `json:"committed_op_seq"`
	Chains         map[string][]domain.Version `json:"chains"`
	Reclaimed      map[string]bool             `json:"reclaimed"`
	Snapshots      []SnapshotState             `json:"snapshots"`
	RecordedAt     string                      `json:"recorded_at"`
}

type Store struct {
	path string
	db   *sql.DB

	// FaultBeforeCommit is used by deterministic durability tests. It is not
	// set by normal workbench execution.
	FaultBeforeCommit func() error
}

func Open(path string) (*Store, error) {
	dsn := path + "?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, domain.NewError(domain.ECkptOpen, "cannot open sqlite checkpoint").
			At(path).With("error", err.Error())
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, classifySQLiteError(path, "ping", err)
	}
	store := &Store{path: path, db: db}
	if err := store.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	var mode string
	_ = db.QueryRow("PRAGMA synchronous").Scan(&mode)
	return store, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) initSchema(ctx context.Context) error {
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			format_version TEXT NOT NULL,
			project_version TEXT NOT NULL,
			source_schema TEXT,
			project_id TEXT NOT NULL,
			batch_id TEXT NOT NULL,
			seed INTEGER NOT NULL,
			committed_op_seq INTEGER NOT NULL,
			recorded_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_chains (
			item_id TEXT PRIMARY KEY,
			chain_json TEXT NOT NULL,
			reclaimed_ids_json TEXT NOT NULL,
			committed_seq INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_snapshots (
			snapshot_id TEXT PRIMARY KEY,
			owner_txn INTEGER NOT NULL,
			created_op_index INTEGER NOT NULL,
			snapshot_json TEXT NOT NULL
		)`,
	}
	for _, statement := range ddl {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return domain.NewError(domain.ECkptOpen, "cannot initialize checkpoint schema").
				At(s.path).With("error", err.Error())
		}
	}
	return nil
}

// Save persists chain and snapshot rows first. The global committed operation
// sequence is updated last inside the same SQLite transaction. A process kill
// therefore leaves either the previous complete transaction or the new one.
func (s *Store) Save(ctx context.Context, state State) error {
	if state.FormatVersion == "" {
		state.FormatVersion = domain.CheckpointFormat
	}
	if state.ProjectVersion == "" {
		state.ProjectVersion = domain.ProjectModelVersion
	}
	if state.RecordedAt == "" {
		state.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.NewError(domain.ECkptWrite, "cannot begin checkpoint transaction").With("error", err.Error())
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_chains`); err != nil {
		return writeErr("clear chains", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_snapshots`); err != nil {
		return writeErr("clear snapshots", err)
	}

	for itemID, chain := range state.Chains {
		chainJSON, err := json.Marshal(chain)
		if err != nil {
			return domain.NewError(domain.ECkptWrite, "cannot encode chain").With("item_id", itemID).With("error", err.Error())
		}
		reclaimedIDs := []string{}
		for id, reclaimed := range state.Reclaimed {
			if reclaimed {
				for _, version := range chain {
					if version.ID == id {
						reclaimedIDs = append(reclaimedIDs, id)
					}
				}
			}
		}
		reclaimedJSON, err := json.Marshal(reclaimedIDs)
		if err != nil {
			return writeErr("encode reclaimed", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO checkpoint_chains(item_id, chain_json, reclaimed_ids_json, committed_seq)
			VALUES (?, ?, ?, ?)`,
			itemID, string(chainJSON), string(reclaimedJSON), state.CommittedOpSeq,
		); err != nil {
			return writeErr("insert chain", err).WithContext(map[string]any{"item_id": itemID})
		}
	}

	for _, snapshot := range state.Snapshots {
		if snapshot.Version == "" {
			snapshot.Version = StateSnapshotVersion
		}
		payload, err := json.Marshal(snapshot)
		if err != nil {
			return domain.NewError(domain.ECkptWrite, "cannot encode snapshot").With("snapshot_id", snapshot.ID)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO checkpoint_snapshots(snapshot_id, owner_txn, created_op_index, snapshot_json)
			VALUES (?, ?, ?, ?)`,
			snapshot.ID, snapshot.OwnerTxn, snapshot.CreatedOpIndex, string(payload),
		); err != nil {
			return writeErr("insert snapshot", err).With("snapshot_id", snapshot.ID)
		}
	}

	if s.FaultBeforeCommit != nil {
		if err := s.FaultBeforeCommit(); err != nil {
			return domain.NewError(domain.ECkptWrite, "injected pre-commit checkpoint fault").With("error", err.Error())
		}
	}

	// This is the deliberate final publish point for global progress.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO checkpoint_meta
			(id, format_version, project_version, source_schema, project_id, batch_id, seed, committed_op_seq, recorded_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			format_version=excluded.format_version,
			project_version=excluded.project_version,
			source_schema=excluded.source_schema,
			project_id=excluded.project_id,
			batch_id=excluded.batch_id,
			seed=excluded.seed,
			committed_op_seq=excluded.committed_op_seq,
			recorded_at=excluded.recorded_at`,
		state.FormatVersion, state.ProjectVersion, state.SourceSchema, state.ProjectID,
		state.BatchID, state.Seed, state.CommittedOpSeq, state.RecordedAt,
	); err != nil {
		return writeErr("upsert meta", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.NewError(domain.ECkptWrite, "cannot commit checkpoint transaction").With("error", err.Error())
	}
	return nil
}

func (s *Store) Load(ctx context.Context) (*State, error) {
	if err := s.integrityCheck(ctx); err != nil {
		return nil, err
	}
	state := &State{Chains: map[string][]domain.Version{}, Reclaimed: map[string]bool{}}
	var sourceSchema sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT format_version, project_version, source_schema, project_id, batch_id,
		       seed, committed_op_seq, recorded_at
		FROM checkpoint_meta WHERE id = 1`).Scan(
		&state.FormatVersion, &state.ProjectVersion, &sourceSchema, &state.ProjectID,
		&state.BatchID, &state.Seed, &state.CommittedOpSeq, &state.RecordedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, classifySQLiteError(s.path, "load meta", err)
	}
	if sourceSchema.Valid {
		state.SourceSchema = sourceSchema.String
	}
	if state.FormatVersion != domain.CheckpointFormat {
		return nil, domain.NewError(domain.ECkptVersionMismatch, "checkpoint format version does not match kernel").
			At("checkpoint_meta.format_version").
			With("got", state.FormatVersion).With("expected", domain.CheckpointFormat).
			With("committed_op_seq", state.CommittedOpSeq)
	}
	if state.ProjectVersion != domain.ProjectModelVersion {
		return nil, domain.NewError(domain.ECkptVersionMismatch, "checkpoint project version does not match canonical model").
			At("checkpoint_meta.project_version").
			With("got", state.ProjectVersion).With("expected", domain.ProjectModelVersion).
			With("committed_op_seq", state.CommittedOpSeq)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT item_id, chain_json, reclaimed_ids_json FROM checkpoint_chains ORDER BY item_id`)
	if err != nil {
		return nil, domain.NewError(domain.ECkptRead, "cannot read checkpoint chains").With("error", err.Error())
	}
	defer rows.Close()
	for rows.Next() {
		var itemID, payload, reclaimedPayload string
		if err := rows.Scan(&itemID, &payload, &reclaimedPayload); err != nil {
			return nil, domain.NewError(domain.ECkptRead, "cannot scan chain row").With("error", err.Error())
		}
		var chain []domain.Version
		if err := json.Unmarshal([]byte(payload), &chain); err != nil {
			return nil, domain.NewError(domain.ECkptTruncated, "checkpoint chain payload is truncated or invalid").
				At("checkpoint_chains.chain_json").With("item_id", itemID).With("decode_error", err.Error())
		}
		state.Chains[itemID] = chain
		var reclaimedIDs []string
		if err := json.Unmarshal([]byte(reclaimedPayload), &reclaimedIDs); err != nil {
			return nil, domain.NewError(domain.ECkptTruncated, "reclaimed id payload is truncated or invalid").
				At("checkpoint_chains.reclaimed_ids_json").With("item_id", itemID).With("decode_error", err.Error())
		}
		for _, id := range reclaimedIDs {
			state.Reclaimed[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, domain.NewError(domain.ECkptRead, "chain rows iteration failed").With("error", err.Error())
	}

	snapRows, err := s.db.QueryContext(ctx, `SELECT snapshot_id, snapshot_json FROM checkpoint_snapshots ORDER BY snapshot_id`)
	if err != nil {
		return nil, domain.NewError(domain.ECkptRead, "cannot read checkpoint snapshots").With("error", err.Error())
	}
	defer snapRows.Close()
	for snapRows.Next() {
		var id, payload string
		if err := snapRows.Scan(&id, &payload); err != nil {
			return nil, domain.NewError(domain.ECkptRead, "cannot scan snapshot row").With("error", err.Error())
		}
		var snapshot SnapshotState
		if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
			return nil, domain.NewError(domain.ECkptTruncated, "snapshot payload is truncated or invalid").
				At("checkpoint_snapshots.snapshot_json").With("snapshot_id", id).With("decode_error", err.Error())
		}
		if snapshot.Version != StateSnapshotVersion {
			return nil, domain.NewError(domain.ESnapshotVersionBad, "snapshot state version does not match kernel").
				At("checkpoint_snapshots.snapshot_json").
				With("snapshot_id", id).With("got", snapshot.Version).With("expected", StateSnapshotVersion)
		}
		state.Snapshots = append(state.Snapshots, snapshot)
	}
	if err := snapRows.Err(); err != nil {
		return nil, domain.NewError(domain.ECkptRead, "snapshot rows iteration failed").With("error", err.Error())
	}
	return state, nil
}

func (s *Store) integrityCheck(ctx context.Context) error {
	var result string
	err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result)
	if err != nil {
		return classifySQLiteError(s.path, "integrity_check", err)
	}
	if !strings.EqualFold(result, "ok") {
		return domain.NewError(domain.ECkptCorrupt, "sqlite integrity check failed").
			At(s.path).With("result", result)
	}
	return nil
}

func (s *Store) Wipe(ctx context.Context) error {
	for _, table := range []string{"checkpoint_snapshots", "checkpoint_chains", "checkpoint_meta"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return domain.NewError(domain.ECkptWrite, "cannot wipe incompatible checkpoint").
				At(table).With("error", err.Error())
		}
	}
	return nil
}

// QuarantineIncompatibleSnapshots removes diagnostic snapshot rows whose
// envelope version is not understood. Committed chains and global sequence are
// left untouched, so an injected bad snapshot cannot make the last good commit
// unavailable.
func (s *Store) QuarantineIncompatibleSnapshots(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT snapshot_id, snapshot_json FROM checkpoint_snapshots`)
	if err != nil {
		return nil, domain.NewError(domain.ECkptRead, "cannot scan snapshots for quarantine").With("error", err.Error())
	}
	type badRow struct{ id, payload string }
	bad := []badRow{}
	for rows.Next() {
		var row badRow
		if err := rows.Scan(&row.id, &row.payload); err != nil {
			rows.Close()
			return nil, domain.NewError(domain.ECkptRead, "cannot scan snapshot row").With("error", err.Error())
		}
		var header struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal([]byte(row.payload), &header); err != nil || header.Version != StateSnapshotVersion {
			bad = append(bad, row)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, domain.NewError(domain.ECkptRead, "cannot close snapshot scan").With("error", err.Error())
	}
	quarantined := []string{}
	for _, row := range bad {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoint_snapshots WHERE snapshot_id = ?`, row.id); err != nil {
			return quarantined, domain.NewError(domain.ECkptWrite, "cannot quarantine incompatible snapshot").
				With("snapshot_id", row.id).With("error", err.Error())
		}
		quarantined = append(quarantined, row.id)
	}
	return quarantined, nil
}

func (s *Store) InjectTruncatedChain(ctx context.Context, itemID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE checkpoint_chains SET chain_json = ? WHERE item_id = ?`, `{"truncated":`, itemID)
	if err != nil {
		return domain.NewError(domain.ECkptWrite, "cannot inject truncated chain").With("error", err.Error())
	}
	return nil
}

func (s *Store) InjectSnapshotVersion(ctx context.Context, snapshotID, version string) error {
	payload := fmt.Sprintf(`{"version":%q,"id":%q,"committed_set":[]}`, version, snapshotID)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO checkpoint_snapshots(snapshot_id, owner_txn, created_op_index, snapshot_json)
		VALUES (?, 0, 0, ?)
		ON CONFLICT(snapshot_id) DO UPDATE SET snapshot_json=excluded.snapshot_json`,
		snapshotID, payload)
	if err != nil {
		return domain.NewError(domain.ECkptWrite, "cannot inject incompatible snapshot").With("error", err.Error())
	}
	return nil
}

func TruncatePhysicalFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return domain.NewError(domain.ECkptTruncated, "cannot stat checkpoint for truncation").With("path", path)
	}
	if info.Size() < 128 {
		return os.Truncate(path, 0)
	}
	return os.Truncate(path, info.Size()/2)
}

func writeErr(action string, err error) *domain.Error {
	return domain.NewError(domain.ECkptWrite, "checkpoint write failed: "+action).With("error", err.Error())
}

func classifySQLiteError(path, action string, err error) error {
	message := strings.ToLower(err.Error())
	code := domain.ECkptRead
	if strings.Contains(message, "malformed") || strings.Contains(message, "not a database") || strings.Contains(message, "file is encrypted") {
		code = domain.ECkptTruncated
	}
	if action == "ping" {
		code = domain.ECkptOpen
		if strings.Contains(message, "malformed") || strings.Contains(message, "not a database") {
			code = domain.ECkptTruncated
		}
	}
	return domain.NewError(code, "sqlite checkpoint failure during "+action).
		At(path).With("error", err.Error())
}
