package cachebench

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// SQLiteColumn is the tiny schema description needed by the checkpoint writer.
type SQLiteColumn struct {
	Name string
	Type string // INTEGER or TEXT
}

// SQLiteTable is a single root-page table. Checkpoints intentionally keep only
// the latest committed node/window rows, so one leaf page per table is enough
// for the local sample and moderate integrations.
type SQLiteTable struct {
	Name    string
	Columns []SQLiteColumn
	Rows    [][]any
}

const sqlitePageSize = 4096

// WriteSQLiteFile writes a valid small SQLite database using only the standard
// library. The file is created next to path, fsynced and atomically renamed.
func WriteSQLiteFile(path string, tables []SQLiteTable) error {
	pageCount := 1 + len(tables)
	pages := make([][]byte, pageCount)

	schemaRows := make([][]any, 0, len(tables))
	for i, t := range tables {
		rootPage := int64(i + 2)
		ddl := createTableSQL(t)
		schemaRows = append(schemaRows, []any{"table", t.Name, t.Name, rootPage, ddl})
		page, err := encodeLeafPage(sqlitePageSize, 0, t.Rows)
		if err != nil {
			return err
		}
		pages[i+1] = page
	}
	schemaPage, err := encodeLeafPage(sqlitePageSize, 100, schemaRows)
	if err != nil {
		return err
	}
	pages[0] = schemaPage
	applyDatabaseHeader(pages[0], uint32(pageCount))

	if err := os.MkdirAll(filepath.Dir(pathOrDot(path)), 0o755); err != nil {
		return NewError(CodeInvalidArgument, "create checkpoint directory: %v", err).WithField("path")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path+"."), ".tmp-checkpoint-*")
	if err != nil {
		return NewError(CodeInvalidArgument, "create temporary checkpoint: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	for _, p := range pages {
		if _, err := tmp.Write(p); err != nil {
			_ = tmp.Close()
			return NewError(CodeInvalidArgument, "write sqlite page: %v", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return NewError(CodeInvalidArgument, "fsync checkpoint: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return NewError(CodeInvalidArgument, "close checkpoint temp: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return NewError(CodeInvalidArgument, "atomic rename checkpoint: %v", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func pathOrDot(p string) string {
	if p == "" {
		return "."
	}
	return p
}

func createTableSQL(t SQLiteTable) string {
	s := "CREATE TABLE " + t.Name + " ("
	for i, c := range t.Columns {
		if i > 0 {
			s += ", "
		}
		typ := c.Type
		if typ == "" {
			typ = "BLOB"
		}
		s += c.Name + " " + typ
	}
	return s + ")"
}

func applyDatabaseHeader(page []byte, pageCount uint32) {
	copy(page[0:16], []byte("SQLite format 3\x00"))
	binary.BigEndian.PutUint16(page[16:18], sqlitePageSize)
	page[18] = 1
	page[19] = 1
	page[20] = 0
	page[21] = 64
	page[22] = 32
	page[23] = 32
	binary.BigEndian.PutUint32(page[24:28], 1)
	binary.BigEndian.PutUint32(page[28:32], pageCount)
	binary.BigEndian.PutUint32(page[32:36], 0)
	binary.BigEndian.PutUint32(page[36:40], 0)
	binary.BigEndian.PutUint32(page[40:44], 1)
	binary.BigEndian.PutUint32(page[44:48], 4)
	binary.BigEndian.PutUint32(page[48:52], 0)
	binary.BigEndian.PutUint32(page[52:56], 0)
	binary.BigEndian.PutUint32(page[56:60], 1)
	binary.BigEndian.PutUint32(page[60:64], 0)
	binary.BigEndian.PutUint32(page[64:68], 0)
	binary.BigEndian.PutUint32(page[92:96], 3045000)
	binary.BigEndian.PutUint32(page[96:100], 3045000)
}

func encodeLeafPage(pageSize, headerOffset int, rows [][]any) ([]byte, error) {
	page := make([]byte, pageSize)
	header := headerOffset
	cellPtrs := header + 8
	cellArea := pageSize
	encodedCells := make([][]byte, 0, len(rows))
	for rowID, row := range rows {
		record, err := encodeRecord(row)
		if err != nil {
			return nil, err
		}
		cell := make([]byte, 0, binary.MaxVarintLen64*2+len(record))
		cell = appendUvarint(cell, uint64(len(record)))
		cell = appendUvarint(cell, uint64(rowID+1))
		cell = append(cell, record...)
		if cellArea-len(cell) < cellPtrs+2*(rowID+1) {
			return nil, NewError(CodeInvalidArgument, "sqlite checkpoint table exceeds one leaf page").
				WithContext("rows", fmt.Sprint(len(rows))).
				WithContext("page_size", fmt.Sprint(pageSize))
		}
		cellArea -= len(cell)
		copy(page[cellArea:], cell)
		encodedCells = append(encodedCells, cell)
		_ = encodedCells
		binary.BigEndian.PutUint16(page[cellPtrs+2*rowID:cellPtrs+2*rowID+2], uint16(cellArea))
	}
	page[header] = 0x0d
	binary.BigEndian.PutUint16(page[header+1:header+3], 0)
	binary.BigEndian.PutUint16(page[header+3:header+5], uint16(len(rows)))
	if len(rows) == 0 {
		binary.BigEndian.PutUint16(page[header+5:header+7], 0)
	} else {
		binary.BigEndian.PutUint16(page[header+5:header+7], uint16(cellArea))
	}
	page[header+7] = 0
	return page, nil
}

func appendUvarint(b []byte, v uint64) []byte {
	n := sqliteVarintLen(v)
	var buf [9]byte
	x := v
	for i := n - 1; i >= 0; i-- {
		if i == 8 {
			buf[i] = byte(x & 0xff)
			x >>= 8
		} else {
			buf[i] = byte(x & 0x7f)
			x >>= 7
		}
	}
	for i := 0; i < n-1 && i < 8; i++ {
		buf[i] |= 0x80
	}
	return append(b, buf[:n]...)
}

func encodeRecord(values []any) ([]byte, error) {
	serials := make([]uint64, len(values))
	bodies := make([][]byte, len(values))
	headerLen := 1
	for i, v := range values {
		switch x := v.(type) {
		case nil:
			serials[i] = 0
		case int:
			b, s := encodeInt64(int64(x))
			bodies[i] = b
			serials[i] = s
		case int64:
			b, s := encodeInt64(x)
			bodies[i] = b
			serials[i] = s
		case bool:
			if x {
				serials[i] = 9
			} else {
				serials[i] = 8
			}
		case string:
			bodies[i] = []byte(x)
			serials[i] = uint64(13 + 2*len(x))
		default:
			return nil, NewError(CodeInvalidArgument, "unsupported sqlite value type %T", v)
		}
		headerLen += uvarintLen(serials[i])
	}
	out := make([]byte, 0, headerLen)
	out = appendUvarint(out, uint64(headerLen))
	for _, s := range serials {
		out = appendUvarint(out, s)
	}
	for _, b := range bodies {
		out = append(out, b...)
	}
	return out, nil
}

func uvarintLen(v uint64) int {
	return sqliteVarintLen(v)
}

func sqliteVarintLen(v uint64) int {
	n := 1
	x := v
	for x >= 0x80 {
		x >>= 7
		n++
	}
	if n > 9 {
		return 9
	}
	return n
}

func readSQLiteVarint(b []byte) (uint64, int, *DomainError) {
	var v uint64
	for i := 0; i < len(b) && i < 9; i++ {
		c := b[i]
		if i == 8 {
			v = (v << 8) | uint64(c)
			return v, 9, nil
		}
		v = (v << 7) | uint64(c&0x7f)
		if c < 0x80 {
			return v, i + 1, nil
		}
	}
	return 0, 0, NewError(CodeCheckpointTruncated, "sqlite varint truncated")
}

func encodeInt64(v int64) ([]byte, uint64) {
	switch {
	case v >= -128 && v <= 127:
		return []byte{byte(v)}, 1
	case v >= -32768 && v <= 32767:
		b := make([]byte, 2)
		binary.BigEndian.PutUint16(b, uint16(v))
		return b, 2
	case v >= -8388608 && v <= 8388607:
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(v))
		return b[1:4], 3
	case v >= -(1<<31) && v <= (1<<31)-1:
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(v))
		return b, 4
	case v >= -(1<<47) && v <= (1<<47)-1:
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(v))
		return b[2:8], 5
	default:
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(v))
		return b, 6
	}
}

// ReadSQLiteTables reads table roots from sqlite_master and decodes their leaf
// pages. It supports the files written by WriteSQLiteFile.
func ReadSQLiteTables(path string) (map[string][][]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][][]any{}, nil
		}
		return nil, NewError(CodeInvalidArgument, "read checkpoint: %v", err)
	}
	if len(b) < 100 || string(b[0:15]) != "SQLite format 3" {
		return nil, NewError(CodeCheckpointTruncated, "checkpoint header is truncated or not sqlite").WithField("path")
	}
	pageSize := int(binary.BigEndian.Uint16(b[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize > 65536 {
		return nil, NewError(CodeSQLiteCorrupt, "invalid page size %d", pageSize)
	}
	pageCount := int(binary.BigEndian.Uint32(b[28:32]))
	if pageCount <= 0 || len(b) < pageSize*pageCount {
		return nil, NewError(CodeCheckpointTruncated, "file is shorter than committed page count").
			WithContext("bytes", fmt.Sprint(len(b))).
			WithContext("expected", fmt.Sprint(pageSize*pageCount))
	}
	masterRows, masterErr := readLeafRows(b, pageSize, 1, 100)
	if masterErr != nil {
		return nil, masterErr
	}
	out := map[string][][]any{}
	for _, row := range masterRows {
		if len(row) < 5 {
			continue
		}
		typ, _ := row[0].(string)
		name, _ := row[1].(string)
		root, _ := row[3].(int64)
		if typ != "table" || name == "" || root <= 0 {
			continue
		}
		rows, tableErr := readLeafRows(b, pageSize, int(root), 0)
		if tableErr != nil {
			return nil, tableErr.WithContext("table", name)
		}
		out[name] = rows
	}
	return out, nil
}

func readLeafRows(file []byte, pageSize, pageNo, headerOffsetInPage int) ([][]any, *DomainError) {
	start := (pageNo - 1) * pageSize
	if start < 0 || start+pageSize > len(file) {
		return nil, NewError(CodeCheckpointTruncated, "page %d is outside file", pageNo)
	}
	page := file[start : start+pageSize]
	h := headerOffsetInPage
	if h+8 > len(page) {
		return nil, NewError(CodeCheckpointTruncated, "leaf header truncated")
	}
	if page[h] != 0x0d {
		return nil, NewError(CodeSQLiteCorrupt, "unsupported sqlite page type 0x%02x (only leaf tables are supported)", page[h])
	}
	n := int(binary.BigEndian.Uint16(page[h+3 : h+5]))
	ptrStart := h + 8
	if ptrStart+2*n > len(page) {
		return nil, NewError(CodeCheckpointTruncated, "cell pointer array truncated")
	}
	rows := make([][]any, 0, n)
	for i := 0; i < n; i++ {
		cellOff := int(binary.BigEndian.Uint16(page[ptrStart+2*i : ptrStart+2*i+2]))
		if cellOff <= 0 || cellOff >= len(page) {
			return nil, NewError(CodeCheckpointTruncated, "cell offset out of range")
		}
		payloadLen, vn, verr := readSQLiteVarint(page[cellOff:])
		if verr != nil {
			return nil, verr
		}
		rowID, rn, rerr := readSQLiteVarint(page[cellOff+vn:])
		if rerr != nil {
			return nil, rerr
		}
		recordStart := cellOff + vn + rn
		if recordStart+int(payloadLen) > len(page) {
			return nil, NewError(CodeCheckpointTruncated, "cell payload truncated")
		}
		rec, err := decodeRecord(page[recordStart : recordStart+int(payloadLen)])
		if err != nil {
			return nil, err
		}
		_ = rowID
		rows = append(rows, rec)
	}
	return rows, nil
}

func decodeRecord(rec []byte) ([]any, *DomainError) {
	headerLen, n, err := readSQLiteVarint(rec)
	if err != nil || int(headerLen) > len(rec) {
		return nil, NewError(CodeCheckpointTruncated, "record header length invalid")
	}
	pos := n
	serials := []uint64{}
	for pos < int(headerLen) {
		s, sn, serr := readSQLiteVarint(rec[pos:])
		if serr != nil {
			return nil, serr
		}
		serials = append(serials, s)
		pos += sn
	}
	dataPos := int(headerLen)
	values := make([]any, 0, len(serials))
	for _, s := range serials {
		v, size, err := decodeSerial(rec[dataPos:], s)
		if err != nil {
			return nil, err
		}
		dataPos += size
		values = append(values, v)
	}
	return values, nil
}

func decodeSerial(b []byte, serial uint64) (any, int, *DomainError) {
	switch serial {
	case 0:
		return nil, 0, nil
	case 1:
		if len(b) < 1 {
			return nil, 0, truncatedSerial()
		}
		return int64(int8(b[0])), 1, nil
	case 2:
		if len(b) < 2 {
			return nil, 0, truncatedSerial()
		}
		return int64(int16(binary.BigEndian.Uint16(b[:2]))), 2, nil
	case 3:
		if len(b) < 3 {
			return nil, 0, truncatedSerial()
		}
		v := int64(b[0])<<16 | int64(b[1])<<8 | int64(b[2])
		if v&(1<<23) != 0 {
			v -= 1 << 24
		}
		return v, 3, nil
	case 4:
		if len(b) < 4 {
			return nil, 0, truncatedSerial()
		}
		return int64(int32(binary.BigEndian.Uint32(b[:4]))), 4, nil
	case 5:
		if len(b) < 6 {
			return nil, 0, truncatedSerial()
		}
		v := int64(b[0])<<40 | int64(b[1])<<32 | int64(b[2])<<24 | int64(b[3])<<16 | int64(b[4])<<8 | int64(b[5])
		if v&(1<<47) != 0 {
			v -= 1 << 48
		}
		return v, 6, nil
	case 6:
		if len(b) < 8 {
			return nil, 0, truncatedSerial()
		}
		return int64(binary.BigEndian.Uint64(b[:8])), 8, nil
	case 7:
		return nil, 8, NewError(CodeSQLiteCorrupt, "float serial not used by checkpoint")
	case 8:
		return int64(0), 0, nil
	case 9:
		return int64(1), 0, nil
	default:
		var size int
		if serial >= 12 && serial%2 == 0 {
			size = int((serial - 12) / 2)
			if len(b) < size {
				return nil, 0, truncatedSerial()
			}
			return append([]byte(nil), b[:size]...), size, nil
		}
		if serial >= 13 && serial%2 == 1 {
			size = int((serial - 13) / 2)
			if len(b) < size {
				return nil, 0, truncatedSerial()
			}
			return string(b[:size]), size, nil
		}
		return nil, 0, NewError(CodeSQLiteCorrupt, "unsupported serial type %d", serial)
	}
}

func truncatedSerial() *DomainError {
	return NewError(CodeCheckpointTruncated, "serial value extends past cell payload")
}
