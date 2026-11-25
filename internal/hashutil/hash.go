package hashutil

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func JSONSHA256(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return SHA256(data), nil
}
