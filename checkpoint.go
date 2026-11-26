package maskqa

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const (
	CheckpointFormatVersion = "stroke-checkpoint-v1"
	RecoveryRecovered       = "recovered_last_committed"
	RecoveryRebuild         = "full_rebuild"
	RecoveryEmpty           = "empty_initialized"
)

// StrokeCommitRecord is one atomic stroke-on-tile checkpoint.
type StrokeCommitRecord struct {
	FormatVersion     string `json:"format_version"`
	Sequence          int64  `json:"sequence"`
	ProjectID         string `json:"project_id"`
	StrokeID          string `json:"stroke_id"`
	StrokeSeq         int    `json:"stroke_seq"`
	OrderIndex        int    `json:"order_index"`
	TileX             int    `json:"tile_x"`
	TileY             int    `json:"tile_y"`
	Component         []byte `json:"component_base64"`
	TileAfter         []byte `json:"tile_after_base64"`
	CacheKey          string `json:"cache_key"`
	TileCacheKey      string `json:"tile_cache_key"`
	ContentHash       string `json:"content_hash"`
	InputHash         string `json:"input_hash"`
	ResidualDigest    string `json:"residual_digest"`
	OrderVersion      string `json:"order_version"`
	ConversionVersion string `json:"conversion_version"`
	AlgorithmVersion  string `json:"algorithm_version"`
}

// RecoveryReport describes restart recovery or mandatory rebuild.
type RecoveryReport struct {
	Action           string `json:"action"`
	Code             string `json:"code,omitempty"`
	Reason           string `json:"reason,omitempty"`
	LastSequence     int64  `json:"last_sequence"`
	RecoveredCommits int    `json:"recovered_commits"`
	RecoveredTiles   int    `json:"recovered_tiles"`
	IgnoredTornTemp  int    `json:"ignored_torn_temp"`
}

// CheckpointStore is a serial, atomic local journal.
type CheckpointStore struct {
	dir        string
	commitsDir string
	lockFile   *os.File
	mu         sync.Mutex
	sequence   int64
	records    []StrokeCommitRecord
	latest     map[TileCoord]StrokeCommitRecord
	recovery   RecoveryReport
}

// OpenCheckpointStore opens or rebuilds the checkpoint directory.
func OpenCheckpointStore(dir string) (*CheckpointStore, RecoveryReport, error) {
	if dir == "" {
		return nil, RecoveryReport{}, decodeError("checkpoint_dir", "empty checkpoint directory")
	}
	commitsDir := filepath.Join(dir, "commits")
	if err := os.MkdirAll(commitsDir, 0o755); err != nil {
		return nil, RecoveryReport{}, err
	}
	lockFile, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, RecoveryReport{}, err
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		lockFile.Close()
		return nil, RecoveryReport{}, err
	}
	store := &CheckpointStore{
		dir: dir, commitsDir: commitsDir, lockFile: lockFile,
		latest: map[TileCoord]StrokeCommitRecord{},
	}
	report, err := store.loadLocked()
	if err != nil {
		_ = lockFile.Close()
		return nil, report, err
	}
	store.recovery = report
	return store, report, nil
}

// Close releases the file lock.
func (s *CheckpointStore) Close() error {
	if s == nil || s.lockFile == nil {
		return nil
	}
	_ = syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
	return s.lockFile.Close()
}

// Recovery returns the open-time recovery report.
func (s *CheckpointStore) Recovery() RecoveryReport { return s.recovery }

// LastSequence returns the highest committed journal sequence.
func (s *CheckpointStore) LastSequence() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sequence
}

// Records returns a copy of committed records in stable order.
func (s *CheckpointStore) Records() []StrokeCommitRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StrokeCommitRecord(nil), s.records...)
}

// LatestTile returns the last committed record for a tile.
func (s *CheckpointStore) LatestTile(coord TileCoord) (StrokeCommitRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.latest[coord]
	return record, ok
}

// CommitStroke atomically and serially persists one stroke-tile component.
func (s *CheckpointStore) CommitStroke(record StrokeCommitRecord) (StrokeCommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence++
	record.Sequence = s.sequence
	if record.FormatVersion == "" {
		record.FormatVersion = CheckpointFormatVersion
	}
	if record.ContentHash == "" {
		record.ContentHash = ContentDigest(record.Component)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		s.sequence--
		return record, err
	}
	path := filepath.Join(s.commitsDir, fmt.Sprintf("%012d.json", record.Sequence))
	if err := atomicWriteJSON(path, data); err != nil {
		s.sequence--
		return record, err
	}
	head := map[string]any{
		"last_sequence": record.Sequence,
		"project_id":    record.ProjectID,
		"updated_by":    AlgorithmVersion,
	}
	headData, err := json.MarshalIndent(head, "", "  ")
	if err == nil {
		_ = atomicWriteJSON(filepath.Join(s.dir, "HEAD.json"), headData)
	}
	s.records = append(s.records, record)
	s.latest[TileCoord{TX: record.TileX, TY: record.TileY}] = record
	return record, nil
}

func atomicWriteJSON(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".commit-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

func (s *CheckpointStore) loadLocked() (RecoveryReport, error) {
	entries, err := os.ReadDir(s.commitsDir)
	if err != nil {
		return RecoveryReport{}, err
	}
	var names []string
	tornTemp := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".json") {
			names = append(names, name)
		} else if strings.HasPrefix(name, ".commit-") && strings.HasSuffix(name, ".tmp") {
			tornTemp++
		}
	}
	sort.Strings(names)
	report := RecoveryReport{IgnoredTornTemp: tornTemp}
	expected := int64(0)
	for _, name := range names {
		expected++
		if !commitNameMatchesSequence(name, expected) {
			report = RecoveryReport{
				Action: RecoveryRebuild, Code: "E_CKPT_SEQUENCE",
				Reason: fmt.Sprintf("non-contiguous checkpoint file %s at expected sequence %d", name, expected),
			}
			s.resetFiles()
			return report, nil
		}
		data, err := os.ReadFile(filepath.Join(s.commitsDir, name))
		if err != nil {
			report.Action = RecoveryRebuild
			report.Code = "E_CKPT_READ"
			report.Reason = err.Error()
			s.resetFiles()
			return report, nil
		}
		var record StrokeCommitRecord
		if err := json.Unmarshal(data, &record); err != nil {
			report.Action = RecoveryRebuild
			report.Code = CodeDecode
			report.Reason = "checkpoint JSON decode failed: " + err.Error()
			s.resetFiles()
			return report, nil
		}
		if mismatch := verifyRecord(&record); mismatch != "" {
			report.Action = RecoveryRebuild
			report.Code = "E_CKPT_VERSION_HASH"
			report.Reason = mismatch
			s.resetFiles()
			return report, nil
		}
		s.records = append(s.records, record)
		s.latest[TileCoord{TX: record.TileX, TY: record.TileY}] = record
		s.sequence = record.Sequence
	}
	if s.sequence == 0 {
		report.Action = RecoveryEmpty
	} else {
		report.Action = RecoveryRecovered
	}
	report.LastSequence = s.sequence
	report.RecoveredCommits = len(s.records)
	report.RecoveredTiles = len(s.latest)
	if tornTemp > 0 {
		report.Reason = fmt.Sprintf("ignored %d torn temporary write(s); last committed sequence is authoritative", tornTemp)
	}
	return report, nil
}

func verifyRecord(record *StrokeCommitRecord) string {
	if record.FormatVersion != CheckpointFormatVersion {
		return fmt.Sprintf("format version %q != %q", record.FormatVersion, CheckpointFormatVersion)
	}
	if record.OrderVersion != OrderVersion {
		return fmt.Sprintf("order version %q != %q", record.OrderVersion, OrderVersion)
	}
	if record.ConversionVersion != ConversionVersion {
		return fmt.Sprintf("conversion version %q != %q", record.ConversionVersion, ConversionVersion)
	}
	if record.AlgorithmVersion != AlgorithmVersion {
		return fmt.Sprintf("algorithm version %q != %q", record.AlgorithmVersion, AlgorithmVersion)
	}
	if ContentDigest(record.Component) != record.ContentHash {
		return fmt.Sprintf("content hash mismatch for %s tile=%d,%d", record.StrokeID, record.TileX, record.TileY)
	}
	return ""
}

func commitNameMatchesSequence(name string, sequence int64) bool {
	var parsed int64
	_, err := fmt.Sscanf(name, "%012d.json", &parsed)
	return err == nil && parsed == sequence
}

func (s *CheckpointStore) resetFiles() {
	entries, _ := os.ReadDir(s.commitsDir)
	for _, entry := range entries {
		_ = os.Remove(filepath.Join(s.commitsDir, entry.Name()))
	}
	_ = os.Remove(filepath.Join(s.dir, "HEAD.json"))
	s.sequence = 0
	s.records = nil
	s.latest = map[TileCoord]StrokeCommitRecord{}
}

// SimulateTornWriteForTest leaves a partial temp file like SIGKILL mid-write.
func (s *CheckpointStore) SimulateTornWriteForTest(payload []byte) error {
	tmp, err := os.CreateTemp(s.commitsDir, ".commit-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	return tmp.Close()
}
