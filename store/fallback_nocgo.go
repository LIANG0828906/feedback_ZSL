//go:build !cgo

package store

func openStore(path string) (Store, error) {
	return openFileStore(path)
}
