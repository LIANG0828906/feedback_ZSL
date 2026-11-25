package fog

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
)

var mediumMagic = [4]byte{'V', 'F', 'O', 'G'}

// EncodeMedium produces a deterministic length-prefixed binary payload with a
// trailing CRC32. It is the local decode/injection boundary used by diagnostics.
func EncodeMedium(m *Medium) ([]byte, error) {
	if m == nil {
		return nil, &Error{Code: EDecode, Field: "medium", Message: "nil medium"}
	}
	var buf bytes.Buffer
	buf.Write(mediumMagic[:])
	buf.WriteByte(1)
	writeString := func(s string) {
		_ = binary.Write(&buf, binary.LittleEndian, uint16(len(s)))
		buf.WriteString(s)
	}
	writeF64 := func(v float64) {
		_ = binary.Write(&buf, binary.LittleEndian, math.Float64bits(v))
	}
	writeI32 := func(v int32) {
		_ = binary.Write(&buf, binary.LittleEndian, v)
	}

	writeString(m.ID)
	for _, v := range []float64{m.Bounds.Min.X, m.Bounds.Min.Y, m.Bounds.Min.Z, m.Bounds.Max.X, m.Bounds.Max.Y, m.Bounds.Max.Z} {
		writeF64(v)
	}
	writeF64(m.Absorption)
	writeF64(m.Scattering)
	writeF64(m.Anisotropy)
	for _, r := range m.Field.Resolution {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(r))
	}
	writeF64(m.Field.VoxelSize)
	writeI32(m.Field.MinValue)
	writeI32(m.Field.MaxValue)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(m.Field.Values)))
	for _, v := range m.Field.Values {
		writeI32(v)
	}
	if m.CastShadows {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
	sum := crc32.ChecksumIEEE(buf.Bytes())
	_ = binary.Write(&buf, binary.LittleEndian, sum)
	return buf.Bytes(), nil
}

func DecodeMedium(data []byte) (*Medium, error) {
	if len(data) < 12 || !bytes.Equal(data[:4], mediumMagic[:]) {
		return nil, &Error{Code: EDecode, Field: "medium.magic", Message: "unrecognized medium payload"}
	}
	r := bytes.NewReader(data)
	if _, err := r.Seek(5, 0); err != nil {
		return nil, &Error{Code: EDecode, Field: "medium.header", Message: err.Error()}
	}
	id, idErr := readCodecString(r)

	if len(data) < 4 {
		return nil, &Error{Code: EDecode, MediaID: id, Field: "medium.crc", Message: "truncated checksum"}
	}
	payload := data[:len(data)-4]
	storedCRC := binary.LittleEndian.Uint32(data[len(data)-4:])
	actualCRC := crc32.ChecksumIEEE(payload)
	if storedCRC != actualCRC {
		return nil, &Error{
			Code: EDecode, MediaID: id, Field: "density.blob_crc32",
			Message: fmt.Sprintf("medium payload corruption: stored CRC %08x != computed %08x", storedCRC, actualCRC),
			Detail:  map[string]any{"stored_crc32": storedCRC, "computed_crc32": actualCRC, "id_parse_error": fmt.Sprint(idErr)},
		}
	}
	if idErr != nil {
		return nil, &Error{Code: EDecode, Field: "medium.id", Message: idErr.Error()}
	}

	m := &Medium{}
	var err error
	if _, err := r.Seek(5, 0); err != nil {
		return nil, &Error{Code: EDecode, Field: "medium.header", Message: err.Error()}
	}
	if m.ID, err = readCodecString(r); err != nil {
		return nil, &Error{Code: EDecode, Field: "medium.id", Message: err.Error()}
	}
	readF64 := func(field string) (float64, error) {
		var bits uint64
		if e := binary.Read(r, binary.LittleEndian, &bits); e != nil {
			return 0, &Error{Code: EDecode, MediaID: m.ID, Field: field, Message: e.Error()}
		}
		return math.Float64frombits(bits), nil
	}
	fields := []struct {
		name string
		dst  *float64
	}{
		{"bounds.min.x", &m.Bounds.Min.X}, {"bounds.min.y", &m.Bounds.Min.Y}, {"bounds.min.z", &m.Bounds.Min.Z},
		{"bounds.max.x", &m.Bounds.Max.X}, {"bounds.max.y", &m.Bounds.Max.Y}, {"bounds.max.z", &m.Bounds.Max.Z},
		{"absorption", &m.Absorption}, {"scattering", &m.Scattering}, {"anisotropy", &m.Anisotropy},
	}
	for _, f := range fields {
		v, err := readF64(f.name)
		if err != nil {
			return nil, err
		}
		*f.dst = v
	}
	for i := 0; i < 3; i++ {
		var v uint32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, &Error{Code: EDecode, MediaID: m.ID, Field: "density.resolution", Message: err.Error()}
		}
		m.Field.Resolution[i] = int(v)
	}
	if m.Field.VoxelSize, err = readF64("density.voxel_size"); err != nil {
		return nil, err
	}
	var minV, maxV int32
	if err := binary.Read(r, binary.LittleEndian, &minV); err != nil {
		return nil, &Error{Code: EDecode, MediaID: m.ID, Field: "density.min_value", Message: err.Error()}
	}
	if err := binary.Read(r, binary.LittleEndian, &maxV); err != nil {
		return nil, &Error{Code: EDecode, MediaID: m.ID, Field: "density.max_value", Message: err.Error()}
	}
	m.Field.MinValue, m.Field.MaxValue = minV, maxV
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, &Error{Code: EDecode, MediaID: m.ID, Field: "density.value_count", Message: err.Error()}
	}
	m.Field.Values = make([]int32, count)
	for i := range m.Field.Values {
		if err := binary.Read(r, binary.LittleEndian, &m.Field.Values[i]); err != nil {
			return nil, &Error{Code: EDecode, MediaID: m.ID, Field: fmt.Sprintf("density.values[%d]", i), Message: err.Error()}
		}
	}
	var shadowByte byte
	if shadowByte, err = r.ReadByte(); err != nil {
		return nil, &Error{Code: EDecode, MediaID: m.ID, Field: "cast_shadows", Message: err.Error()}
	}
	m.CastShadows = shadowByte != 0
	m.Rehash()
	return m, nil
}

func readCodecString(r *bytes.Reader) (string, error) {
	var n uint16
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	b := make([]byte, n)
	if _, err := r.Read(b); err != nil {
		return "", err
	}
	return string(b), nil
}

// PeekCodecID reads only the ID without validating the CRC, useful when a
// corrupted payload must still be localized.
func PeekCodecID(data []byte) string {
	if len(data) < 7 || !bytes.Equal(data[:4], mediumMagic[:]) {
		return ""
	}
	r := bytes.NewReader(data)
	_, _ = r.Seek(5, 0)
	id, _ := readCodecString(r)
	return id
}

func FlipPayloadByte(data []byte, offset int) []byte {
	out := append([]byte(nil), data...)
	if offset < 0 {
		offset = len(out) / 2
	}
	if offset >= len(out)-4 {
		offset = len(out) - 8
	}
	if offset < 5 {
		offset = 5
	}
	out[offset] ^= 0xff
	return out
}
