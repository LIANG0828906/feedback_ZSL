package hashutil

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"sort"
)

func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func CanonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func CanonicalHash(v any) string {
	return SHA256Hex(CanonicalJSON(v))
}

func HashInt64Values(prefix string, dims ...int) func(values []int64) string {
	return func(values []int64) string {
		h := sha256.New()
		_, _ = h.Write([]byte(prefix))
		var buf [8]byte
		for _, d := range dims {
			binary.LittleEndian.PutUint64(buf[:], uint64(d))
			_, _ = h.Write(buf[:])
		}
		for _, v := range values {
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
			_, _ = h.Write(buf[:])
		}
		return hex.EncodeToString(h.Sum(nil))
	}
}

func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func HashStringPairs(pairs ...string) string {
	h := sha256.New()
	var lenBuf [4]byte
	for _, p := range pairs {
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(p)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
