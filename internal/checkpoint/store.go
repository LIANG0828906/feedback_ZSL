// Package checkpoint persists atomic batch commits in a pure-Go SQLite file.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"wardrobekernel/internal/atomicfile"
	"wardrobekernel/internal/model"
	"wardrobekernel/internal/sqlitefile"
	"wardrobekernel/internal/xerr"
)

// PersistedBatch contains every field required at a recovery boundary.
type PersistedBatch struct {
	model.Batch
	Commit model.CommitRecord `json:"commit"`
}

// Store is a mutex-serialized SQLite checkpoint file.
type Store struct {
	path              string
	algorithmVersion  string
	constraintVersion string

	mu      sync.Mutex
	records []PersistedBatch
}

// Open reads a checkpoint and verifies global/per-batch versions.
func Open(path, algorithmVersion, constraintVersion string) (*Store, error) {
	store := &Store{path: path, algorithmVersion: algorithmVersion, constraintVersion: constraintVersion}
	rows, err := sqlitefile.ReadKVFile(path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, xerr.Wrap(xerr.EIO, "read SQLite checkpoint: "+path, err)
	}

	values := map[string]string{}
	for _, row := range rows {
		values[row.Key] = row.Value
	}
	persistedAlgo := values["meta.algorithm_version"]
	if persistedAlgo != "" && persistedAlgo != algorithmVersion {
		return nil, &xerr.Error{
			Code: xerr.EVer, Message: "checkpoint algorithm version mismatch; recompute from affected batches",
			ExpectedVersion: algorithmVersion, PersistedVersion: persistedAlgo,
			AffectedBatches: allPersistedBatchNumbers(rows),
		}
	}

	for _, row := range rows {
		if len(row.Key) < 6 || row.Key[:6] != "batch:" {
			continue
		}
		var rec PersistedBatch
		if err := json.Unmarshal([]byte(row.Value), &rec); err != nil {
			return nil, xerr.Wrap(xerr.EState, "decode checkpoint row "+row.Key, err)
		}
		if rec.AlgorithmVersion != "" && rec.AlgorithmVersion != algorithmVersion {
			return nil, &xerr.Error{
				Code: xerr.EVer, Message: "persisted batch algorithm version mismatch",
				ExpectedVersion: algorithmVersion, PersistedVersion: rec.AlgorithmVersion,
				AffectedBatches: []int{rec.Number},
			}
		}
		if constraintVersion != "" && rec.ConstraintVersion != "" && rec.ConstraintVersion != constraintVersion {
			return nil, &xerr.Error{
				Code: xerr.EVer, Message: "persisted batch constraint version mismatch; recompute affected batches",
				ExpectedVersion: constraintVersion, PersistedVersion: rec.ConstraintVersion,
				AffectedBatches: []int{rec.Number},
			}
		}
		store.records = append(store.records, rec)
	}
	sort.SliceStable(store.records, func(i, j int) bool {
		if store.records[i].Commit.Sequence != store.records[j].Commit.Sequence {
			return store.records[i].Commit.Sequence < store.records[j].Commit.Sequence
		}
		return store.records[i].Number < store.records[j].Number
	})
	return store, nil
}

func allPersistedBatchNumbers(rows []sqlitefile.Row) []int {
	out := []int{}
	for _, row := range rows {
		var rec PersistedBatch
		if err := json.Unmarshal([]byte(row.Value), &rec); err == nil && rec.Number > 0 {
			out = append(out, rec.Number)
		}
	}
	sort.Ints(out)
	return out
}

// Update runs fn in one serialized checkpoint transaction and flushes once.
func (s *Store) Update(fn func(tx *Transaction) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	startLen := len(s.records)
	tx := &Transaction{store: s}
	if err := fn(tx); err != nil {
		s.records = s.records[:startLen]
		return err
	}
	if err := s.flushLocked(); err != nil {
		s.records = s.records[:startLen]
		return err
	}
	return nil
}

// Transaction stages batch commits for one atomic SQLite snapshot replace.
type Transaction struct {
	store *Store
}

// CommitBatch appends one batch row with a stable sequence number.
func (tx *Transaction) CommitBatch(batch model.Batch) (model.CommitRecord, error) {
	sequence := len(tx.store.records) + 1
	commit := model.CommitRecord{
		Sequence: sequence, BatchNumber: batch.Number, CacheKey: batch.CacheKey,
		ContentHash: batch.ContentHash, ConstraintVersion: batch.ConstraintVersion,
		AlgorithmVersion: batch.AlgorithmVersion, CommittedAtNano: time.Now().UnixNano(),
	}
	rec := PersistedBatch{Batch: batch, Commit: commit}
	tx.store.records = append(tx.store.records, rec)
	return commit, nil
}

// CommitBatch is the one-batch transaction convenience API.
func (s *Store) CommitBatch(batch model.Batch) (model.CommitRecord, error) {
	var commit model.CommitRecord
	err := s.Update(func(tx *Transaction) error {
		var err error
		commit, err = tx.CommitBatch(batch)
		return err
	})
	return commit, err
}

func (s *Store) flushLocked() error {
	rows := []sqlitefile.Row{
		{Key: "meta.schema", Value: "wardrobe-checkpoint-sqlite-v1"},
		{Key: "meta.algorithm_version", Value: s.algorithmVersion},
		{Key: "meta.constraint_version", Value: s.constraintVersion},
	}
	for _, rec := range s.records {
		data, err := json.Marshal(rec)
		if err != nil {
			return xerr.Wrap(xerr.EState, "marshal checkpoint batch", err)
		}
		rows = append(rows, sqlitefile.Row{
			Key:   fmt.Sprintf("batch:%06d", rec.Commit.Sequence),
			Value: string(data),
		})
	}
	db, err := sqlitefile.BuildKVDB(rows)
	if err != nil {
		return xerr.Wrap(xerr.EState, "build SQLite checkpoint pages", err)
	}
	if err := atomicfile.WriteBytes(s.path, db); err != nil {
		return xerr.Wrap(xerr.EIO, "atomic commit SQLite checkpoint", err)
	}
	return nil
}

// Batches returns persisted batches in commit order.
func (s *Store) Batches() []model.Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Batch, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, rec.Batch)
	}
	return out
}

// Records returns full persisted records.
func (s *Store) Records() []PersistedBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PersistedBatch(nil), s.records...)
}

// LastSequence returns the highest durable commit sequence.
func (s *Store) LastSequence() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// Reset deletes the checkpoint file and clears in-memory rows.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = nil
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
