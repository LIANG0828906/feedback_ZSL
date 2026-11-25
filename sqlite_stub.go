//go:build !cgo || !linux

package thermalcache

type sqliteDB struct{}
type sqliteRow []any

func openSQLiteDB(path string, readOnly bool) (*sqliteDB, error) {
	return nil, biz(CodeSQLiteUnavailable, "SQLite requires cgo and a system libsqlite3", map[string]any{"path": path})
}

func (d *sqliteDB) Close() error { return nil }
func (d *sqliteDB) Exec(query string, args ...any) error {
	return biz(CodeSQLiteUnavailable, "SQLite is unavailable", nil)
}
func (d *sqliteDB) Query(query string, args ...any) ([]sqliteRow, error) {
	return nil, biz(CodeSQLiteUnavailable, "SQLite is unavailable", nil)
}
