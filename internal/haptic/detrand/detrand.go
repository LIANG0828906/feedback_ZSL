package detrand

import "math"

func mix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func SubSource(seed int64, frameNo int) uint64 {
	return mix64(uint64(seed) ^ (uint64(frameNo) * 0x9e3779b185ebca87))
}

func JitterPermille(seed int64, frameNo, amplitude int) int {
	if amplitude <= 0 {
		return 0
	}
	r := SubSource(seed, frameNo)
	bound := uint64(amplitude*2 + 1)
	if amplitude == math.MaxInt {
		bound = uint64(math.MaxInt64)
	}
	v := int(r % bound)
	return v - amplitude
}

func Progress(consumedEventOffset int) int64 {
	return int64(consumedEventOffset)
}
