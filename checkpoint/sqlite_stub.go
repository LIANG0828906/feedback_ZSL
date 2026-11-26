//go:build !cgo || !linux

package checkpoint

import (
	"context"

	"iconflow/errs"
)

type SQLiteStore struct{}

func OpenSQLite(path string) (*SQLiteStore, error) {
	return nil, errs.New(errs.ECheck, path, "CGO/linux SQLite adapter is unavailable; use OpenFileStore for stdlib atomic checkpoints")
}

func (s *SQLiteStore) Close() error { return nil }

func (s *SQLiteStore) CommitStage(context.Context, StageCommit) (int64, error) {
	return 0, errs.New(errs.ECheck, "$.sqlite", "SQLite adapter is unavailable in this build")
}

func (s *SQLiteStore) CommitWindow(context.Context, WindowCommit) (int64, error) {
	return 0, errs.New(errs.ECheck, "$.sqlite", "SQLite adapter is unavailable in this build")
}

func (s *SQLiteStore) StageHistory(context.Context, string) ([]StageCommit, error) {
	return nil, errs.New(errs.ECheck, "$.sqlite", "SQLite adapter is unavailable in this build")
}

func (s *SQLiteStore) WindowHistory(context.Context, string) ([]WindowCommit, error) {
	return nil, errs.New(errs.ECheck, "$.sqlite", "SQLite adapter is unavailable in this build")
}
