package mirror

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// HashJSON returns a stable SHA-256 of a JSON-serializable value. Structs and
// encoding/json's sorted map keys keep this independent of map iteration.
func HashJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var canonical any
	if err := dec.Decode(&canonical); err != nil {
		panic(err)
	}
	data, err = json.Marshal(canonical)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
