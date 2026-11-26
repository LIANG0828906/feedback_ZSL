package hashx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func Strings(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func Short(full string, n int) string {
	if len(full) <= n {
		return full
	}
	return full[:n]
}
