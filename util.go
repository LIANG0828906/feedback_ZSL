package iconmask

import "crypto/sha256"

func sha256String(value string) [32]byte {
	return sha256.Sum256([]byte(value))
}
