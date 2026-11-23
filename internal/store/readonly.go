package store

import (
	"os"
	"path/filepath"
)

// OpenReadOnly replays a checkpoint without ever holding a writable handle.
// It is used by the comparison and verification paths so their pure
// recomputation cannot mutate persisted state.
func OpenReadOnly(dir string) (*Store, error) {
	meta, err := readMeta(dir)
	if err != nil {
		return nil, err
	}
	blocks, frames, err := readLog(filepath.Join(dir, LogName))
	if err != nil {
		return nil, err
	}
	return &Store{
		dir:     dir,
		file:    nil,
		meta:    meta,
		records: blocks,
		frames:  frames,
		nextSeq: len(blocks) + len(frames),
	}, nil
}

// Exists reports whether a checkpoint directory already has metadata.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, MetaName))
	return err == nil
}
