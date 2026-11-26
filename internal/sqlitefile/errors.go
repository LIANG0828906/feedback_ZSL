package sqlitefile

import "errors"

var (
	ErrNotSQLite           = errors.New("sqlitefile: missing SQLite format 3 header")
	ErrUnsupportedPageSize = errors.New("sqlitefile: unsupported page size")
	ErrTruncatedVarint     = errors.New("sqlitefile: truncated varint")
	ErrTruncatedRecord     = errors.New("sqlitefile: truncated record")
	ErrTooLarge            = errors.New("sqlitefile: checkpoint document exceeds dependency-free single-leaf writer capacity")
)
