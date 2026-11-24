package contenthash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func Of(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		sum := sha256.Sum256([]byte(err.Error()))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
