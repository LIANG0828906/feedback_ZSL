// Package sqlitelite writes and reads the tiny SQLite database used for
// checkpoint persistence. It intentionally supports only one table-leaf-page
// schema with text, integer and null values. The resulting file is a valid
// SQLite 3 database that can be inspected with ordinary SQLite tooling.
package sqlitelite

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
)

var (
	ErrTruncated = errors.New("sqlite file truncated")
	ErrCorrupt   = errors.New("sqlite file corrupt")
	ErrTooLarge  = errors.New("sqlite row too large")
)

const (
	pageSize = 32768
	table    = "checkpoint"
	tableSQL = "CREATE TABLE checkpoint(kind TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,payload TEXT NOT NULL,checksum TEXT NOT NULL)"
)

type Row struct {
	RowID  int64
	Values []any
}

// ReadCheckpoint returns all rows from the checkpoint table.
func ReadCheckpoint(path string) ([]Row, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseDatabase(raw)
}

// WriteCheckpoint atomically writes a complete database containing the rows.
func WriteCheckpoint(path string, rows []Row) error {
	if len(rows) == 0 {
		return ErrTruncated
	}
	raw, err := buildDatabase(rows)
	if err != nil {
		return err
	}
	return atomicWrite(path, raw)
}

func buildDatabase(rows []Row) ([]byte, error) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].RowID < rows[j].RowID })
	for i := 1; i < len(rows); i++ {
		if rows[i].RowID == rows[i-1].RowID {
			return nil, fmt.Errorf("%w: duplicate rowid", ErrCorrupt)
		}
	}
	schemaCell, err := tableLeafCell(1, []any{
		"table", table, table, int64(2), tableSQL,
	})
	if err != nil {
		return nil, err
	}
	dataCells := make([][]byte, 0, len(rows))
	for _, row := range rows {
		if len(row.Values) != 5 {
			return nil, fmt.Errorf("%w: checkpoint row needs 5 values", ErrCorrupt)
		}
		cell, err := tableLeafCell(row.RowID, row.Values)
		if err != nil {
			return nil, err
		}
		dataCells = append(dataCells, cell)
	}
	page1, err := buildLeafPage(1, [][]byte{schemaCell}, true)
	if err != nil {
		return nil, err
	}
	page2, err := buildLeafPage(2, dataCells, false)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 100)
	copy(header[0:16], []byte("SQLite format 3\x00"))
	binary.BigEndian.PutUint16(header[16:18], pageSize)
	header[18] = 1
	header[19] = 1
	header[20] = 0
	header[21] = 64
	header[22] = 32
	header[23] = 32
	binary.BigEndian.PutUint32(header[24:28], 1)
	binary.BigEndian.PutUint32(header[28:32], 2)
	binary.BigEndian.PutUint32(header[40:44], 1)
	binary.BigEndian.PutUint32(header[44:48], 4)
	binary.BigEndian.PutUint32(header[52:56], 2)
	binary.BigEndian.PutUint32(header[56:60], 1)
	binary.BigEndian.PutUint32(header[92:96], 1)
	binary.BigEndian.PutUint32(header[96:100], 3046001)
	copy(page1[:100], header)
	raw := make([]byte, 0, pageSize*2)
	raw = append(raw, page1...)
	raw = append(raw, page2...)
	if len(raw) < 100 || binary.BigEndian.Uint32(raw[28:32]) != 2 {
		return nil, ErrCorrupt
	}
	return raw, nil
}

func tableLeafCell(rowID int64, values []any) ([]byte, error) {
	record, err := encodeRecord(values)
	if err != nil {
		return nil, err
	}
	if len(record) > pageSize-35 {
		return nil, ErrTooLarge
	}
	cell := make([]byte, 0, len(record)+20)
	cell = append(cell, encodeVarint(int64(len(record)))...)
	cell = append(cell, encodeVarint(rowID)...)
	cell = append(cell, record...)
	return cell, nil
}

func encodeRecord(values []any) ([]byte, error) {
	var types []byte
	var body []byte
	for _, value := range values {
		switch v := value.(type) {
		case nil:
			types = append(types, encodeVarint(0)...)
		case string:
			serial := int64(13 + 2*len(v))
			types = append(types, encodeVarint(serial)...)
			body = append(body, []byte(v)...)
		case int:
			body = append(body, encodeInt64(int64(v))...)
			types = append(types, encodeVarint(6)...)
		case int64:
			body = append(body, encodeInt64(v)...)
			types = append(types, encodeVarint(6)...)
		case uint32:
			body = append(body, encodeInt64(int64(v))...)
			types = append(types, encodeVarint(6)...)
		default:
			return nil, fmt.Errorf("%w: unsupported SQLite value %T", ErrCorrupt, value)
		}
	}
	// The header-size varint is part of the header. Recompute until stable if
	// the length crosses a varint boundary (not expected for this schema, but
	// deterministic for all supported inputs).
	headerSize := 1 + len(types)
	for {
		encoded := encodeVarint(int64(headerSize))
		if headerSize == len(encoded)+len(types) {
			break
		}
		headerSize = len(encoded) + len(types)
	}
	header := append(encodeVarint(int64(headerSize)), types...)
	return append(header, body...), nil
}

func encodeInt64(v int64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, uint64(v))
	return out
}

func encodeVarint(v int64) []byte {
	if v < 0 {
		panic("negative varint")
	}
	if v > 0x00ffffffffffffff {
		out := make([]byte, 9)
		out[8] = byte(v & 0xff)
		v >>= 8
		for i := 7; i >= 0; i-- {
			out[i] = byte(v&0x7f) | 0x80
			v >>= 7
		}
		return out
	}
	tmp := make([]byte, 0, 8)
	for {
		tmp = append(tmp, byte(v&0x7f))
		v >>= 7
		if v == 0 {
			break
		}
	}
	out := make([]byte, len(tmp))
	for i := range tmp {
		pos := len(tmp) - 1 - i
		b := tmp[i]
		if i > 0 {
			b |= 0x80
		}
		out[pos] = b
	}
	return out
}

func buildLeafPage(pageNumber int, cells [][]byte, pageOne bool) ([]byte, error) {
	page := make([]byte, pageSize)
	headerOffset := 0
	usableStart := 0
	if pageOne {
		headerOffset = 100
		usableStart = 100
	}
	if len(cells) > math.MaxUint16 {
		return nil, ErrTooLarge
	}
	content := pageSize
	offsets := make([]int, len(cells))
	for i, cell := range cells {
		content -= len(cell)
		if content < usableStart+8+2*len(cells) {
			return nil, ErrTooLarge
		}
		copy(page[content:], cell)
		offsets[i] = content
	}
	page[headerOffset] = 0x0d
	binary.BigEndian.PutUint16(page[headerOffset+1:headerOffset+3], 0)
	binary.BigEndian.PutUint16(page[headerOffset+3:headerOffset+5], uint16(len(cells)))
	cstart := content
	if cstart == 65536 {
		cstart = 0
	}
	binary.BigEndian.PutUint16(page[headerOffset+5:headerOffset+7], uint16(cstart))
	page[headerOffset+7] = 0
	ptr := headerOffset + 8
	for _, off := range offsets {
		binary.BigEndian.PutUint16(page[ptr:ptr+2], uint16(off))
		ptr += 2
	}
	_ = pageNumber
	return page, nil
}

func parseDatabase(raw []byte) ([]Row, error) {
	if len(raw) < 100 {
		return nil, ErrTruncated
	}
	if !bytes.Equal(raw[:16], []byte("SQLite format 3\x00")) {
		return nil, ErrCorrupt
	}
	ps := int(binary.BigEndian.Uint16(raw[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps > 65536 || ps&(ps-1) != 0 {
		return nil, ErrCorrupt
	}
	pages := int(binary.BigEndian.Uint32(raw[28:32]))
	if pages <= 0 {
		return nil, ErrCorrupt
	}
	if len(raw) < pages*ps {
		return nil, ErrTruncated
	}
	if len(raw) > pages*ps {
		return nil, ErrCorrupt
	}
	if raw[56] != 0 && raw[56] != 1 {
		return nil, ErrCorrupt
	}
	schemaRows, err := readLeafPage(raw, ps, 1, true)
	if err != nil {
		return nil, err
	}
	root := int64(0)
	for _, row := range schemaRows {
		if len(row.Values) < 5 {
			continue
		}
		name, _ := row.Values[1].(string)
		tbl, _ := row.Values[2].(string)
		if name == table && tbl == table {
			rootValue := row.Values[3]
			switch v := rootValue.(type) {
			case int64:
				root = v
			case int:
				root = int64(v)
			}
		}
	}
	if root <= 0 {
		return nil, errors.New("checkpoint table missing")
	}
	rows, err := readLeafPage(raw, ps, int(root), false)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if len(row.Values) != 5 {
			return nil, ErrCorrupt
		}
	}
	return rows, nil
}

func readLeafPage(raw []byte, ps, pageNumber int, pageOne bool) ([]Row, error) {
	if pageNumber <= 0 {
		return nil, ErrCorrupt
	}
	start := (pageNumber - 1) * ps
	if start+ps > len(raw) {
		return nil, ErrTruncated
	}
	headerOffset := 0
	if pageOne {
		headerOffset = 100
	}
	page := raw[start : start+ps]
	if page[headerOffset] != 0x0d {
		return nil, ErrCorrupt
	}
	cellCount := int(binary.BigEndian.Uint16(page[headerOffset+3 : headerOffset+5]))
	if cellCount > (ps-headerOffset-8)/2 {
		return nil, ErrCorrupt
	}
	var rows []Row
	lastRowID := int64(-1)
	for i := 0; i < cellCount; i++ {
		ptr := headerOffset + 8 + i*2
		off := int(binary.BigEndian.Uint16(page[ptr : ptr+2]))
		if off < headerOffset+8 || off >= ps {
			return nil, ErrCorrupt
		}
		payloadLen, n, ok := readVarint(page[off:])
		if !ok {
			return nil, ErrCorrupt
		}
		rowID, m, ok := readVarint(page[off+n:])
		if !ok {
			return nil, ErrCorrupt
		}
		bodyStart := off + n + m
		if payloadLen > int64(ps-35) || int64(bodyStart)+payloadLen > int64(ps) {
			return nil, ErrCorrupt
		}
		record := page[bodyStart : bodyStart+int(payloadLen)]
		values, err := decodeRecord(record)
		if err != nil {
			return nil, err
		}
		if rowID <= lastRowID {
			return nil, ErrCorrupt
		}
		lastRowID = rowID
		rows = append(rows, Row{RowID: rowID, Values: values})
	}
	return rows, nil
}

func readVarint(data []byte) (int64, int, bool) {
	if len(data) == 0 {
		return 0, 0, false
	}
	var value uint64
	for i := 0; i < 9 && i < len(data); i++ {
		b := data[i]
		if i == 8 {
			value = value<<8 | uint64(b)
			return int64(value), 9, true
		}
		value = value<<7 | uint64(b&0x7f)
		if b&0x80 == 0 {
			return int64(value), i + 1, true
		}
	}
	return 0, 0, false
}

func decodeRecord(record []byte) ([]any, error) {
	headerSize, n, ok := readVarint(record)
	if !ok || headerSize < int64(n) || headerSize > int64(len(record)) {
		return nil, ErrCorrupt
	}
	header := record[n:headerSize]
	var serials []int64
	for pos := 0; pos < len(header); {
		serial, used, ok := readVarint(header[pos:])
		if !ok {
			return nil, ErrCorrupt
		}
		serials = append(serials, serial)
		pos += used
	}
	body := record[headerSize:]
	values := make([]any, 0, len(serials))
	cursor := 0
	for _, serial := range serials {
		size, kind, err := serialSize(serial)
		if err != nil {
			return nil, err
		}
		if cursor+size > len(body) {
			return nil, ErrCorrupt
		}
		chunk := body[cursor : cursor+size]
		cursor += size
		switch kind {
		case "null":
			values = append(values, nil)
		case "int":
			values = append(values, decodeSigned(chunk))
		case "string":
			values = append(values, string(chunk))
		case "blob":
			values = append(values, append([]byte(nil), chunk...))
		default:
			return nil, ErrCorrupt
		}
	}
	if cursor != len(body) {
		return nil, ErrCorrupt
	}
	return values, nil
}

func serialSize(serial int64) (int, string, error) {
	switch serial {
	case 0:
		return 0, "null", nil
	case 1:
		return 1, "int", nil
	case 2:
		return 2, "int", nil
	case 3:
		return 3, "int", nil
	case 4:
		return 4, "int", nil
	case 5:
		return 6, "int", nil
	case 6:
		return 8, "int", nil
	case 8:
		return 0, "int", nil
	case 9:
		return 0, "int", nil
	}
	if serial >= 12 && (serial-12)%2 == 0 {
		return int((serial - 12) / 2), "blob", nil
	}
	if serial >= 13 {
		return int((serial - 13) / 2), "string", nil
	}
	return 0, "", ErrCorrupt
}

func decodeSigned(chunk []byte) int64 {
	var value uint64
	for _, b := range chunk {
		value = value<<8 | uint64(b)
	}
	shift := uint(64 - 8*len(chunk))
	return int64(value<<shift) >> shift
}

func atomicWrite(path string, data []byte) error {
	dir := "."
	if idx := lastSlash(path); idx >= 0 {
		dir = path[:idx]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".checkpoint-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func lastSlash(path string) int {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return i
		}
	}
	return -1
}
