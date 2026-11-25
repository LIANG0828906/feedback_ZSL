//go:build !cgo

// Package sqlitecheckpoint exposes a clear build error when CGO is disabled.
package sqlitecheckpoint

import (
	"errors"

	ct "colortrace"
)

// Store embeds the in-memory contract only so callers still compile. Open
// always reports that the real SQLite implementation requires CGO.
type Store struct {
	*ct.MemoryStore
	path string
}

// Open reports that this binary was built without the cgo SQLite binding.
func Open(path string, _ ct.SnapshotMeta) (*Store, error) {
	return &Store{MemoryStore: ct.NewMemoryStore(), path: path},
		errors.New("sqlitecheckpoint requires CGO_ENABLED=1 and libsqlite3.so.0")
}
