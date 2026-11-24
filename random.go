package syncgame

import (
	"encoding/binary"
	"hash/fnv"
)

// deriveUint64 is a pure function of (seed, tick, player, purpose). No mutable
// random cursor is shared by a simulation path, which makes recovery and replay
// independent of scheduling.
func deriveUint64(seed uint64, tick int, playerID, purpose string) uint64 {
	h := fnv.New64a()
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], seed)
	binary.BigEndian.PutUint64(b[8:], uint64(tick))
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(playerID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(purpose))
	return splitMix64(h.Sum64())
}

func splitMix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func deriveRange(seed uint64, tick int, playerID, purpose string, low, high int) int {
	if high <= low {
		return low
	}
	n := high - low + 1
	return low + int(deriveUint64(seed, tick, playerID, purpose)%uint64(n))
}
