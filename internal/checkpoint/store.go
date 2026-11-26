// Package checkpoint writes crash-atomic SQLite quota checkpoints.
package checkpoint

import (
	"unsafe"

	"quota-cluster/internal/engine"
	"quota-cluster/internal/model"
)

// Checkpoint is the recoverable committed moment.
type Checkpoint struct {
	FormatVersion string                 `json:"format_version"`
	ProjectID     string                 `json:"project_id"`
	ProjectHash   string                 `json:"project_hash"`
	CommittedTick int64                  `json:"committed_tick"`
	TenantStates  []model.TenantSnapshot `json:"tenant_states"`
	NodeStates    []model.NodeSnapshot   `json:"node_states"`
	Queue         []model.QueueMember    `json:"queue"`
	Decisions     []model.Decision       `json:"decisions"`
	EngineState   *engine.State          `json:"engine_state,omitempty"`
}

// SaveOptions controls checkpoint writing.
type SaveOptions struct {
	ProjectHash      string
	State            *engine.State
	BeforeGlobalTick func()
}

// Store is implemented by the cgo SQLite backend and a no-cgo fallback.
type Store struct {
	path string
	db   unsafe.Pointer
}

func Open(path string) (*Store, error) { return openStore(path) }

func (s *Store) Close() error { return s.close() }

// SaveCheckpoint is a convenience one-shot save.
func SaveCheckpoint(path string, cp *Checkpoint, opts SaveOptions) error {
	st, err := Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.Save(cp, opts)
}

// LoadCheckpoint is a convenience one-shot load.
func LoadCheckpoint(path, expectedProjectHash string) (*Checkpoint, error) {
	st, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.Load(expectedProjectHash)
}
