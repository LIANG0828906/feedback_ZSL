package colortrace

import (
	"encoding/binary"
	"fmt"
)

var textureMagic = [4]byte{'C', 'T', 'X', '1'}

// EncodePlane serializes a plane in a tiny deterministic little-endian format.
func EncodePlane(plane *Plane) ([]byte, error) {
	if plane == nil {
		return nil, schemaError("plane", "nil plane")
	}
	name := []byte(plane.Name)
	if len(name) > 255 {
		return nil, schemaError("plane.name", "name too long")
	}
	data := make([]byte, 0, 12+len(name)+8*len(plane.Values))
	data = append(data, textureMagic[:]...)
	data = append(data, byte(len(name)))
	data = append(data, name...)
	data = binary.LittleEndian.AppendUint16(data, uint16(plane.Width))
	data = binary.LittleEndian.AppendUint16(data, uint16(plane.Height))
	data = binary.LittleEndian.AppendUint16(data, uint16(0))
	for _, value := range plane.Values {
		data = binary.LittleEndian.AppendUint32(data, uint32(value))
	}
	return data, nil
}

// DecodePlane parses EncodePlane output. Any short read is E_DECODE.
func DecodePlane(data []byte) (*Plane, error) {
	if len(data) < 4 || data[0] != textureMagic[0] || data[1] != textureMagic[1] || data[2] != textureMagic[2] || data[3] != textureMagic[3] {
		return nil, &Error{Code: EDecode, Path: "$.magic", Message: "bad texture magic or truncated header"}
	}
	pos := 4
	if pos >= len(data) {
		return nil, decodeTrunc("$.name_len", pos)
	}
	nameLen := int(data[pos])
	pos++
	if pos+nameLen > len(data) {
		return nil, decodeTrunc("$.name", pos)
	}
	name := string(data[pos : pos+nameLen])
	pos += nameLen
	readU16 := func(field string) (int, error) {
		if pos+2 > len(data) {
			return 0, decodeTrunc("$."+field, pos)
		}
		value := int(binary.LittleEndian.Uint16(data[pos : pos+2]))
		pos += 2
		return value, nil
	}
	width, err := readU16("width")
	if err != nil {
		return nil, err
	}
	height, err := readU16("height")
	if err != nil {
		return nil, err
	}
	if _, err := readU16("reserved"); err != nil {
		return nil, err
	}
	expected := width * height
	values := make([]int64, 0, expected)
	for i := 0; i < expected; i++ {
		if pos+4 > len(data) {
			return nil, decodeTrunc(fmt.Sprintf("$.values[%d]", i), pos)
		}
		values = append(values, int64(binary.LittleEndian.Uint32(data[pos:pos+4])))
		pos += 4
	}
	if pos != len(data) {
		return nil, &Error{Code: EDecode, Path: "$.trailing_bytes", Message: fmt.Sprintf("%d unexpected trailing bytes", len(data)-pos)}
	}
	return &Plane{PlaneDesc: PlaneDesc{Name: name, Width: width, Height: height}, Values: values}, nil
}

func decodeTrunc(path string, offset int) error {
	return &Error{Code: EDecode, Path: path, Message: fmt.Sprintf("truncated texture at byte offset %d", offset)}
}
