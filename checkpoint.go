package lightprobe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const checkpointFormat = "lp-atomic-checkpoint-v1"

type ProbeCheckpoint struct {
	ProbeID            string             `json:"probe_id"`
	IJK                [3]int             `json:"ijk"`
	Digest             string             `json:"digest"`
	ContentHash        string             `json:"content_hash"`
	RadianceFixed      [3]int64           `json:"radiance_fixed"`
	Truncations        []TruncationRecord `json:"truncations,omitempty"`
	RawSampleCount     float64            `json:"raw_sample_count"`
	SampleCount        int                `json:"sample_count"`
	SHQuantResidualMax float64            `json:"sh_quant_residual_max"`
	CountRoundingError float64            `json:"count_rounding_error"`
}

type RoomRecord struct {
	RoomID            string            `json:"room_id"`
	ProbeDigests      map[string]string `json:"probe_digests"`
	Probes            []ProbeCheckpoint `json:"probes"`
	RadianceBuffer    [][3]int64        `json:"radiance_buffer"`
	DependencySet     []string          `json:"dependency_set"`
	ContentHash       string            `json:"content_hash"`
	ResolutionVersion string            `json:"resolution_version"`
	ParamVersion      string            `json:"param_version"`
	AlgorithmVersion  string            `json:"algorithm_version"`
	CommitIndex       int               `json:"commit_index"`
}

type CheckpointSnapshot struct {
	Format           string                `json:"format"`
	Revision         int64                 `json:"revision"`
	Seed             int64                 `json:"seed"`
	AlgorithmVersion string                `json:"algorithm_version"`
	Order            []string              `json:"order"`
	Rooms            map[string]RoomRecord `json:"rooms"`
}

type CheckpointStore struct {
	path string
	mu   sync.Mutex
	snap CheckpointSnapshot
}

type Transaction struct {
	store    *CheckpointStore
	snap     CheckpointSnapshot
	finished bool
}

type StaleRoom struct {
	RoomID string `json:"room_id"`
	Reason string `json:"reason"`
}

func OpenCheckpoint(path string) (*CheckpointStore, error) {
	store := &CheckpointStore{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			store.snap = CheckpointSnapshot{
				Format: checkpointFormat, AlgorithmVersion: AlgorithmVersion,
				Rooms: map[string]RoomRecord{},
			}
			return store, nil
		}
		return nil, domainError(EDecode, path, "checkpoint_file", "read failed: %v", err)
	}
	if err := json.Unmarshal(data, &store.snap); err != nil {
		return nil, domainError(EDecode, path, "checkpoint_file", "invalid checkpoint JSON: %v", err)
	}
	if store.snap.Format != checkpointFormat {
		return nil, domainError(EDecode, path, "format", "unexpected checkpoint format %q", store.snap.Format)
	}
	if store.snap.Rooms == nil {
		store.snap.Rooms = map[string]RoomRecord{}
	}
	return store, nil
}

func (s *CheckpointStore) Path() string { return s.path }

func (s *CheckpointStore) Snapshot() CheckpointSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSnapshot(s.snap)
}

func (s *CheckpointStore) Begin(seed int64) *Transaction {
	s.mu.Lock()
	snap := cloneSnapshot(s.snap)
	if snap.Rooms == nil {
		snap.Rooms = map[string]RoomRecord{}
	}
	snap.Format = checkpointFormat
	snap.Seed = seed
	snap.AlgorithmVersion = AlgorithmVersion
	return &Transaction{store: s, snap: snap}
}

func (tx *Transaction) PutRoom(rec RoomRecord) {
	if tx.finished {
		return
	}
	if _, exists := tx.snap.Rooms[rec.RoomID]; !exists {
		rec.CommitIndex = len(tx.snap.Order)
		tx.snap.Order = append(tx.snap.Order, rec.RoomID)
	} else {
		rec.CommitIndex = tx.snap.Rooms[rec.RoomID].CommitIndex
	}
	tx.snap.Rooms[rec.RoomID] = rec
}

func (tx *Transaction) Commit() error {
	if tx.finished {
		return domainError(EState, "checkpoint-tx", "transaction", "transaction already finished")
	}
	tx.snap.Revision++
	if err := writeAtomicJSON(tx.store.path, tx.snap); err != nil {
		tx.store.mu.Unlock()
		tx.finished = true
		return err
	}
	tx.store.snap = cloneSnapshot(tx.snap)
	tx.finished = true
	tx.store.mu.Unlock()
	return nil
}

func (tx *Transaction) Rollback() {
	if tx.finished {
		return
	}
	tx.finished = true
	tx.store.mu.Unlock()
}

func (s *CheckpointStore) CommitRoom(seed int64, rec RoomRecord) error {
	tx := s.Begin(seed)
	tx.PutRoom(rec)
	return tx.Commit()
}

func writeAtomicJSON(path string, value any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return domainError(EVal, path, "checkpoint_dir", "cannot create directory: %v", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return domainError(EVal, path, "checkpoint_payload", "marshal failed: %v", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-checkpoint-*")
	if err != nil {
		return domainError(EVal, path, "checkpoint_temp", "create temp failed: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return domainError(EVal, path, "checkpoint_temp", "write failed: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return domainError(EVal, path, "checkpoint_temp", "fsync failed: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return domainError(EVal, path, "checkpoint_temp", "close failed: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return domainError(EVal, path, "checkpoint_file", "atomic rename failed: %v", err)
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

func cloneSnapshot(in CheckpointSnapshot) CheckpointSnapshot {
	b, _ := json.Marshal(in)
	var out CheckpointSnapshot
	_ = json.Unmarshal(b, &out)
	if out.Rooms == nil {
		out.Rooms = map[string]RoomRecord{}
	}
	return out
}

type roomHashPayload struct {
	RoomID            string            `json:"room_id"`
	ProbeDigests      map[string]string `json:"probe_digests"`
	Probes            []ProbeCheckpoint `json:"probes"`
	RadianceBuffer    [][3]int64        `json:"radiance_buffer"`
	DependencySet     []string          `json:"dependency_set"`
	ResolutionVersion string            `json:"resolution_version"`
	ParamVersion      string            `json:"param_version"`
	AlgorithmVersion  string            `json:"algorithm_version"`
}

func ComputeRoomHash(rec RoomRecord) string {
	payload := roomHashPayload{
		RoomID: rec.RoomID, ProbeDigests: rec.ProbeDigests, Probes: rec.Probes,
		RadianceBuffer: rec.RadianceBuffer, DependencySet: rec.DependencySet,
		ResolutionVersion: rec.ResolutionVersion, ParamVersion: rec.ParamVersion,
		AlgorithmVersion: rec.AlgorithmVersion,
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return "room-hash:" + hex.EncodeToString(sum[:])
}

func (s *CheckpointStore) VerifiedRooms(resolutionVersions map[string]string, paramVersion string) (map[string]RoomRecord, []StaleRoom) {
	s.mu.Lock()
	snap := cloneSnapshot(s.snap)
	s.mu.Unlock()

	valid := map[string]RoomRecord{}
	var stale []StaleRoom
	ids := make([]string, 0, len(snap.Rooms))
	for id := range snap.Rooms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec := snap.Rooms[id]
		switch {
		case rec.AlgorithmVersion != AlgorithmVersion:
			stale = append(stale, StaleRoom{RoomID: id, Reason: "algorithm-version-mismatch"})
		case rec.ParamVersion != paramVersion:
			stale = append(stale, StaleRoom{RoomID: id, Reason: "param-version-mismatch"})
		case resolutionVersions[id] != "" && rec.ResolutionVersion != resolutionVersions[id]:
			stale = append(stale, StaleRoom{RoomID: id, Reason: "resolution-version-mismatch"})
		case rec.ContentHash != ComputeRoomHash(rec):
			stale = append(stale, StaleRoom{RoomID: id, Reason: "content-hash-mismatch"})
		default:
			valid[id] = rec
		}
	}
	return valid, stale
}
