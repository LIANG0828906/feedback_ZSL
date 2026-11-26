package faultinject

import (
	"crypto/sha256"
	"encoding/hex"

	"schedlab/internal/runtime"
)

func storeDir(s *runtime.Store) string { return s.Dir() }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
