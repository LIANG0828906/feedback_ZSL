package store

import (
	"crypto/sha256"
	"encoding/hex"

	"replaykernel/internal/prng"
)

// RootChain 是哈希链的固定创世值。
const RootChain = "root-chain-v1"

// ChainHash 把上一窗口哈希与本窗口内容哈希串成哈希链，
// 保证窗口顺序稳定、不可部分混用。
func ChainHash(prev, current string) string {
	h := prng.Mix64(0x517cc1b727220a95)
	for _, c := range prev {
		h = prng.Mix64(h ^ uint64(c))
	}
	for _, c := range current {
		h = prng.Mix64(h ^ uint64(c))
	}
	sum := sha256.Sum256([]byte(hex.EncodeToString([]byte{
		byte(h >> 56), byte(h >> 48), byte(h >> 40), byte(h >> 32),
		byte(h >> 24), byte(h >> 16), byte(h >> 8), byte(h),
	})))
	return hex.EncodeToString(sum[:])
}
