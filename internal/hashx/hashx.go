// Package hashx provides small deterministic content and cache hashes.
package hashx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CanonicalJSON marshals a value with deterministic struct order.
func CanonicalJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("hashx: marshal canonical document: %v", err))
	}
	return data
}

// SHA256Hex returns the hex SHA-256 of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Of returns a short content address.
func Of(v any) string {
	return "sha256:" + SHA256Hex(CanonicalJSON(v))[:32]
}

// Key joins upstream keys into one order-independent cache key.
func Key(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			clean = append(clean, part)
		}
	}
	sort.Strings(clean)
	return "ck:" + SHA256Hex([]byte(strings.Join(clean, "|")))[:24]
}
