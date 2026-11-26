package sqlitefile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

// RawValue is a decoded SQLite record value.
type RawValue struct {
	Text   string
	Int64  int64
	IsText bool
	IsInt  bool
}

// ReadKVFile reads all k/v rows from a checkpoint SQLite file.
func ReadKVFile(path string) ([]Row, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ReadKV(data)
}

// ReadKV parses the minimal records table.
func ReadKV(data []byte) ([]Row, error) {
	if len(data) < 100 || !bytes.Equal(data[:16], []byte("SQLite format 3\x00")) {
		return nil, ErrNotSQLite
	}
	pageSize := int(binary.BigEndian.Uint16(data[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize != PageSize {
		return nil, fmt.Errorf("%w: reader supports 65536-byte checkpoint pages, got %d", ErrUnsupportedPageSize, pageSize)
	}
	if len(data) < 2*pageSize {
		return nil, fmt.Errorf("%w: database shorter than data root page", ErrTruncatedRecord)
	}

	root := data[pageSize : 2*pageSize]
	pageNumbers := []uint32{}
	switch root[0] {
	case 0x0d:
		pageNumbers = []uint32{2}
	case 0x05:
		count := int(binary.BigEndian.Uint16(root[3:5]))
		for i := 0; i < count; i++ {
			pointer := int(binary.BigEndian.Uint16(root[12+i*2 : 14+i*2]))
			child := binary.BigEndian.Uint32(root[pointer : pointer+4])
			pageNumbers = append(pageNumbers, child)
		}
		pageNumbers = append(pageNumbers, binary.BigEndian.Uint32(root[8:12]))
	default:
		return nil, fmt.Errorf("expected table btree page type 0x0d/0x05, got 0x%02x", root[0])
	}

	rows := make([]Row, 0)
	for _, pageNo := range pageNumbers {
		start := int(pageNo-1) * pageSize
		end := start + pageSize
		if pageNo < 1 || end > len(data) {
			return nil, fmt.Errorf("btree child page %d is outside database", pageNo)
		}
		pageRows, err := readLeafPage(data[start:end])
		if err != nil {
			return nil, err
		}
		rows = append(rows, pageRows...)
	}
	return rows, nil
}

func readLeafPage(page []byte) ([]Row, error) {
	if page[0] != 0x0d {
		return nil, fmt.Errorf("expected table leaf page type 0x0d, got 0x%02x", page[0])
	}
	count := int(binary.BigEndian.Uint16(page[3:5]))
	rows := make([]Row, 0, count)
	for i := 0; i < count; i++ {
		pointer := int(binary.BigEndian.Uint16(page[8+i*2 : 10+i*2]))
		payloadLen, n, err := DecodeVarint(page, pointer)
		if err != nil {
			return nil, err
		}
		rowid, m, err := DecodeVarint(page, pointer+n)
		if err != nil {
			return nil, err
		}
		payloadStart := pointer + n + m
		payload := page[payloadStart : payloadStart+int(payloadLen)]
		values, err := DecodeRecord(payload)
		if err != nil {
			return nil, fmt.Errorf("rowid %d: %w", rowid, err)
		}
		if len(values) < 2 || !values[0].IsText || !values[1].IsText {
			return nil, fmt.Errorf("rowid %d is not a k/v text record", rowid)
		}
		_ = rowid
		rows = append(rows, Row{Key: values[0].Text, Value: values[1].Text})
	}
	return rows, nil
}

// DecodeRecord decodes serial types and values from a record payload.
func DecodeRecord(record []byte) ([]RawValue, error) {
	headerLength, n, err := DecodeVarint(record, 0)
	if err != nil {
		return nil, err
	}
	serialOffset := n
	serials := []uint64{}
	for uint64(serialOffset) < headerLength {
		serial, size, err := DecodeVarint(record, serialOffset)
		if err != nil {
			return nil, err
		}
		serials = append(serials, serial)
		serialOffset += size
	}

	bodyOffset := int(headerLength)
	values := make([]RawValue, 0, len(serials))
	for _, serial := range serials {
		switch {
		case serial == 0:
			values = append(values, RawValue{})
		case serial >= 1 && serial <= 6:
			size := map[uint64]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8}[serial]
			if bodyOffset+size > len(record) {
				return nil, ErrTruncatedRecord
			}
			var v int64
			for _, b := range record[bodyOffset : bodyOffset+size] {
				v = (v << 8) | int64(b)
			}
			if size < 8 && v&(1<<(size*8-1)) != 0 {
				v -= 1 << (size * 8)
			}
			values = append(values, RawValue{Int64: v, IsInt: true})
			bodyOffset += size
		case serial == 7:
			return nil, fmt.Errorf("float serial type 7 is not used by checkpoints")
		case serial == 8:
			values = append(values, RawValue{Int64: 0, IsInt: true})
		case serial == 9:
			values = append(values, RawValue{Int64: 1, IsInt: true})
		case serial >= 12 && serial%2 == 0:
			size := int((serial - 12) / 2)
			bodyOffset += size
		case serial >= 13 && serial%2 == 1:
			size := int((serial - 13) / 2)
			if bodyOffset+size > len(record) {
				return nil, ErrTruncatedRecord
			}
			values = append(values, RawValue{Text: string(record[bodyOffset : bodyOffset+size]), IsText: true})
			bodyOffset += size
		default:
			return nil, fmt.Errorf("unsupported serial type %d", serial)
		}
	}
	return values, nil
}
