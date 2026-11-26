package maskqc

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type StageState string

const (
	StagePending    StageState = "pending"
	StageProcessing StageState = "processing"
	StageCommitted  StageState = "committed"
	StageFailed     StageState = "failed"

	PhaseCoverage = "coverage_accumulation"
	PhaseTopology = "topology_verification"
)

var legalStageTransitions = map[StageState]map[StageState]bool{
	StagePending:    {StageProcessing: true},
	StageProcessing: {StageCommitted: true, StageFailed: true},
	StageFailed:     {StageProcessing: true},
	// A new invalidation generation is allowed to re-enter processing; all
	// other backward jumps remain illegal.
	StageCommitted: {StageProcessing: true},
}

type StrokeCommitRecord struct {
	TxSeq            int64  `json:"tx_seq"`
	Kind             string `json:"kind"`
	StrokeID         string `json:"stroke_id"`
	ChunkID          int    `json:"chunk_id"`
	Coverage         []byte `json:"coverage"`
	CacheKey         string `json:"cache_key"`
	ContentHash      string `json:"content_hash"`
	ResidualDigest   string `json:"residual_digest"`
	OrderVersion     string `json:"order_version"`
	AlgorithmVersion string `json:"algorithm_version"`
}

type StageTransitionRecord struct {
	TxSeq   int64      `json:"tx_seq"`
	Kind    string     `json:"kind"`
	ChunkID int        `json:"chunk_id"`
	Phase   string     `json:"phase"`
	From    StageState `json:"from"`
	To      StageState `json:"to"`
	Reason  string     `json:"reason,omitempty"`
}

type ChunkResultRecord struct {
	TxSeq     int64  `json:"tx_seq"`
	Kind      string `json:"kind"`
	ChunkID   int    `json:"chunk_id"`
	Mask      []byte `json:"mask"`
	CacheKey  string `json:"cache_key"`
	InputHash string `json:"input_hash"`
}

type txEnvelope struct {
	Seq      int64           `json:"seq"`
	Kind     string          `json:"kind"`
	Checksum string          `json:"checksum"`
	Payload  json.RawMessage `json:"payload"`
}

// Store is a pure-Go append-only transaction log. Go's standard library has no
// sqlite3 driver, and the task permits a repository-local pure-Go file store.
type Store struct {
	dir         string
	path        string
	mu          sync.Mutex
	file        *os.File
	nextSeq     int64
	strokes     []StrokeCommitRecord
	components  map[string]StrokeCommitRecord
	results     map[int]ChunkResultRecord
	stages      map[string]StageState
	transitions []StageTransitionRecord
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "checkpoint.txlog")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Store{
		dir: dir, path: path, file: f,
		components: map[string]StrokeCommitRecord{},
		results:    map[int]ChunkResultRecord{},
		stages:     map[string]StageState{},
	}
	if err := s.replay(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Sync()
	if closeErr := s.file.Close(); err == nil {
		err = closeErr
	}
	s.file = nil
	return err
}

func checksumPayload(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (s *Store) append(kind string, payload any) (int64, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	env := txEnvelope{Seq: s.nextSeq + 1, Kind: kind, Checksum: checksumPayload(data), Payload: data}
	line, err := json.Marshal(env)
	if err != nil {
		return 0, err
	}
	if _, err := s.file.Write(append(line, '\n')); err != nil {
		return 0, err
	}
	if err := s.file.Sync(); err != nil {
		return 0, err
	}
	s.nextSeq = env.Seq
	return env.Seq, nil
}

func (s *Store) replay() error {
	info, err := s.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	if _, err := s.file.Seek(0, 0); err != nil {
		return err
	}
	scanner := bufio.NewScanner(s.file)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	var validBytes int64
	for scanner.Scan() {
		line := scanner.Bytes()
		startOffset := validBytes
		_ = startOffset
		var env txEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			// A torn final line is the expected forced-kill boundary.
			break
		}
		if checksumPayload(env.Payload) != env.Checksum {
			break
		}
		if env.Seq != s.nextSeq+1 {
			return fmt.Errorf("checkpoint sequence gap: want %d got %d", s.nextSeq+1, env.Seq)
		}
		switch env.Kind {
		case "stroke-component":
			var rec StrokeCommitRecord
			if err := json.Unmarshal(env.Payload, &rec); err != nil {
				return err
			}
			s.strokes = append(s.strokes, rec)
			s.components[componentKey(rec.StrokeID, rec.ChunkID)] = rec
		case "chunk-result":
			var rec ChunkResultRecord
			if err := json.Unmarshal(env.Payload, &rec); err != nil {
				return err
			}
			s.results[rec.ChunkID] = rec
		case "stage-transition":
			var rec StageTransitionRecord
			if err := json.Unmarshal(env.Payload, &rec); err != nil {
				return err
			}
			key := stageKey(rec.ChunkID, rec.Phase)
			s.stages[key] = rec.To
			s.transitions = append(s.transitions, rec)
		}
		s.nextSeq = env.Seq
		validBytes += int64(len(line)) + 1
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// Restore append position and truncate a torn tail if necessary.
	if err := s.file.Sync(); err != nil {
		return err
	}
	if err := s.file.Truncate(validBytes); err != nil {
		return err
	}
	_, err = s.file.Seek(validBytes, 0)
	return err
}

func stageKey(chunkID int, phase string) string {
	return fmt.Sprintf("%d:%s", chunkID, phase)
}

func componentKey(strokeID string, chunkID int) string {
	return fmt.Sprintf("%s@%d", strokeID, chunkID)
}

func (s *Store) StageState(chunkID int, phase string) StageState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stageStateLocked(chunkID, phase)
}

func (s *Store) stageStateLocked(chunkID int, phase string) StageState {
	if st, ok := s.stages[stageKey(chunkID, phase)]; ok {
		return st
	}
	return StagePending
}

// TransitionStage validates and persists a state-machine transition. Illegal
// transitions return E_STATE without mutating or appending anything.
func (s *Store) TransitionStage(chunkID int, phase string, to StageState, reason string) (StageTransitionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.stageStateLocked(chunkID, phase)
	if !legalStageTransitions[from][to] {
		return StageTransitionRecord{}, &BizError{
			Code: CodeState, Message: "illegal checkpoint stage transition",
			ChunkID: chunkID, Field: "stage",
			Want: fmt.Sprintf("one of %s from %s", legalTargets(from), from),
			Got:  string(to),
		}
	}
	rec := StageTransitionRecord{ChunkID: chunkID, Kind: "stage-transition", Phase: phase, From: from, To: to, Reason: reason}
	seq, err := s.append("stage-transition", &rec)
	if err != nil {
		return StageTransitionRecord{}, err
	}
	rec.TxSeq = seq
	s.stages[stageKey(chunkID, phase)] = to
	s.transitions = append(s.transitions, rec)
	return rec, nil
}

func legalTargets(from StageState) string {
	targets := []string{}
	for to := range legalStageTransitions[from] {
		targets = append(targets, string(to))
	}
	sort.Strings(targets)
	return "[" + strings.Join(targets, ",") + "]"
}

func (s *Store) CommitStrokeComponent(rec StrokeCommitRecord) (StrokeCommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.Kind = "stroke-component"
	rec.TxSeq = s.nextSeq + 1
	seq, err := s.append(rec.Kind, &rec)
	if err != nil {
		return StrokeCommitRecord{}, err
	}
	rec.TxSeq = seq
	s.strokes = append(s.strokes, rec)
	s.components[componentKey(rec.StrokeID, rec.ChunkID)] = rec
	return rec, nil
}

func (s *Store) StrokeComponent(strokeID string, chunkID int) (StrokeCommitRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.components[componentKey(strokeID, chunkID)]
	return r, ok
}

func (s *Store) CommitChunkResult(rec ChunkResultRecord) (ChunkResultRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.Kind = "chunk-result"
	seq, err := s.append(rec.Kind, &rec)
	if err != nil {
		return ChunkResultRecord{}, err
	}
	rec.TxSeq = seq
	s.results[rec.ChunkID] = rec
	return rec, nil
}

func (s *Store) StrokeCommits() []StrokeCommitRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]StrokeCommitRecord(nil), s.strokes...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].TxSeq < out[j].TxSeq })
	return out
}

func (s *Store) LogicalCommitSequence() []string {
	commits := s.StrokeCommits()
	out := make([]string, 0, len(commits))
	for _, c := range commits {
		out = append(out, fmt.Sprintf("%06d:%s@chunk-%d:%s", c.TxSeq, c.StrokeID, c.ChunkID, c.CacheKey[:min(12, len(c.CacheKey))]))
	}
	return out
}

// CanonicalLogicalSequence returns path-independent stroke/chunk commit IDs.
// Raw TxSeq may differ after recovery setup; this is what three-path comparison uses.
func CanonicalLogicalSequence(records []StrokeCommitRecord) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, fmt.Sprintf("%s@%d:%s", r.StrokeID, r.ChunkID, r.CacheKey))
	}
	sort.Strings(out)
	return out
}

func (s *Store) ChunkResult(chunkID int) (ChunkResultRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.results[chunkID]
	return r, ok
}

func (s *Store) LatestChunkResults() map[int]ChunkResultRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]ChunkResultRecord, len(s.results))
	for k, v := range s.results {
		out[k] = v
	}
	return out
}

func (s *Store) TransitionLog() []StageTransitionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StageTransitionRecord(nil), s.transitions...)
}
