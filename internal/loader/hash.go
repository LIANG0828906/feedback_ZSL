package loader

import (
	"crypto/sha256"
	"encoding/hex"
)

// contentHash deterministically hashes the project content bytes. The
// checkpoint store uses it to decide whether a persisted snapshot still
// matches the project on disk.
func contentHash(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ContentHash exposes the hash for the checkpoint package.
func ContentHash(parts ...[]byte) string { return contentHash(parts...) }
