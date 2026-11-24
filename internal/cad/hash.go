package cad

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return SHA256Hex(data)
}

func HashJSONStable(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		panic(err)
	}
	stable, err := json.Marshal(generic)
	if err != nil {
		panic(err)
	}
	return SHA256Hex(stable)
}
