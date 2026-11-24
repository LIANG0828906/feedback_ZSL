package generator

import (
	"encoding/binary"
	"os"
)

func WritePCM(path string, samples []int32) error {
	b := make([]byte, len(samples)*4)
	for i, v := range samples {
		binary.LittleEndian.PutUint32(b[i*4:], uint32(v))
	}
	return os.WriteFile(path, b, 0o644)
}

func ReadPCM(path string) ([]int32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make([]int32, len(b)/4)
	for i := range out {
		out[i] = int32(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}
