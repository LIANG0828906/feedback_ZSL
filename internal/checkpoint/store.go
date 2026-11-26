package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"

	"layerforge/internal/apperr"
	"layerforge/internal/model"
	"layerforge/internal/sqlitefile"
)

var ErrNoCheckpoint = errors.New("checkpoint does not exist")

const (
	LayerStateBase      = "base"
	LayerStateCommitted = "committed"
)

type LayerRecord struct {
	LayerID      string
	Digest       string
	SizeBytes    int64
	CacheKey     string
	ProducerStep int
	State        string
	Round        int
}

type IndexRecord struct {
	CacheKey     string
	LayerID      string
	Digest       string
	SizeBytes    int64
	ProducerStep int
	Round        int
}

type RoundRecord struct {
	Round           int      `json:"round"`
	BuiltLayers     []string `json:"builtLayers"`
	DeferredLayers  []string `json:"deferredLayers"`
	LatencyUsedMS   int64    `json:"latencyUsedMs"`
	LatencyBudgetMS int64    `json:"latencyBudgetMs"`
	WorkersUsed     int      `json:"workersUsed"`
	PeakMemoryBytes int64    `json:"peakMemoryBytes"`
	DeferredJSON    string   `json:"deferredJson,omitempty"`
	DegradedJSON    string   `json:"degradedJson,omitempty"`
}

type Snapshot struct {
	Version    string
	ProjectID  string
	Generation int64
	LastRound  int
	Layers     map[string]LayerRecord
	Index      map[string]IndexRecord
	Rounds     []RoundRecord
}

func NewSnapshot(projectID string) *Snapshot {
	return &Snapshot{
		Version:   model.CheckpointVersion,
		ProjectID: projectID,
		Layers:    make(map[string]LayerRecord),
		Index:     make(map[string]IndexRecord),
	}
}

func (s *Snapshot) Clone() *Snapshot {
	if s == nil {
		return nil
	}
	out := &Snapshot{
		Version:    s.Version,
		ProjectID:  s.ProjectID,
		Generation: s.Generation,
		LastRound:  s.LastRound,
		Layers:     make(map[string]LayerRecord, len(s.Layers)),
		Index:      make(map[string]IndexRecord, len(s.Index)),
		Rounds:     append([]RoundRecord(nil), s.Rounds...),
	}
	for key, value := range s.Layers {
		out.Layers[key] = value
	}
	for key, value := range s.Index {
		out.Index[key] = value
	}
	return out
}

type Store struct {
	Path string

	mu        sync.Mutex
	faultHook func(sqlitefile.CommitStage) error
}

func Open(path string) *Store {
	return &Store{Path: path}
}

func (s *Store) SetFaultHook(hook func(sqlitefile.CommitStage) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faultHook = hook
}

func (s *Store) Load() (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (*Snapshot, error) {
	if _, err := os.Stat(s.Path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCheckpoint
		}
		return nil, apperr.Wrap(apperr.CheckpointIO, "load-checkpoint", s.Path, err)
	}
	db, err := sqlitefile.Read(s.Path)
	if err != nil {
		if errors.Is(err, sqlitefile.ErrTruncated) {
			return nil, apperr.Wrap(apperr.CheckpointTruncated, "load-checkpoint", s.Path, err)
		}
		return nil, apperr.Wrap(apperr.CheckpointIntegrity, "load-checkpoint", s.Path, err)
	}
	snapshot := NewSnapshot("")
	meta, err := tableRows(db, "meta")
	if err != nil {
		return nil, err
	}
	for _, row := range meta {
		if len(row) != 2 {
			return nil, integrity(s.Path, "meta row has wrong column count")
		}
		key, keyOK := row[0].(string)
		value, valueOK := row[1].(string)
		if !keyOK || !valueOK {
			return nil, integrity(s.Path, "meta row has invalid types")
		}
		switch key {
		case "version":
			snapshot.Version = value
		case "project_id":
			snapshot.ProjectID = value
		case "generation":
			snapshot.Generation, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, integrity(s.Path, "invalid generation")
			}
		case "last_round":
			snapshot.LastRound, err = strconv.Atoi(value)
			if err != nil {
				return nil, integrity(s.Path, "invalid last_round")
			}
		case "state":
			if value != "complete" {
				return nil, integrity(s.Path, "checkpoint is not marked complete")
			}
		}
	}
	if snapshot.Version != model.CheckpointVersion {
		return nil, apperr.New(apperr.CheckpointVersion, "load-checkpoint", s.Path, fmt.Sprintf("checkpoint version %q does not match %q", snapshot.Version, model.CheckpointVersion))
	}
	if snapshot.ProjectID == "" {
		return nil, integrity(s.Path, "missing project_id")
	}
	layerRows, err := tableRows(db, "layers")
	if err != nil {
		return nil, err
	}
	for _, row := range layerRows {
		record, err := parseLayerRow(row)
		if err != nil {
			return nil, integrity(s.Path, err.Error())
		}
		if _, exists := snapshot.Layers[record.LayerID]; exists {
			return nil, integrity(s.Path, "duplicate layer row "+record.LayerID)
		}
		snapshot.Layers[record.LayerID] = record
	}
	indexRows, err := tableRows(db, "cache_index")
	if err != nil {
		return nil, err
	}
	for _, row := range indexRows {
		record, err := parseIndexRow(row)
		if err != nil {
			return nil, integrity(s.Path, err.Error())
		}
		if _, exists := snapshot.Index[record.CacheKey]; exists {
			return nil, integrity(s.Path, "duplicate cache index row "+record.CacheKey)
		}
		snapshot.Index[record.CacheKey] = record
	}
	roundRows, err := tableRows(db, "rounds")
	if err != nil {
		return nil, err
	}
	for _, row := range roundRows {
		record, err := parseRoundRow(row)
		if err != nil {
			return nil, integrity(s.Path, err.Error())
		}
		snapshot.Rounds = append(snapshot.Rounds, record)
	}
	sort.Slice(snapshot.Rounds, func(i, j int) bool { return snapshot.Rounds[i].Round < snapshot.Rounds[j].Round })
	if snapshot.LastRound != len(snapshot.Rounds) {
		return nil, integrity(s.Path, "last_round does not match persisted round count")
	}
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *Store) Commit(snapshot *Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snapshot == nil {
		return apperr.New(apperr.CheckpointIntegrity, "commit-checkpoint", s.Path, "nil snapshot")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	db := snapshotDatabase(snapshot)
	hook := s.faultHook
	if err := sqlitefile.WriteAtomic(s.Path, db, hook); err != nil {
		return apperr.Wrap(apperr.CheckpointIO, "commit-checkpoint", s.Path, err)
	}
	return nil
}

func (s *Store) Reset(projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := NewSnapshot(projectID)
	if err := s.commitLocked(snapshot); err != nil {
		return err
	}
	return nil
}

func (s *Store) CommitRound(snapshot *Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(snapshot)
}

func (s *Store) commitLocked(snapshot *Snapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	hook := s.faultHook
	if err := sqlitefile.WriteAtomic(s.Path, snapshotDatabase(snapshot), hook); err != nil {
		return apperr.Wrap(apperr.CheckpointIO, "commit-checkpoint", s.Path, err)
	}
	return nil
}

func (s *Snapshot) Validate() error {
	if s.Version != model.CheckpointVersion {
		return apperr.New(apperr.CheckpointVersion, "validate-checkpoint", "version", "checkpoint version mismatch")
	}
	if s.ProjectID == "" {
		return apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "project_id", "project id is required")
	}
	if s.LastRound != len(s.Rounds) {
		return apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "rounds", "last_round must equal number of complete rounds")
	}
	for layerID, layer := range s.Layers {
		if layerID == "" || layer.Digest == "" || layer.SizeBytes <= 0 {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "layers", "layer record is incomplete"), layerID)
		}
		if layer.State != LayerStateBase && layer.State != LayerStateCommitted {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "layers.state", "layer is not committed"), layerID)
		}
		if layer.State == LayerStateBase && layer.ProducerStep != 0 {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "layers.state", "base layer has a producer step"), layerID)
		}
		if layer.State == LayerStateBase && layer.CacheKey != "" {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "layers.cache_key", "base layer must not have a cache key"), layerID)
		}
		if layer.State == LayerStateCommitted && (layer.ProducerStep <= 0 || layer.CacheKey == "") {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "layers.state", "committed layer requires producer and cache key"), layerID)
		}
		if layer.Round < 0 || layer.Round > s.LastRound {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "layers.round", "layer references an incomplete round"), layerID)
		}
	}
	for cacheKey, entry := range s.Index {
		if cacheKey == "" || entry.CacheKey != cacheKey {
			return apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "cache_index", "index key is inconsistent")
		}
		layer, ok := s.Layers[entry.LayerID]
		if !ok {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "cache_index.layer", "cache index points to a missing layer"), entry.LayerID)
		}
		if layer.State != LayerStateCommitted {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "cache_index.layer", "cache index points to an uncommitted layer"), entry.LayerID)
		}
		if layer.Digest != entry.Digest || layer.SizeBytes != entry.SizeBytes || layer.ProducerStep != entry.ProducerStep {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "cache_index.layer", "cache index metadata does not match layer state"), entry.LayerID)
		}
	}
	for layerID, layer := range s.Layers {
		if layer.State != LayerStateCommitted {
			continue
		}
		entry, ok := s.Index[layer.CacheKey]
		if !ok || entry.LayerID != layerID {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "cache_index", "committed layer is missing its cache index entry"), layerID)
		}
	}
	for i, round := range s.Rounds {
		if round.Round != i+1 {
			return apperr.New(apperr.CheckpointIntegrity, "validate-checkpoint", "rounds", "round sequence is not contiguous")
		}
	}
	return nil
}

func snapshotDatabase(snapshot *Snapshot) sqlitefile.Database {
	meta := sqlitefile.Table{
		Name:    "meta",
		Columns: []string{"key", "value"},
		Rows: [][]sqlitefile.Value{
			{"version", snapshot.Version},
			{"project_id", snapshot.ProjectID},
			{"generation", strconv.FormatInt(snapshot.Generation, 10)},
			{"last_round", strconv.Itoa(snapshot.LastRound)},
			{"state", "complete"},
		},
	}
	layerIDs := make([]string, 0, len(snapshot.Layers))
	for layerID := range snapshot.Layers {
		layerIDs = append(layerIDs, layerID)
	}
	sort.Strings(layerIDs)
	layers := sqlitefile.Table{
		Name:    "layers",
		Columns: []string{"layer_id", "digest", "size_bytes", "cache_key", "producer_step", "state", "round_no"},
	}
	for _, layerID := range layerIDs {
		layer := snapshot.Layers[layerID]
		layers.Rows = append(layers.Rows, []sqlitefile.Value{
			layer.LayerID,
			layer.Digest,
			layer.SizeBytes,
			layer.CacheKey,
			int64(layer.ProducerStep),
			layer.State,
			int64(layer.Round),
		})
	}
	keys := make([]string, 0, len(snapshot.Index))
	for cacheKey := range snapshot.Index {
		keys = append(keys, cacheKey)
	}
	sort.Strings(keys)
	index := sqlitefile.Table{
		Name:    "cache_index",
		Columns: []string{"cache_key", "layer_id", "digest", "size_bytes", "producer_step", "round_no"},
	}
	for _, cacheKey := range keys {
		entry := snapshot.Index[cacheKey]
		index.Rows = append(index.Rows, []sqlitefile.Value{
			entry.CacheKey,
			entry.LayerID,
			entry.Digest,
			entry.SizeBytes,
			int64(entry.ProducerStep),
			int64(entry.Round),
		})
	}
	rounds := sqlitefile.Table{
		Name:    "rounds",
		Columns: []string{"round_no", "built_layers", "deferred_layers", "latency_used_ms", "latency_budget_ms", "workers_used", "peak_memory_bytes", "deferred_json", "degraded_json"},
	}
	for _, round := range snapshot.Rounds {
		built, _ := json.Marshal(round.BuiltLayers)
		deferred, _ := json.Marshal(round.DeferredLayers)
		rounds.Rows = append(rounds.Rows, []sqlitefile.Value{
			int64(round.Round),
			string(built),
			string(deferred),
			round.LatencyUsedMS,
			round.LatencyBudgetMS,
			int64(round.WorkersUsed),
			round.PeakMemoryBytes,
			round.DeferredJSON,
			round.DegradedJSON,
		})
	}
	return sqlitefile.Database{Tables: []sqlitefile.Table{meta, layers, index, rounds}}
}

func tableRows(db *sqlitefile.Database, name string) ([][]sqlitefile.Value, error) {
	for _, table := range db.Tables {
		if table.Name == name {
			return table.Rows, nil
		}
	}
	return nil, apperr.New(apperr.CheckpointIntegrity, "load-checkpoint", name, "required SQLite table is missing")
}

func parseLayerRow(row []sqlitefile.Value) (LayerRecord, error) {
	if len(row) != 7 {
		return LayerRecord{}, fmt.Errorf("layers row has wrong column count")
	}
	layerID, ok1 := row[0].(string)
	digest, ok2 := row[1].(string)
	size, ok3 := row[2].(int64)
	cacheKey, ok4 := row[3].(string)
	producer, ok5 := row[4].(int64)
	state, ok6 := row[5].(string)
	round, ok7 := row[6].(int64)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 {
		return LayerRecord{}, fmt.Errorf("layers row has invalid types")
	}
	return LayerRecord{
		LayerID:      layerID,
		Digest:       digest,
		SizeBytes:    size,
		CacheKey:     cacheKey,
		ProducerStep: int(producer),
		State:        state,
		Round:        int(round),
	}, nil
}

func parseIndexRow(row []sqlitefile.Value) (IndexRecord, error) {
	if len(row) != 6 {
		return IndexRecord{}, fmt.Errorf("cache_index row has wrong column count")
	}
	cacheKey, ok1 := row[0].(string)
	layerID, ok2 := row[1].(string)
	digest, ok3 := row[2].(string)
	size, ok4 := row[3].(int64)
	producer, ok5 := row[4].(int64)
	round, ok6 := row[5].(int64)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return IndexRecord{}, fmt.Errorf("cache_index row has invalid types")
	}
	return IndexRecord{
		CacheKey:     cacheKey,
		LayerID:      layerID,
		Digest:       digest,
		SizeBytes:    size,
		ProducerStep: int(producer),
		Round:        int(round),
	}, nil
}

func parseRoundRow(row []sqlitefile.Value) (RoundRecord, error) {
	if len(row) != 9 {
		return RoundRecord{}, fmt.Errorf("rounds row has wrong column count")
	}
	round, ok1 := row[0].(int64)
	builtJSON, ok2 := row[1].(string)
	deferredJSON, ok3 := row[2].(string)
	used, ok4 := row[3].(int64)
	budget, ok5 := row[4].(int64)
	workers, ok6 := row[5].(int64)
	peak, ok7 := row[6].(int64)
	deferredDetail, ok8 := row[7].(string)
	degraded, ok9 := row[8].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 || !ok8 || !ok9 {
		return RoundRecord{}, fmt.Errorf("rounds row has invalid types")
	}
	record := RoundRecord{
		Round:           int(round),
		LatencyUsedMS:   used,
		LatencyBudgetMS: budget,
		WorkersUsed:     int(workers),
		PeakMemoryBytes: peak,
		DeferredJSON:    deferredDetail,
		DegradedJSON:    degraded,
	}
	if err := json.Unmarshal([]byte(builtJSON), &record.BuiltLayers); err != nil {
		return RoundRecord{}, fmt.Errorf("round built_layers is invalid JSON")
	}
	if err := json.Unmarshal([]byte(deferredJSON), &record.DeferredLayers); err != nil {
		return RoundRecord{}, fmt.Errorf("round deferred_layers is invalid JSON")
	}
	return record, nil
}

func integrity(path, detail string) error {
	return apperr.New(apperr.CheckpointIntegrity, "load-checkpoint", path, detail)
}
