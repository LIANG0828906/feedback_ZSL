// Package sqlitefile implements the small subset of the SQLite 3 file format
// needed by LayerForge checkpoints. It deliberately has no third-party
// dependency. The writer emits ordinary rowid tables in leaf b-tree pages.
package sqlitefile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	pageSize      = 65536
	sqliteVersion = 3046000
)

var (
	ErrNotSQLite = errors.New("not a SQLite 3 database")
	ErrTruncated = errors.New("truncated SQLite database")
	ErrCorrupt   = errors.New("corrupt SQLite database")
)

type Value = any

type Table struct {
	Name    string
	Columns []string
	Rows    [][]Value
}

type Database struct {
	Tables []Table
}

type CommitStage string

const (
	StageAfterLayers  CommitStage = "after-layers"
	StageAfterIndex   CommitStage = "after-index"
	StageAfterRounds  CommitStage = "after-rounds"
	StageBeforeWrite  CommitStage = "before-write"
	StageBeforeRename CommitStage = "before-rename"
	StageAfterRename  CommitStage = "after-rename"
)

func WriteAtomic(path string, db Database, hook func(CommitStage) error) error {
	if len(db.Tables) == 0 {
		return fmt.Errorf("sqlitefile: database has no tables")
	}
	if hook != nil {
		for _, stage := range []CommitStage{StageAfterLayers, StageAfterIndex, StageAfterRounds, StageBeforeWrite} {
			if err := hook(stage); err != nil {
				return err
			}
		}
	}
	data, err := encode(db)
	if err != nil {
		return err
	}
	if hook != nil {
		if err := hook(StageBeforeRename); err != nil {
			return err
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	if hook != nil {
		if err := hook(StageAfterRename); err != nil {
			return err
		}
	}
	return nil
}

func encode(db Database) ([]byte, error) {
	pageCount := 1 + len(db.Tables)
	if pageCount > 0x7fff {
		return nil, fmt.Errorf("sqlitefile: too many tables")
	}
	out := make([]byte, pageCount*pageSize)
	writeDatabaseHeader(out, pageCount)

	schemaRows := make([][]Value, 0, len(db.Tables))
	for index, table := range db.Tables {
		rootPage := index + 2
		if table.Name == "" || len(table.Columns) == 0 {
			return nil, fmt.Errorf("sqlitefile: table name and columns are required")
		}
		sql := createSQL(table)
		schemaRows = append(schemaRows, []Value{"table", table.Name, table.Name, int64(rootPage), sql})
		if err := writeTablePage(out[(rootPage-1)*pageSize:rootPage*pageSize], table.Rows); err != nil {
			return nil, fmt.Errorf("sqlitefile: table %s: %w", table.Name, err)
		}
	}
	if err := writeTablePage(out[:pageSize], schemaRows); err != nil {
		return nil, fmt.Errorf("sqlitefile: sqlite_master: %w", err)
	}
	return out, nil
}

func writeDatabaseHeader(data []byte, pageCount int) {
	copy(data[:16], []byte("SQLite format 3\x00"))
	binary.BigEndian.PutUint16(data[16:18], 1) // 65536, represented as 1
	data[18] = 1                               // write version: rollback journal
	data[19] = 1                               // read version
	data[20] = 0                               // reserved bytes per page
	data[21] = 64
	data[22] = 32
	data[23] = 32
	binary.BigEndian.PutUint32(data[24:28], 1)
	binary.BigEndian.PutUint32(data[28:32], uint32(pageCount))
	binary.BigEndian.PutUint32(data[40:44], 1)
	binary.BigEndian.PutUint32(data[44:48], 4)
	binary.BigEndian.PutUint32(data[56:60], 1)
	binary.BigEndian.PutUint32(data[92:96], 1)
	binary.BigEndian.PutUint32(data[96:100], sqliteVersion)
}

func createSQL(table Table) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s(", quoteIdent(table.Name))
	for i, column := range table.Columns {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quoteIdent(column))
		b.WriteByte(' ')
		b.WriteString(columnType(table.Rows, i))
	}
	b.WriteByte(')')
	return b.String()
}

func columnType(rows [][]Value, column int) string {
	sawValue := false
	for _, row := range rows {
		if column >= len(row) || row[column] == nil {
			continue
		}
		sawValue = true
		switch row[column].(type) {
		case int, int64, uint64:
		default:
			return "TEXT"
		}
	}
	if sawValue {
		return "INTEGER"
	}
	return "TEXT"
}

func quoteIdent(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func writeTablePage(page []byte, rows [][]Value) error {
	headerOffset := 0
	if len(page) == pageSize {
		// The caller only reaches this branch for page 1 when the buffer is a
		// standalone page. In encode(), page 1 is passed as out[:pageSize].
	}
	// Page 1 is detected by the SQLite magic and has its b-tree header at 100.
	isPageOne := len(page) >= 100 && bytes.Equal(page[:16], []byte("SQLite format 3\x00"))
	if isPageOne {
		headerOffset = 100
	}
	page[headerOffset] = 0x0d // table b-tree leaf
	binary.BigEndian.PutUint16(page[headerOffset+1:headerOffset+3], 0)
	binary.BigEndian.PutUint16(page[headerOffset+3:headerOffset+5], uint16(len(rows)))
	binary.BigEndian.PutUint16(page[headerOffset+7:headerOffset+9], 0)

	pointerOffset := headerOffset + 8
	contentTop := len(page)
	for index := len(rows) - 1; index >= 0; index-- {
		cell := encodeRecord(rows[index])
		rowID := uint64(index + 1)
		cell = append(encodeVarint(uint64(len(cell))), append(encodeVarint(rowID), cell...)...)
		contentTop -= len(cell)
		if contentTop < pointerOffset+2*len(rows) {
			return fmt.Errorf("rows do not fit on one SQLite page")
		}
		copy(page[contentTop:], cell)
		offset := contentTop
		if offset == pageSize {
			offset = 0
		}
		binary.BigEndian.PutUint16(page[pointerOffset+2*index:pointerOffset+2*index+2], uint16(offset))
	}
	contentOffset := contentTop
	if contentOffset == pageSize {
		contentOffset = 0
	}
	binary.BigEndian.PutUint16(page[headerOffset+5:headerOffset+7], uint16(contentOffset))
	return nil
}

func encodeRecord(values []Value) []byte {
	serials := make([][]byte, len(values))
	payloads := make([][]byte, len(values))
	serialBytes := 0
	for i, value := range values {
		serial, payload := encodeValue(value)
		serials[i] = encodeVarint(serial)
		payloads[i] = payload
		serialBytes += len(serials[i])
	}
	headerLen := serialBytes + 1
	for varintLen(headerLen)+serialBytes != headerLen {
		headerLen = varintLen(headerLen) + serialBytes
	}
	out := make([]byte, 0, headerLen+sumLengths(payloads))
	out = append(out, encodeVarint(uint64(headerLen))...)
	for _, serial := range serials {
		out = append(out, serial...)
	}
	for _, payload := range payloads {
		out = append(out, payload...)
	}
	return out
}

func encodeValue(value Value) (uint64, []byte) {
	switch typed := value.(type) {
	case nil:
		return 0, nil
	case int:
		return 6, encodeInt64(int64(typed))
	case int64:
		if typed == 0 {
			return 8, nil
		}
		if typed == 1 {
			return 9, nil
		}
		return 6, encodeInt64(typed)
	case uint64:
		return 6, encodeInt64(int64(typed))
	case string:
		return uint64(13 + 2*len(typed)), []byte(typed)
	case []byte:
		return uint64(12 + 2*len(typed)), typed
	default:
		panic(fmt.Sprintf("sqlitefile: unsupported value type %T", value))
	}
}

func encodeInt64(value int64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, uint64(value))
	return out
}

func sumLengths(values [][]byte) int {
	total := 0
	for _, value := range values {
		total += len(value)
	}
	return total
}

func encodeVarint(value uint64) []byte {
	if value > 0x00ffffffffffffff {
		out := make([]byte, 9)
		out[8] = byte(value)
		value >>= 8
		for i := 7; i >= 0; i-- {
			out[i] = byte(value&0x7f) | 0x80
			value >>= 7
		}
		return out
	}
	var tmp [9]byte
	size := 0
	for {
		size++
		tmp[9-size] = byte(value & 0x7f)
		value >>= 7
		if value == 0 {
			break
		}
	}
	out := make([]byte, size)
	copy(out, tmp[9-size:])
	for i := 0; i < len(out)-1; i++ {
		out[i] |= 0x80
	}
	return out
}

func varintLen(value int) int {
	n := 1
	for value >= 0x80 {
		n++
		value >>= 7
	}
	return n
}

func Read(path string) (*Database, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

func Decode(data []byte) (*Database, error) {
	if len(data) < 100 {
		return nil, ErrTruncated
	}
	if !bytes.Equal(data[:16], []byte("SQLite format 3\x00")) {
		return nil, ErrNotSQLite
	}
	size := int(binary.BigEndian.Uint16(data[16:18]))
	if size == 1 {
		size = pageSize
	}
	if size < 512 || size > pageSize || size&(size-1) != 0 {
		return nil, ErrCorrupt
	}
	pageCount := int(binary.BigEndian.Uint32(data[28:32]))
	if pageCount <= 0 {
		return nil, ErrCorrupt
	}
	if len(data) < pageCount*size {
		return nil, ErrTruncated
	}
	if pageCount*size != len(data) {
		return nil, ErrTruncated
	}
	masterRows, err := readTable(data, 1, size, nil)
	if err != nil {
		return nil, err
	}
	db := &Database{}
	for _, row := range masterRows {
		if len(row) < 5 {
			return nil, ErrCorrupt
		}
		kind, okKind := row[0].(string)
		name, okName := row[1].(string)
		sql, okSQL := row[4].(string)
		root, okRoot := row[3].(int64)
		if !okKind || !okName || !okSQL || !okRoot || kind != "table" {
			continue
		}
		rows, err := readTable(data, int(root), size, nil)
		if err != nil {
			return nil, err
		}
		db.Tables = append(db.Tables, Table{
			Name:    name,
			Columns: parseColumns(sql),
			Rows:    rows,
		})
	}
	sort.Slice(db.Tables, func(i, j int) bool { return db.Tables[i].Name < db.Tables[j].Name })
	return db, nil
}

func readTable(data []byte, pageNumber, size int, visited map[int]bool) ([][]Value, error) {
	if visited == nil {
		visited = make(map[int]bool)
	}
	if visited[pageNumber] {
		return nil, ErrCorrupt
	}
	visited[pageNumber] = true
	start := (pageNumber - 1) * size
	if start < 0 || start+size > len(data) {
		return nil, ErrTruncated
	}
	page := data[start : start+size]
	headerOffset := 0
	if pageNumber == 1 {
		headerOffset = 100
	}
	pageType := page[headerOffset]
	cellCount := int(binary.BigEndian.Uint16(page[headerOffset+3 : headerOffset+5]))
	var rows [][]Value
	switch pageType {
	case 0x0d:
		pointerStart := headerOffset + 8
		for i := 0; i < cellCount; i++ {
			offset := int(binary.BigEndian.Uint16(page[pointerStart+2*i : pointerStart+2*i+2]))
			if offset == 0 {
				offset = pageSize
			}
			row, err := decodeTableCell(page, offset, size)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
	case 0x05:
		pointerStart := headerOffset + 12
		rightmost := int(binary.BigEndian.Uint32(page[headerOffset+8 : headerOffset+12]))
		children := make([]int, 0, cellCount+1)
		for i := 0; i < cellCount; i++ {
			offset := int(binary.BigEndian.Uint16(page[pointerStart+2*i : pointerStart+2*i+2]))
			if offset+4 > len(page) {
				return nil, ErrCorrupt
			}
			children = append(children, int(binary.BigEndian.Uint32(page[offset:offset+4])))
		}
		children = append(children, rightmost)
		for _, child := range children {
			childRows, err := readTable(data, child, size, visited)
			if err != nil {
				return nil, err
			}
			rows = append(rows, childRows...)
		}
	default:
		return nil, ErrCorrupt
	}
	return rows, nil
}

func decodeTableCell(page []byte, offset, size int) ([]Value, error) {
	if offset < 0 || offset >= len(page) {
		return nil, ErrCorrupt
	}
	payloadLen, n := decodeVarint(page[offset:])
	if n == 0 {
		return nil, ErrCorrupt
	}
	offset += n
	_, n = decodeVarint(page[offset:])
	if n == 0 {
		return nil, ErrCorrupt
	}
	offset += n
	maxLocal := size - 35
	if payloadLen > uint64(maxLocal) {
		return nil, fmt.Errorf("%w: overflow payload is not supported", ErrCorrupt)
	}
	if offset+int(payloadLen) > len(page) {
		return nil, ErrTruncated
	}
	return decodeRecord(page[offset : offset+int(payloadLen)])
}

func decodeRecord(payload []byte) ([]Value, error) {
	headerLen, n := decodeVarint(payload)
	if n == 0 || headerLen > uint64(len(payload)) {
		return nil, ErrCorrupt
	}
	header := payload[n:int(headerLen)]
	serials := make([]uint64, 0, len(header))
	for len(header) > 0 {
		serial, used := decodeVarint(header)
		if used == 0 {
			return nil, ErrCorrupt
		}
		serials = append(serials, serial)
		header = header[used:]
	}
	body := payload[headerLen:]
	values := make([]Value, 0, len(serials))
	for _, serial := range serials {
		value, used, err := decodeValue(serial, body)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		body = body[used:]
	}
	return values, nil
}

func decodeValue(serial uint64, body []byte) (Value, int, error) {
	switch serial {
	case 0:
		return nil, 0, nil
	case 1:
		if len(body) < 1 {
			return nil, 0, ErrTruncated
		}
		return int64(int8(body[0])), 1, nil
	case 2:
		if len(body) < 2 {
			return nil, 0, ErrTruncated
		}
		return int64(int16(binary.BigEndian.Uint16(body))), 2, nil
	case 3:
		if len(body) < 3 {
			return nil, 0, ErrTruncated
		}
		value := int64(body[0])<<16 | int64(body[1])<<8 | int64(body[2])
		if value&0x800000 != 0 {
			value |= ^int64(0xffffff)
		}
		return value, 3, nil
	case 4:
		if len(body) < 4 {
			return nil, 0, ErrTruncated
		}
		return int64(int32(binary.BigEndian.Uint32(body))), 4, nil
	case 5:
		if len(body) < 6 {
			return nil, 0, ErrTruncated
		}
		value := int64(binary.BigEndian.Uint16(body))<<32 | int64(binary.BigEndian.Uint32(body[2:]))
		if value&0x800000000000 != 0 {
			value |= ^int64(0xffffffffffff)
		}
		return value, 6, nil
	case 6:
		if len(body) < 8 {
			return nil, 0, ErrTruncated
		}
		return int64(binary.BigEndian.Uint64(body)), 8, nil
	case 7:
		if len(body) < 8 {
			return nil, 0, ErrTruncated
		}
		return binary.BigEndian.Uint64(body), 8, nil
	case 8:
		return int64(0), 0, nil
	case 9:
		return int64(1), 0, nil
	case 10, 11:
		return nil, 0, ErrCorrupt
	default:
		if serial%2 == 1 {
			length := (serial - 13) / 2
			if length > uint64(len(body)) {
				return nil, 0, ErrTruncated
			}
			return string(body[:length]), int(length), nil
		}
		length := (serial - 12) / 2
		if length > uint64(len(body)) {
			return nil, 0, ErrTruncated
		}
		return append([]byte(nil), body[:length]...), int(length), nil
	}
}

func decodeVarint(data []byte) (uint64, int) {
	var value uint64
	for i := 0; i < len(data) && i < 9; i++ {
		if i == 8 {
			value = (value << 8) | uint64(data[i])
			return value, 9
		}
		value = (value << 7) | uint64(data[i]&0x7f)
		if data[i]&0x80 == 0 {
			return value, i + 1
		}
	}
	return 0, 0
}

func parseColumns(sql string) []string {
	start := strings.IndexByte(sql, '(')
	end := strings.LastIndexByte(sql, ')')
	if start < 0 || end <= start {
		return nil
	}
	body := sql[start+1 : end]
	parts := strings.Split(body, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		name = strings.Trim(name, `"`)
		name = strings.ReplaceAll(name, `""`, `"`)
		out = append(out, name)
	}
	return out
}
