package project

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
)

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RecomputeContentHash hashes the canonical manifest plus the current terrain
// arrays. Truth annotations and edit history are metadata and do not affect the
// render cache content hash.
func (p *Project) RecomputeContentHash() string {
	h := sha256.New()
	manifestBytes, _ := json.Marshal(p.Manifest)
	_, _ = h.Write(manifestBytes)
	for _, id := range p.BlockOrder {
		bd := p.Blocks[id]
		if bd == nil {
			continue
		}
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{0})
		writeHashLen(h, uint64(len(bd.Height)))
		for _, v := range bd.Height {
			_, _ = h.Write(uint64Bytes(math.Float64bits(v)))
		}
		writeHashLen(h, uint64(len(bd.Weights)))
		for _, v := range bd.Weights {
			_, _ = h.Write([]byte{byte(v), byte(v >> 8)})
		}
		writeHashLen(h, uint64(len(bd.Mask)))
		_, _ = h.Write(bd.Mask)
	}
	p.ContentHash = hex.EncodeToString(h.Sum(nil))
	return p.ContentHash
}

func writeHashLen(h interface{ Write([]byte) (int, error) }, n uint64) {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, n)
	_, _ = h.Write(buf)
}

func uint64Bytes(n uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, n)
	return b
}

func (p *Project) Summary() string {
	return fmt.Sprintf("content=%s blocks=%d edits=%d", p.ContentHash, len(p.BlockOrder), len(p.Edits))
}
