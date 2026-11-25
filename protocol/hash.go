package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func SessionContentHash(session *Session) (string, error) {
	if session == nil {
		return "", Errorf(CodeSchema, "session", "$", "session is nil")
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return "", Errorf(CodeDecode, "session", "$", "cannot canonicalize session: %v", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func HashBytes(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func HashText(text string) string {
	return HashBytes([]byte(text))
}
