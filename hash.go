package quantforensics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func sha256Bytes(b []byte) [32]byte { return sha256.Sum256(b) }

func HashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func CanonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// All core model values passed here are JSON-safe by construction.
		panic(err)
	}
	return b
}

func CanonicalHash(v any) string {
	return HashHex(CanonicalJSON(v))
}

// BlockContentHash hashes exactly the local pixel bytes and dimensions. It is
// independent of map iteration and block completion order.
func BlockContentHash(im Image, b Block) string {
	h := sha256.New()
	_, _ = h.Write([]byte(im.ID))
	var dims [16]byte
	dims[0] = byte(b.W >> 24)
	dims[1] = byte(b.W >> 16)
	dims[2] = byte(b.W >> 8)
	dims[3] = byte(b.W)
	dims[4] = byte(b.H >> 24)
	dims[5] = byte(b.H >> 16)
	dims[6] = byte(b.H >> 8)
	dims[7] = byte(b.H)
	_, _ = h.Write(dims[:8])
	for y := 0; y < b.H; y++ {
		for x := 0; x < b.W; x++ {
			c := im.At(b.X+x, b.Y+y)
			_, _ = h.Write([]byte{c.R, c.G, c.B})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
