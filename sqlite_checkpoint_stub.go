//go:build !cgo

package leasekit

type SQLiteCheckpointStore struct {
	Path string
}

func NewSQLiteCheckpointStore(path string) *SQLiteCheckpointStore {
	return &SQLiteCheckpointStore{Path: path}
}

func (s *SQLiteCheckpointStore) Location() string { return s.Path }

func (s *SQLiteCheckpointStore) unavailable() error {
	return validationError("E_SQLITE_CGO_DISABLED", s.Path, "this binary was built without cgo; use FileCheckpointStore or rebuild with CGO_ENABLED=1")
}

func (s *SQLiteCheckpointStore) WriteHolderStates(Checkpoint) error {
	return s.unavailable()
}

func (s *SQLiteCheckpointStore) WriteCommittedSeq(Checkpoint) error {
	return s.unavailable()
}

func (s *SQLiteCheckpointStore) LoadCheckpoint() (*Checkpoint, error) {
	return nil, s.unavailable()
}

type sqliteDB struct{}

func openSQLite(string) (*sqliteDB, error) {
	return nil, validationError("E_SQLITE_CGO_DISABLED", "sqlite", "cgo is disabled")
}

func (*sqliteDB) exec(string) error { return nil }
func (*sqliteDB) close() error      { return nil }
