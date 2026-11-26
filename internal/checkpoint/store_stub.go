//go:build !cgo

package checkpoint

import "quota-cluster/internal/bizerror"

func openStore(path string) (*Store, error) {
	return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, path, "SQLite checkpoint support requires cgo with libsqlite3")
}

func (s *Store) close() error { return nil }

func (s *Store) Save(cp *Checkpoint, opts SaveOptions) error {
	return bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "SQLite checkpoint support requires cgo")
}

func (s *Store) Load(expectedProjectHash string) (*Checkpoint, error) {
	return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "SQLite checkpoint support requires cgo")
}
