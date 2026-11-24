package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func canonicalJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func stableDigest(v any) string {
	sum := sha256.Sum256(canonicalJSON(v))
	return hex.EncodeToString(sum[:])
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func rankKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
