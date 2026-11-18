package seed

import (
	"encoding/binary"
	"fmt"
)

const (
	SmallCount  = 140
	MediumCount = 60
)

type Spec struct {
	ID   string
	Seed int64
	Size int64
	Kind string
}

// FullProfile returns the fixed local dataset: 140 small objects (64 KiB to
// 512 KiB) and 60 medium objects (1 MiB to 4 MiB). Sizes are selected
// deterministically so the aggregate payload is approximately 300 MiB.
func FullProfile(seedBase int64) []Spec {
	out := make([]Spec, 0, SmallCount+MediumCount)
	for i := 0; i < SmallCount; i++ {
		var size int64
		if i%3 == 0 {
			size = 64<<10 + int64(i*7919)%(448<<10+1)
		} else {
			size = 512<<10 - int64(i%7)*(1<<10)
		}
		out = append(out, Spec{
			ID:   fmt.Sprintf("seed-s-%03d", i),
			Seed: seedBase + int64(i+1),
			Size: size,
			Kind: "small",
		})
	}
	for i := 0; i < MediumCount; i++ {
		size := 4<<20 - int64(i%32)*(8<<10)
		out = append(out, Spec{
			ID:   fmt.Sprintf("seed-m-%03d", i),
			Seed: seedBase + int64(10000+i),
			Size: size,
			Kind: "medium",
		})
	}
	return out
}

func Generate(seed int64, size int64) []byte {
	if size <= 0 {
		return []byte{}
	}
	buf := make([]byte, size)
	state := uint64(seed)
	var tmp [8]byte
	for off := 0; off < len(buf); off += 8 {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z = z ^ (z >> 31)
		binary.LittleEndian.PutUint64(tmp[:], z)
		n := copy(buf[off:], tmp[:])
		if n == 0 {
			break
		}
	}
	return buf
}

func SumSizes(specs []Spec) (total int64, small int64, medium int64, smallN, mediumN int) {
	for _, sp := range specs {
		total += sp.Size
		if sp.Kind == "small" {
			small += sp.Size
			smallN++
		} else {
			medium += sp.Size
			mediumN++
		}
	}
	return
}
