package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"

	"wardrobe/domain"
)

const (
	StatusBuilding = "building"
	StatusReady    = "ready"
)

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
	path    string
}

type LoadResult struct {
	Found       bool   `json:"found"`
	Recovered   bool   `json:"recovered"`
	Invalidated bool   `json:"invalidated"`
	Reason      string `json:"reason,omitempty"`
	Result      *domain.CaseResult
}

type budgetRecord struct {
	VertexUsed    int      `json:"vertexUsed"`
	IndexUsed     int      `json:"indexUsed"`
	MaterialSlots []string `json:"materialSlots"`
}

type batchRow struct {
	BatchID           string
	CommitSequence    int
	PartIDsJSON       string
	PlacementsJSON    string
	BudgetJSON        string
	DependenciesJSON  string
	ContentHash       string
	ConstraintVersion string
	AlgorithmVersion  string
	CacheKey          string
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite path is empty")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		directory := filepath.Dir(path)
		if directory != "." {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				return nil, fmt.Errorf("create sqlite directory: %w", err)
			}
		}
	}
	dsn := path
	if dsn == ":memory:" {
		dsn = "file:wardrobe-memory?mode=memory&cache=shared"
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	dsn += separator + "_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on&_synchronous=FULL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_sets (
			project_id TEXT NOT NULL,
			case_id TEXT NOT NULL,
			input_hash TEXT NOT NULL,
			constraint_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			expected_batches INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('building','ready')),
			result_json TEXT,
			PRIMARY KEY (project_id, case_id)
		)`,
		`CREATE TABLE IF NOT EXISTS batch_snapshots (
			project_id TEXT NOT NULL,
			case_id TEXT NOT NULL,
			batch_id TEXT NOT NULL,
			commit_sequence INTEGER NOT NULL,
			part_ids_json TEXT NOT NULL,
			placements_json TEXT NOT NULL,
			budget_json TEXT NOT NULL,
			dependencies_json TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			constraint_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			PRIMARY KEY (project_id, case_id, batch_id),
			UNIQUE (project_id, case_id, commit_sequence)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_batch_snapshot_order
			ON batch_snapshots(project_id, case_id, commit_sequence)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	return nil
}

// SaveCase writes one batch per transaction. The final batch transaction also
// flips the set to ready, so a crash can only leave a visibly incomplete set.
func (s *Store) SaveCase(ctx context.Context, project domain.Project, result domain.CaseResult) error {
	if result.InputHash == "" {
		return errors.New("save checkpoint: empty input hash")
	}
	if len(result.Batches) != len(result.CommitSequence) {
		return errors.New("save checkpoint: batch/commit count mismatch")
	}
	for index, batch := range result.Batches {
		if batch.CommitSequence != index+1 {
			return fmt.Errorf("save checkpoint: batch %s commit sequence %d, expected %d", batch.ID, batch.CommitSequence, index+1)
		}
		if batch.ID != result.CommitSequence[index] {
			return fmt.Errorf("save checkpoint: commit sequence[%d]=%s, batch=%s", index, result.CommitSequence[index], batch.ID)
		}
	}
	storedResult := result
	storedResult.Source = ""
	storedResult.ReusedBatchIDs = nil
	storedResult.InvalidatedBatchIDs = nil
	storedResult.Schedule = nil
	storedResult.Batches = append([]domain.BatchResult(nil), result.Batches...)
	for index := range storedResult.Batches {
		storedResult.Batches[index].Source = ""
	}
	resultJSON, err := json.Marshal(storedResult)
	if err != nil {
		return fmt.Errorf("marshal checkpoint result: %w", err)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin checkpoint set: %w", err)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM batch_snapshots WHERE project_id=? AND case_id=?`, project.ProjectID, result.CaseID)
	if err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM checkpoint_sets WHERE project_id=? AND case_id=?`, project.ProjectID, result.CaseID)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO checkpoint_sets
			(project_id, case_id, input_hash, constraint_version, algorithm_version, expected_batches, status, result_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
			project.ProjectID, result.CaseID, result.InputHash, project.Constraints.Version, domain.AlgorithmVersion, len(result.Batches), StatusBuilding)
	}
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("initialize checkpoint set: %w", err)
	}
	if len(result.Batches) == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE checkpoint_sets SET status=?, result_json=? WHERE project_id=? AND case_id=?`,
			StatusReady, string(resultJSON), project.ProjectID, result.CaseID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("finalize empty checkpoint: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit checkpoint set header: %w", err)
	}
	if len(result.Batches) == 0 {
		return nil
	}

	for index, batch := range result.Batches {
		batchJSON, err := marshalBatch(batch)
		if err != nil {
			return err
		}
		last := index == len(result.Batches)-1
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin batch %s transaction: %w", batch.ID, err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO batch_snapshots
			(project_id, case_id, batch_id, commit_sequence, part_ids_json, placements_json,
			 budget_json, dependencies_json, content_hash, constraint_version, algorithm_version, cache_key)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			project.ProjectID, result.CaseID, batch.ID, batch.CommitSequence,
			batchJSON.PartIDsJSON, batchJSON.PlacementsJSON, batchJSON.BudgetJSON,
			batchJSON.DependenciesJSON, ContentHash(batch), project.Constraints.Version,
			domain.AlgorithmVersion, batch.CacheKey)
		if err == nil && last {
			_, err = tx.ExecContext(ctx, `UPDATE checkpoint_sets SET status=?, result_json=?
				WHERE project_id=? AND case_id=? AND expected_batches=?`,
				StatusReady, string(resultJSON), project.ProjectID, result.CaseID, len(result.Batches))
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("commit batch %s snapshot: %w", batch.ID, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit batch %s transaction: %w", batch.ID, err)
		}
	}
	return nil
}

func (s *Store) LoadCase(
	ctx context.Context,
	projectID string,
	caseID string,
	inputHash string,
	constraintVersion string,
	algorithmVersion string,
) (LoadResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var (
		storedInputHash  string
		storedConstraint string
		storedAlgorithm  string
		expectedBatches  int
		status           string
		resultJSON       sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `SELECT input_hash, constraint_version, algorithm_version,
		expected_batches, status, result_json FROM checkpoint_sets WHERE project_id=? AND case_id=?`,
		projectID, caseID).Scan(&storedInputHash, &storedConstraint, &storedAlgorithm, &expectedBatches, &status, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return LoadResult{Found: false}, nil
	}
	if err != nil {
		return LoadResult{}, fmt.Errorf("read checkpoint set: %w", err)
	}
	invalidate := func(reason string) (LoadResult, error) {
		if err := s.invalidateLocked(ctx, projectID, caseID); err != nil {
			return LoadResult{}, err
		}
		return LoadResult{Found: true, Invalidated: true, Reason: reason}, nil
	}
	if status != StatusReady {
		return invalidate("checkpoint status is " + status + " (incomplete or interrupted)")
	}
	if storedInputHash != inputHash {
		return invalidate("input content hash changed")
	}
	if storedConstraint != constraintVersion {
		return invalidate("constraint version changed")
	}
	if storedAlgorithm != algorithmVersion || algorithmVersion != domain.AlgorithmVersion {
		return invalidate("algorithm version changed")
	}
	if !resultJSON.Valid || resultJSON.String == "" {
		return invalidate("ready checkpoint has no result payload")
	}
	rows, err := s.readRowsLocked(ctx, projectID, caseID)
	if err != nil {
		return LoadResult{}, err
	}
	if len(rows) != expectedBatches {
		return invalidate(fmt.Sprintf("batch count %d does not match expected %d", len(rows), expectedBatches))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CommitSequence < rows[j].CommitSequence })
	for index, row := range rows {
		if row.CommitSequence != index+1 {
			return invalidate(fmt.Sprintf("commit sequence gap at %d", index+1))
		}
		if row.ConstraintVersion != constraintVersion {
			return invalidate("batch constraint version changed: " + row.BatchID)
		}
		if row.AlgorithmVersion != algorithmVersion {
			return invalidate("batch algorithm version changed: " + row.BatchID)
		}
		reconstructed, err := reconstructBatch(row)
		if err != nil {
			return invalidate("batch payload corrupt: " + row.BatchID + ": " + err.Error())
		}
		if ContentHash(reconstructed) != row.ContentHash {
			return invalidate("batch content hash mismatch: " + row.BatchID)
		}
	}
	var result domain.CaseResult
	if err := json.Unmarshal([]byte(resultJSON.String), &result); err != nil {
		return invalidate("result payload corrupt: " + err.Error())
	}
	if result.InputHash != inputHash || result.ConstraintVersion != constraintVersion || result.AlgorithmVersion != algorithmVersion {
		return invalidate("result metadata versions changed")
	}
	if len(result.Batches) != len(rows) || len(result.CommitSequence) != len(rows) {
		return invalidate("result batch count changed")
	}
	for index, row := range rows {
		reconstructed, err := reconstructBatch(row)
		if err != nil {
			return invalidate("batch payload corrupt: " + row.BatchID + ": " + err.Error())
		}
		if !reflect.DeepEqual(reconstructed, result.Batches[index]) {
			return invalidate("batch row differs from result payload: " + row.BatchID)
		}
	}
	result.Source = "sqlite_recovery"
	return LoadResult{Found: true, Recovered: true, Result: &result}, nil
}

func (s *Store) invalidateLocked(ctx context.Context, projectID, caseID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin checkpoint invalidation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM batch_snapshots WHERE project_id=? AND case_id=?`, projectID, caseID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete invalid batch snapshots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_sets WHERE project_id=? AND case_id=?`, projectID, caseID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete invalid checkpoint set: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit checkpoint invalidation: %w", err)
	}
	return nil
}

func (s *Store) readRowsLocked(ctx context.Context, projectID, caseID string) ([]batchRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT batch_id, commit_sequence, part_ids_json, placements_json,
		budget_json, dependencies_json, content_hash, constraint_version, algorithm_version, cache_key
		FROM batch_snapshots WHERE project_id=? AND case_id=? ORDER BY commit_sequence`, projectID, caseID)
	if err != nil {
		return nil, fmt.Errorf("read batch snapshots: %w", err)
	}
	defer rows.Close()
	out := make([]batchRow, 0)
	for rows.Next() {
		var row batchRow
		if err := rows.Scan(&row.BatchID, &row.CommitSequence, &row.PartIDsJSON, &row.PlacementsJSON,
			&row.BudgetJSON, &row.DependenciesJSON, &row.ContentHash, &row.ConstraintVersion,
			&row.AlgorithmVersion, &row.CacheKey); err != nil {
			return nil, fmt.Errorf("scan batch snapshot: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate batch snapshots: %w", err)
	}
	return out, nil
}

func reconstructBatch(row batchRow) (domain.BatchResult, error) {
	var partIDs []string
	var placements []domain.PartPlacement
	var budget budgetRecord
	var dependencies []string
	if err := json.Unmarshal([]byte(row.PartIDsJSON), &partIDs); err != nil {
		return domain.BatchResult{}, fmt.Errorf("partIds: %w", err)
	}
	if err := json.Unmarshal([]byte(row.PlacementsJSON), &placements); err != nil {
		return domain.BatchResult{}, fmt.Errorf("placements: %w", err)
	}
	if err := json.Unmarshal([]byte(row.BudgetJSON), &budget); err != nil {
		return domain.BatchResult{}, fmt.Errorf("budget: %w", err)
	}
	if err := json.Unmarshal([]byte(row.DependenciesJSON), &dependencies); err != nil {
		return domain.BatchResult{}, fmt.Errorf("dependencies: %w", err)
	}
	return domain.BatchResult{
		ID:             row.BatchID,
		Ordinal:        row.CommitSequence,
		CommitSequence: row.CommitSequence,
		PartIDs:        partIDs,
		Parts:          placements,
		MaterialSlots:  budget.MaterialSlots,
		VertexUsed:     budget.VertexUsed,
		IndexUsed:      budget.IndexUsed,
		Dependencies:   dependencies,
		CacheKey:       row.CacheKey,
	}, nil
}

func marshalBatch(batch domain.BatchResult) (batchRow, error) {
	partIDs, err := json.Marshal(nonNil(batch.PartIDs))
	if err != nil {
		return batchRow{}, err
	}
	placements, err := json.Marshal(nonNilPlacements(batch.Parts))
	if err != nil {
		return batchRow{}, err
	}
	budget, err := json.Marshal(budgetRecord{
		VertexUsed:    batch.VertexUsed,
		IndexUsed:     batch.IndexUsed,
		MaterialSlots: append([]string(nil), batch.MaterialSlots...),
	})
	if err != nil {
		return batchRow{}, err
	}
	dependencies, err := json.Marshal(nonNil(batch.Dependencies))
	if err != nil {
		return batchRow{}, err
	}
	return batchRow{
		PartIDsJSON:      string(partIDs),
		PlacementsJSON:   string(placements),
		BudgetJSON:       string(budget),
		DependenciesJSON: string(dependencies),
	}, nil
}

func ContentHash(batch domain.BatchResult) string {
	payload := struct {
		ID             string                 `json:"id"`
		CommitSequence int                    `json:"commitSequence"`
		PartIDs        []string               `json:"partIds"`
		Parts          []domain.PartPlacement `json:"parts"`
		MaterialSlots  []string               `json:"materialSlots"`
		VertexUsed     int                    `json:"vertexUsed"`
		IndexUsed      int                    `json:"indexUsed"`
	}{
		ID:             batch.ID,
		CommitSequence: batch.CommitSequence,
		PartIDs:        batch.PartIDs,
		Parts:          batch.Parts,
		MaterialSlots:  batch.MaterialSlots,
		VertexUsed:     batch.VertexUsed,
		IndexUsed:      batch.IndexUsed,
	}
	return domain.HashJSON(payload)
}

func (s *Store) DebugSetStatus(ctx context.Context, projectID, caseID, status string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE checkpoint_sets SET status=? WHERE project_id=? AND case_id=?`, status, projectID, caseID)
	return err
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nonNilPlacements(values []domain.PartPlacement) []domain.PartPlacement {
	if values == nil {
		return []domain.PartPlacement{}
	}
	return values
}
