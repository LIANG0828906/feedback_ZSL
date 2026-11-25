package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func JSONHash(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func MustJSONHash(v any) string {
	hash, err := JSONHash(v)
	if err != nil {
		panic(err)
	}
	return hash
}

func Fingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
