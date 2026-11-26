//go:build !(cgo && linux)

// Package sqlite has no native bridge on this platform. The SQLStore remains
// usable with any externally registered database/sql SQLite driver.
package sqlite

const DriverName = ""
