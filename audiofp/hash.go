package audiofp

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
)

// AlgorithmVersion identifies the extraction, quantization, and key layout.
const AlgorithmVersion = "afp-v1"

func digest(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(p)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func digestString(s string) string { return digest([]byte(s)) }

func writeString(h hash.Hash, s string) {
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(s)))
	_, _ = h.Write(size[:])
	_, _ = io.WriteString(h, s)
}

func canonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func recipeVersion(r Recipe) string {
	b, err := canonicalJSON(r)
	if err != nil {
		return digestString(fmt.Sprintf("%#v", r))
	}
	return digest(b)
}

// RecipeHash is the stable canonical hash of a recipe.
func RecipeHash(r Recipe) string { return recipeVersion(r) }

// ShardCacheKey exposes the dependency-derived key for one fingerprint shard.
func ShardCacheKey(contentHash string, r Recipe, firstFrame, lastFrame int) string {
	return shardCacheKey(contentHash, r, firstFrame, lastFrame)
}
