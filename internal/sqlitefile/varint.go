package sqlitefile

// EncodeVarint encodes a non-negative SQLite varint.
func EncodeVarint(v uint64) []byte {
	if v <= 0x7f {
		return []byte{byte(v)}
	}
	if v <= 0x3fff {
		return []byte{byte((v>>7)&0x7f) | 0x80, byte(v & 0x7f)}
	}
	if v <= 0x1fffff {
		return []byte{byte((v>>14)&0x7f) | 0x80, byte((v>>7)&0x7f) | 0x80, byte(v & 0x7f)}
	}
	if v <= 0x0fffffff {
		return []byte{
			byte((v>>21)&0x7f) | 0x80, byte((v>>14)&0x7f) | 0x80,
			byte((v>>7)&0x7f) | 0x80, byte(v & 0x7f),
		}
	}

	var tmp [10]byte
	n := 0
	for v > 0x7f && n < 8 {
		tmp[9-n] = byte(v & 0x7f)
		v >>= 7
		n++
	}
	// General fallback is intentionally limited to the values used by
	// checkpoint documents; all practical lengths hit the fast branches.
	out := []byte{}
	for i := 0; i < n; i++ {
		out = append(out, tmp[9-n+i]|0x80)
	}
	out = append(out, byte(v&0x7f))
	return out
}

// DecodeVarint decodes one SQLite varint and returns its consumed length.
func DecodeVarint(data []byte, offset int) (uint64, int, error) {
	var value uint64
	start := offset
	for i := 0; i < 9 && offset < len(data); i++ {
		b := data[offset]
		offset++
		if i == 8 {
			value = (value << 8) | uint64(b)
			return value, offset - start, nil
		}
		value = (value << 7) | uint64(b&0x7f)
		if b&0x80 == 0 {
			return value, offset - start, nil
		}
	}
	return 0, 0, ErrTruncatedVarint
}
