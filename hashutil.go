package iconcut

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func CanonicalJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		return nil, err
	}
	return json.Marshal(x)
}

func HashJSON(v any) string {
	b, err := CanonicalJSON(v)
	if err != nil {
		return SHA256Bytes([]byte(err.Error()))
	}
	return SHA256Bytes(b)
}
