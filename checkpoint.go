package maskqc

import "context"

// ComponentRecord is one stroke's coverage on one affected chunk.
type ComponentRecord struct {
	ProjectID        string     `json:"project_id"`
	OrderIndex       int        `json:"order_index"`
	StrokeSeq        int        `json:"stroke_seq"`
	Chunk            ChunkCoord `json:"chunk"`
	Bounds           Rect       `json:"bounds"`
	Alpha            []byte     `json:"alpha"`
	CacheKey         string     `json:"cache_key"`
	ContentHash      string     `json:"content_hash"`
	ResidualDigest   string     `json:"residual_digest"`
	OrderVersion     string     `json:"order_version"`
	AlgorithmVersion string     `json:"algorithm_version"`
	CommitSeq        int64      `json:"commit_seq,omitempty"`
}

// StrokeCheckpoint is the atomic unit committed for one stroke.
type StrokeCheckpoint struct {
	ProjectID       string            `json:"project_id"`
	SchemaVersion   string            `json:"schema_version"`
	OrderIndex      int               `json:"order_index"`
	StrokeSeq       int               `json:"stroke_seq"`
	RootContentHash string            `json:"root_content_hash"`
	OrderVersion    string            `json:"order_version"`
	ParamsVersion   string            `json:"params_version"`
	Components      []ComponentRecord `json:"components"`
}

// CheckpointSink is implemented by the SQLite atomic checkpoint store.
type CheckpointSink interface {
	CommitStrokeCheckpoint(ctx context.Context, checkpoint StrokeCheckpoint) (int64, error)
}

// CheckpointSource loads only durably committed records after recovery.
type CheckpointSource interface {
	LoadComponentRecords(ctx context.Context, projectID string) ([]ComponentRecord, error)
	ValidateSnapshot(ctx context.Context, projectID string, identity SnapshotIdentity) (*SnapshotValidation, error)
}

// SnapshotIdentity identifies expected immutable construction state.
type SnapshotIdentity struct {
	ProjectID        string `json:"project_id"`
	RootContentHash  string `json:"root_content_hash"`
	OrderVersion     string `json:"order_version"`
	ParamsVersion    string `json:"params_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	SchemaVersion    string `json:"schema_version"`
}

// SnapshotValidation reports all-or-nothing restart invalidation.
type SnapshotValidation struct {
	Valid         bool              `json:"valid"`
	Invalidated   bool              `json:"invalidated"`
	Reason        string            `json:"reason,omitempty"`
	ChangedFields []string          `json:"changed_fields,omitempty"`
	Expected      SnapshotIdentity  `json:"expected"`
	Stored        *SnapshotIdentity `json:"stored,omitempty"`
}

// MemoryCheckpointSink is useful in tests and the no-persistence fast path.
type MemoryCheckpointSink struct {
	Records []ComponentRecord
}

func (m *MemoryCheckpointSink) CommitStrokeCheckpoint(_ context.Context, cp StrokeCheckpoint) (int64, error) {
	base := int64(len(m.Records))
	for i, rec := range cp.Components {
		rec.CommitSeq = base + int64(i) + 1
		m.Records = append(m.Records, rec)
	}
	return base + 1, nil
}
