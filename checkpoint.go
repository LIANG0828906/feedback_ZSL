package quantforensics

import "sync"

type StageRecord struct {
	Seq           int64      `json:"seq"`
	ProjectID     string     `json:"projectId"`
	BlockID       string     `json:"blockId"`
	Stage         BlockStage `json:"stage"`
	State         string     `json:"state"`
	OrderKey      int        `json:"orderKey"`
	OutputSummary string     `json:"outputSummary"`
	CacheKey      string     `json:"cacheKey"`
	ContentHash   string     `json:"contentHash"`
	ParamVersion  string     `json:"paramVersion"`
	AlgoVersion   string     `json:"algoVersion"`
	OutputJSON    []byte     `json:"outputJson"`
	CommitNano    int64      `json:"commitNano"`
}

type CheckpointStore interface {
	Path() string
	Init() error
	CommitStage(record StageRecord) (int64, error)
	Records(projectID string) ([]StageRecord, error)
	Close() error
}

type MemoryStore struct {
	mu      sync.Mutex
	path    string
	nextSeq int64
	records []StageRecord
}

func NewMemoryStore(path string) *MemoryStore {
	if path == "" {
		path = ":memory:"
	}
	return &MemoryStore{path: path, nextSeq: 1}
}

func (s *MemoryStore) Path() string { return s.path }
func (s *MemoryStore) Init() error  { return nil }

func (s *MemoryStore) CommitStage(record StageRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.Seq = s.nextSeq
	s.nextSeq++
	if record.CommitNano == 0 {
		record.CommitNano = record.Seq
	}
	b := append([]byte(nil), record.OutputJSON...)
	record.OutputJSON = b
	s.records = append(s.records, record)
	return record.Seq, nil
}

func (s *MemoryStore) Records(projectID string) ([]StageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []StageRecord{}
	for _, r := range s.records {
		if r.ProjectID == projectID {
			r.OutputJSON = append([]byte(nil), r.OutputJSON...)
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *MemoryStore) Close() error { return nil }

func LatestStageRecords(records []StageRecord) map[string]StageRecord {
	latest := map[string]StageRecord{}
	for _, r := range records {
		if r.State != string(StateCommitted) {
			continue
		}
		key := r.BlockID + ":" + string(r.Stage)
		old, ok := latest[key]
		if !ok || r.Seq > old.Seq {
			latest[key] = r
		}
	}
	return latest
}
