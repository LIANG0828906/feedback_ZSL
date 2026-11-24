package gen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
)

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func unitName(i int) string {
	return "u" + string(rune('a'+(i/26))) + string(rune('a'+(i%26)))
}

func sortStrings(s []string) { sort.Strings(s) }

// hashStr 是确定性字符串混合，用于从对局 ID 派生稳定偏移。
func hashStr(s string) uint64 {
	h := uint64(1469598103934665603)
	for _, c := range s {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}
