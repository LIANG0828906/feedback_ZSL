package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/model"
)

const RecordVersion = 1

type SegmentRecord struct {
	Version          int                 `json:"version"`
	ContentHash      string              `json:"content_hash"`
	ParamVersion     string              `json:"param_version"`
	AlgorithmVersion string              `json:"algorithm_version"`
	Config           model.Config        `json:"config"`
	Segment          model.SegmentResult `json:"segment"`
}

type Store struct {
	Dir  string
	lock *sync.Mutex
}

var dirLocks sync.Map

func New(dir string) *Store {
	clean := filepath.Clean(dir)
	value, _ := dirLocks.LoadOrStore(clean, &sync.Mutex{})
	return &Store{Dir: clean, lock: value.(*sync.Mutex)}
}

func (s *Store) segmentDir() string { return filepath.Join(s.Dir, "segments") }

func (s *Store) Commit(contentHash, paramVersion, algorithm string, cfg model.Config, segment model.SegmentResult) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if segment.Index < 0 {
		e := apperr.New(apperr.State, "cannot checkpoint a negative segment index")
		e.Field = "segment_index"
		e.Actual = segment.Index
		return e
	}
	dir := s.segmentDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return apperr.Wrap(apperr.Decode, "cannot create checkpoint directory", err)
	}
	existing, err := s.listRecordFiles()
	if err != nil {
		return err
	}
	if segment.Index < len(existing) {
		e := apperr.New(apperr.State, "segment has already been committed; committed data is frozen")
		e.Field = "segment_index"
		e.Expected = fmt.Sprintf(">=%d", len(existing))
		e.Actual = segment.Index
		return e
	}
	if segment.Index != len(existing) {
		e := apperr.New(apperr.State, "checkpoint segment commits must be stable and sequential")
		e.Field = "segment_index"
		e.Expected = len(existing)
		e.Actual = segment.Index
		return e
	}
	record := SegmentRecord{
		Version: RecordVersion, ContentHash: contentHash,
		ParamVersion: paramVersion, AlgorithmVersion: algorithm,
		Config: cfg, Segment: segment,
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return apperr.Wrap(apperr.Decode, "cannot encode checkpoint segment", err)
	}
	data = append(data, '\n')
	finalPath := filepath.Join(dir, fmt.Sprintf("%06d.json", segment.Index))
	tmp, err := os.CreateTemp(dir, fmt.Sprintf(".%06d-*.tmp", segment.Index))
	if err != nil {
		return apperr.Wrap(apperr.Decode, "cannot create checkpoint temp file", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return apperr.Wrap(apperr.Decode, "cannot write checkpoint temp file", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return apperr.Wrap(apperr.Decode, "cannot fsync checkpoint temp file", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return apperr.Wrap(apperr.Decode, "cannot close checkpoint temp file", err)
	}
	if _, err := os.Stat(finalPath); err == nil {
		os.Remove(tmpName)
		e := apperr.New(apperr.State, "checkpoint file already exists")
		e.Field = "segment_index"
		e.Expected = len(existing)
		e.Actual = segment.Index
		return e
	}
	if err := os.Rename(tmpName, finalPath); err != nil {
		os.Remove(tmpName)
		return apperr.Wrap(apperr.Decode, "cannot atomically publish checkpoint segment", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *Store) Load(contentHash, paramVersion, algorithm string) ([]model.SegmentResult, *model.Invalidation, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	files, err := s.listRecordFiles()
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, nil
	}
	records := make([]SegmentRecord, 0, len(files))
	for i, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, apperr.Wrap(apperr.Decode, "cannot read checkpoint segment", err)
		}
		var rec SegmentRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, nil, apperr.Wrap(apperr.Decode, "checkpoint segment is not valid JSON", err)
		}
		if rec.Version != RecordVersion {
			e := apperr.New(apperr.Decode, "unsupported checkpoint record version")
			e.Field = "checkpoint_version"
			e.Expected = RecordVersion
			e.Actual = rec.Version
			return nil, nil, e
		}
		if rec.Segment.Index != i {
			e := apperr.New(apperr.Decode, "checkpoint segments are not contiguous and ordered")
			e.Field = "segment_index"
			e.Expected = i
			e.Actual = rec.Segment.Index
			return nil, nil, e
		}
		records = append(records, rec)
	}
	inv := compareVersions(contentHash, paramVersion, algorithm, records[0])
	if inv != nil {
		if err := s.archiveAllLocked(); err != nil {
			return nil, inv, err
		}
		return nil, inv, nil
	}
	for i := 1; i < len(records); i++ {
		if inv := compareVersions(contentHash, paramVersion, algorithm, records[i]); inv != nil {
			if err := s.archiveAllLocked(); err != nil {
				return nil, inv, err
			}
			return nil, inv, nil
		}
	}
	out := make([]model.SegmentResult, len(records))
	for i := range records {
		out[i] = records[i].Segment
	}
	return out, nil, nil
}

func compareVersions(contentHash, paramVersion, algorithm string, rec SegmentRecord) *model.Invalidation {
	inv := &model.Invalidation{
		Expected: map[string]string{},
		Actual:   map[string]string{},
	}
	check := func(field, expected, actual string) {
		if expected == actual {
			return
		}
		inv.Fields = append(inv.Fields, field)
		inv.Expected[field] = expected
		inv.Actual[field] = actual
		switch field {
		case "content_hash":
			inv.Reasons = append(inv.Reasons, "input content changed; the entire old checkpoint is obsolete")
		case "param_version":
			inv.Reasons = append(inv.Reasons, "parameter version changed; committed segments cannot be mixed with new parameters")
		case "algorithm_version":
			inv.Reasons = append(inv.Reasons, "algorithm version changed; the entire old checkpoint is obsolete")
		}
	}
	check("content_hash", contentHash, rec.ContentHash)
	check("param_version", paramVersion, rec.ParamVersion)
	check("algorithm_version", algorithm, rec.AlgorithmVersion)
	if len(inv.Fields) == 0 {
		return nil
	}
	return inv
}

func (s *Store) archiveAllLocked() error {
	src := s.segmentDir()
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return apperr.Wrap(apperr.Decode, "cannot stat checkpoint directory for archival", err)
	}
	invalidRoot := filepath.Join(s.Dir, "invalid")
	if err := os.MkdirAll(invalidRoot, 0o755); err != nil {
		return apperr.Wrap(apperr.Decode, "cannot create checkpoint archive directory", err)
	}
	name := "snapshot-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	dst := filepath.Join(invalidRoot, name)
	if err := os.Rename(src, dst); err != nil {
		return apperr.Wrap(apperr.Decode, "cannot archive invalid checkpoint snapshot", err)
	}
	marker := map[string]any{
		"invalidated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"reason":         "content_hash, param_version or algorithm_version mismatch",
	}
	if data, err := json.MarshalIndent(marker, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dst, "_invalid.json"), append(data, '\n'), 0o644)
	}
	return nil
}

func (s *Store) listRecordFiles() ([]string, error) {
	entries, err := os.ReadDir(s.segmentDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, apperr.Wrap(apperr.Decode, "cannot list checkpoint segments", err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		base := strings.TrimSuffix(entry.Name(), ".json")
		if len(base) != 6 {
			continue
		}
		if _, err := strconv.Atoi(base); err != nil {
			continue
		}
		files = append(files, filepath.Join(s.segmentDir(), entry.Name()))
	}
	sort.Strings(files)
	return files, nil
}

func (s *Store) Reset() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := os.RemoveAll(s.segmentDir()); err != nil {
		return err
	}
	return nil
}

func (s *Store) HasSnapshot() bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	files, err := s.listRecordFiles()
	return err == nil && len(files) > 0
}
