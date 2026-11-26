//go:build !cgo || !linux

package sqlite

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
)

func init() {
	sql.Register("sqlite3", unavailableDriver{})
	sql.Register("warmcache-sqlite3", unavailableDriver{})
}

type unavailableDriver struct{}

func (unavailableDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("sqlite driver requires cgo and the system libsqlite3 on Linux")
}
