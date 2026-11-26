package kernel

import (
	"encoding/binary"
	"math"
)

const sqliteMagic = "SQLite format 3\x00"
const sqlitePageSize = 65536

type SQLiteTable struct {
	Name string
	SQL  string
	Rows [][]any
}

// BuildNativeSQLite writes a small valid SQLite 3 database using only the
// standard library. Checkpoint rows are deliberately small and use one b-tree
// leaf page per table; the meta table carries the complete JSON bundle.
func BuildNativeSQLite(tables []SQLiteTable) ([]byte, error) {
	pageSize := sqlitePageSize
	numPages := 1 + len(tables)
	data := make([]byte, pageSize*numPages)
	copy(data[0:16], sqliteMagic)
	binary.BigEndian.PutUint16(data[16:18], 1) // 1 means 65536.
	data[18] = 1
	data[19] = 1
	data[20] = 0
	data[21] = 64
	data[22] = 32
	data[23] = 32
	binary.BigEndian.PutUint32(data[24:28], 1)
	binary.BigEndian.PutUint32(data[28:32], uint32(numPages))
	binary.BigEndian.PutUint32(data[32:36], 0)
	binary.BigEndian.PutUint32(data[36:40], 0)
	binary.BigEndian.PutUint32(data[40:44], 1)
	binary.BigEndian.PutUint32(data[44:48], 4)
	binary.BigEndian.PutUint32(data[48:52], 0)
	binary.BigEndian.PutUint32(data[52:56], 0)
	binary.BigEndian.PutUint32(data[56:60], 1)
	binary.BigEndian.PutUint32(data[60:64], 0)
	binary.BigEndian.PutUint32(data[64:68], 0)
	binary.BigEndian.PutUint32(data[68:72], 0)
	binary.BigEndian.PutUint32(data[92:96], 1)
	binary.BigEndian.PutUint32(data[96:100], 3046001)

	masterRows := make([][]any, 0, len(tables))
	for i, table := range tables {
		rootPage := int64(i + 2)
		masterRows = append(masterRows, []any{"table", table.Name, table.Name, rootPage, table.SQL})
		pageStart := (i + 1) * pageSize
		page := data[pageStart : pageStart+pageSize]
		if err := encodeLeafPage(page, 0, table.Rows); err != nil {
			return nil, err.WithDetail("table", table.Name)
		}
	}
	if err := encodeLeafPage(data[0:pageSize], 100, masterRows); err != nil {
		return nil, err.WithDetail("table", "sqlite_master")
	}
	return data, nil
}

func encodeLeafPage(page []byte, headerOffset int, rows [][]any) *BizError {
	cells := make([][]byte, 0, len(rows))
	for i, row := range rows {
		record, err := encodeRecord(row)
		if err != nil {
			return err.WithDetail("row", i)
		}
		cell := appendSQLiteVarint(nil, uint64(len(record)))
		cell = appendSQLiteVarint(cell, uint64(i+1))
		cell = append(cell, record...)
		cells = append(cells, cell)
	}

	pointerStart := headerOffset + 8
	pointerEnd := pointerStart + 2*len(cells)
	cursor := len(page)
	for _, cell := range cells {
		cursor -= len(cell)
		if cursor < pointerEnd {
			return NewBizError(CodeCheckpointCorrupt, "SQLite leaf page is full; checkpoint bundle is too large")
		}
		copy(page[cursor:cursor+len(cell)], cell)
	}
	page[headerOffset] = 0x0d
	page[headerOffset+1] = 0
	page[headerOffset+2] = 0
	binary.BigEndian.PutUint16(page[headerOffset+3:headerOffset+5], uint16(len(cells)))
	binary.BigEndian.PutUint16(page[headerOffset+5:headerOffset+7], uint16(cursor))
	page[headerOffset+7] = 0
	for i := range cells {
		binary.BigEndian.PutUint16(page[pointerStart+2*i:pointerStart+2*i+2], uint16(cursor))
		// Recover each cell start independently because cursor advanced upward.
	}
	// The loop above wrote pointers before cell starts were known in the final
	// order; rewrite pointers by scanning our placement sequence.
	place := len(page)
	for i, cell := range cells {
		place -= len(cell)
		binary.BigEndian.PutUint16(page[pointerStart+2*i:pointerStart+2*i+2], uint16(place))
	}
	return nil
}

func encodeRecord(values []any) ([]byte, *BizError) {
	var serials []byte
	var body []byte
	for _, value := range values {
		switch v := value.(type) {
		case nil:
			serials = appendSQLiteVarint(serials, 0)
		case string:
			raw := []byte(v)
			serials = appendSQLiteVarint(serials, uint64(13+2*len(raw)))
			body = append(body, raw...)
		case ID:
			raw := []byte(v)
			serials = appendSQLiteVarint(serials, uint64(13+2*len(raw)))
			body = append(body, raw...)
		case int:
			serials = appendSQLiteVarint(serials, 6)
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], uint64(int64(v)))
			body = append(body, buf[:]...)
		case int64:
			serials = appendSQLiteVarint(serials, 6)
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], uint64(v))
			body = append(body, buf[:]...)
		default:
			return nil, NewBizError(CodeCheckpointCorrupt, "unsupported SQLite value type %T", value)
		}
	}
	serialLen := len(serials)
	headerLen := serialLen + 1
	if n := sqliteVarintLen(uint64(headerLen)); n > 1 {
		headerLen = serialLen + n
		if n2 := sqliteVarintLen(uint64(headerLen)); n2 != n {
			headerLen = serialLen + n2
		}
	}
	header := appendSQLiteVarint(nil, uint64(headerLen))
	if len(header) > 1 {
		// Recompute with the actual length-of-length.
		headerLen = serialLen + len(header)
		header = appendSQLiteVarint(nil, uint64(headerLen))
	}
	if len(header)+len(serials) != headerLen {
		headerLen = len(header) + serialLen
		header = appendSQLiteVarint(nil, uint64(headerLen))
	}
	record := append(header, serials...)
	record = append(record, body...)
	return record, nil
}

func appendSQLiteVarint(dst []byte, value uint64) []byte {
	var groups []byte
	for {
		groups = append(groups, byte(value&0x7f))
		value >>= 7
		if value == 0 {
			break
		}
	}
	for i := len(groups) - 1; i >= 0; i-- {
		b := groups[i]
		if i != 0 {
			b |= 0x80
		}
		dst = append(dst, b)
	}
	return dst
}

func sqliteVarintLen(v uint64) int {
	n := 0
	for {
		n++
		if v < 128 {
			return n
		}
		v >>= 7
	}
}

func readSQLiteVarint(buf []byte, offset int) (uint64, int, error) {
	var value uint64
	start := offset
	for i := 0; i < 9; i++ {
		if offset >= len(buf) {
			return 0, offset, NewBizError(CodeCheckpointTruncated, "varint at %d is truncated", start)
		}
		b := buf[offset]
		offset++
		if i == 8 {
			value = (value << 8) | uint64(b)
			return value, offset, nil
		}
		value = (value << 7) | uint64(b&0x7f)
		if b&0x80 == 0 {
			return value, offset, nil
		}
	}
	return value, offset, nil
}

func ReadMetaBundle(data []byte, pageSize, rootPage int) (string, error) {
	if rootPage < 1 {
		return "", NewBizError(CodeCheckpointCorrupt, "invalid meta root page %d", rootPage)
	}
	pageStart := (rootPage - 1) * pageSize
	if pageStart+8 > len(data) {
		return "", NewBizError(CodeCheckpointTruncated, "meta b-tree header is truncated")
	}
	page := data[pageStart:]
	if page[0] != 0x0d {
		return "", NewBizError(CodeCheckpointCorrupt, "meta page type 0x%02x is not table leaf", page[0])
	}
	cellCount := int(binary.BigEndian.Uint16(page[3:5]))
	pointerStart := 8
	for i := 0; i < cellCount; i++ {
		ptrOffset := pointerStart + 2*i
		if ptrOffset+2 > len(page) {
			return "", NewBizError(CodeCheckpointTruncated, "cell pointer %d is truncated", i)
		}
		cellPtr := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
		if cellPtr <= 0 || cellPtr >= pageSize || pageStart+cellPtr >= len(data) {
			return "", NewBizError(CodeCheckpointTruncated, "cell %d pointer %d is outside file", i, cellPtr)
		}
		values, err := readRecordAt(data, pageStart+cellPtr)
		if err != nil {
			return "", err
		}
		if len(values) >= 2 && values[0] == "bundle_json" {
			if bundle, ok := values[1].(string); ok {
				return bundle, nil
			}
		}
	}
	return "", NewBizError(CodeCheckpointCorrupt, "meta bundle_json row not found")
}

func readRecordAt(data []byte, offset int) ([]any, error) {
	payloadLen, p, err := readSQLiteVarint(data, offset)
	if err != nil {
		return nil, err
	}
	_, p, err = readSQLiteVarint(data, p) // rowid
	if err != nil {
		return nil, err
	}
	recordStart := p
	if recordStart >= len(data) {
		return nil, NewBizError(CodeCheckpointTruncated, "record start is beyond file")
	}
	headerLen, afterHeaderLen, err := readSQLiteVarint(data, recordStart)
	if err != nil {
		return nil, err
	}
	headerEnd := recordStart + int(headerLen)
	if headerEnd > len(data) {
		return nil, NewBizError(CodeCheckpointTruncated, "record header length %d is truncated", headerLen)
	}
	serialOffset := afterHeaderLen
	serials := []uint64{}
	for serialOffset < headerEnd {
		serial, next, err := readSQLiteVarint(data, serialOffset)
		if err != nil {
			return nil, err
		}
		serials = append(serials, serial)
		serialOffset = next
	}
	bodyOffset := headerEnd
	values := make([]any, 0, len(serials))
	for _, serial := range serials {
		value, next, err := readSerialValue(data, bodyOffset, serial)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		bodyOffset = next
	}
	_ = payloadLen
	return values, nil
}

func readSerialValue(data []byte, offset int, serial uint64) (any, int, error) {
	switch {
	case serial == 0:
		return nil, offset, nil
	case serial == 8:
		return int64(0), offset, nil
	case serial == 9:
		return int64(1), offset, nil
	case serial >= 1 && serial <= 6:
		widths := map[uint64]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8}
		n := widths[serial]
		if offset+n > len(data) {
			return nil, offset, NewBizError(CodeCheckpointTruncated, "integer serial %d at %d truncated", serial, offset)
		}
		var v int64
		for i := 0; i < n; i++ {
			v = (v << 8) | int64(data[offset+i])
		}
		// Sign extend.
		bits := uint(n * 8)
		if bits < 64 {
			signBit := int64(1) << (bits - 1)
			v = (v ^ signBit) - signBit
		}
		return v, offset + n, nil
	case serial == 7:
		if offset+8 > len(data) {
			return nil, offset, NewBizError(CodeCheckpointTruncated, "float value truncated")
		}
		bits := binary.BigEndian.Uint64(data[offset : offset+8])
		return float64frombits(bits), offset + 8, nil
	case serial >= 12 && serial%2 == 0:
		n := int((serial - 12) / 2)
		if offset+n > len(data) {
			return nil, offset, NewBizError(CodeCheckpointTruncated, "blob value truncated")
		}
		return append([]byte(nil), data[offset:offset+n]...), offset + n, nil
	case serial >= 13 && serial%2 == 1:
		n := int((serial - 13) / 2)
		if offset+n > len(data) {
			return nil, offset, NewBizError(CodeCheckpointTruncated, "text value truncated")
		}
		return string(data[offset : offset+n]), offset + n, nil
	default:
		return nil, offset, NewBizError(CodeCheckpointCorrupt, "unsupported SQLite serial type %d", serial)
	}
}

func float64frombits(b uint64) float64 {
	return math.Float64frombits(b)
}
