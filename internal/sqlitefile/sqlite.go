// Package sqlitefile implements a small, dependency-free subset of the
// SQLite 3 file format. It is intentionally limited to UTF-8 rowid tables with
// leaf/interior table B-trees and overflow pages. The checkpoint layer uses it
// so the repository needs neither CGO nor a sqlite3 driver or executable.
package sqlitefile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	pageSize      = 4096
	sqliteVersion = 3_046_000
	usableSize    = pageSize
	headerSize    = 100
	tableLeaf     = 0x0d
	tableInterior = 0x05
)

var (
	ErrTruncated = errors.New("truncated SQLite file")
	ErrCorrupt   = errors.New("corrupt SQLite file")
)

type Column struct {
	Name string
	Type string
}

type Table struct {
	Name    string
	Columns []Column
	Rows    [][]any
}

type Row struct {
	RowID  int64
	Values []any
}

type TableData struct {
	Name        string
	Columns     []string
	ColumnSQL   string
	RootPage    uint32
	Rows        []Row
	columnIndex map[string]int
}

func (t *TableData) Column(name string) int {
	if t == nil {
		return -1
	}
	index, ok := t.columnIndex[name]
	if !ok {
		return -1
	}
	return index
}

func (r Row) String(index int) (string, bool) {
	if index < 0 || index >= len(r.Values) {
		return "", false
	}
	value, ok := r.Values[index].(string)
	return value, ok
}

func (r Row) Int64(index int) (int64, bool) {
	if index < 0 || index >= len(r.Values) {
		return 0, false
	}
	value, ok := r.Values[index].(int64)
	return value, ok
}

type Database struct {
	Tables map[string]*TableData
}

type builder struct {
	pages     map[uint32][]byte
	nextPage  uint32
	roots     map[string]uint32
	tableDefs []Table
}

type payloadRow struct {
	rowID   int64
	payload []byte
	cellLen int
}

type childRef struct {
	page   uint32
	maxKey int64
}

func Encode(tables []Table) ([]byte, error) {
	if len(tables) == 0 {
		return nil, fmt.Errorf("at least one table is required")
	}
	builder := &builder{
		pages:    make(map[uint32][]byte),
		nextPage: 2,
		roots:    make(map[string]uint32),
	}
	seen := make(map[string]struct{}, len(tables))
	for _, table := range tables {
		if table.Name == "" || len(table.Columns) == 0 {
			return nil, fmt.Errorf("table name and columns are required")
		}
		if _, exists := seen[table.Name]; exists {
			return nil, fmt.Errorf("duplicate table %q", table.Name)
		}
		seen[table.Name] = struct{}{}
		root, err := builder.buildTable(table)
		if err != nil {
			return nil, err
		}
		builder.roots[table.Name] = root
		builder.tableDefs = append(builder.tableDefs, table)
	}
	if err := builder.buildSchemaPage(); err != nil {
		return nil, err
	}
	return builder.finish()
}

func (b *builder) allocPage() (uint32, []byte, error) {
	page := b.nextPage
	if page == math.MaxUint32 {
		return 0, nil, fmt.Errorf("SQLite page number overflow")
	}
	b.nextPage++
	data := make([]byte, pageSize)
	b.pages[page] = data
	return page, data, nil
}

func (b *builder) buildTable(table Table) (uint32, error) {
	rows := make([]payloadRow, 0, len(table.Rows))
	for index, values := range table.Rows {
		if len(values) != len(table.Columns) {
			return 0, fmt.Errorf("table %s row %d has %d values, want %d", table.Name, index, len(values), len(table.Columns))
		}
		payload, err := encodeRecord(values)
		if err != nil {
			return 0, fmt.Errorf("table %s row %d: %w", table.Name, index, err)
		}
		rowID := int64(index + 1)
		local, overflow := localPayloadSize(len(payload))
		cellLen := varintLen(uint64(len(payload))) + varintLen(uint64(rowID)) + local
		if overflow {
			cellLen += 4
		}
		rows = append(rows, payloadRow{rowID: rowID, payload: payload, cellLen: cellLen})
	}
	if len(rows) == 0 {
		page, data, err := b.allocPage()
		if err != nil {
			return 0, err
		}
		writeLeafHeader(data, 0)
		return page, nil
	}
	groups := groupLeafRows(rows, func(row payloadRow) int { return row.cellLen })
	children := make([]childRef, 0, len(groups))
	for _, group := range groups {
		page, data, err := b.allocPage()
		if err != nil {
			return 0, err
		}
		if err := b.buildLeaf(data, group); err != nil {
			return 0, err
		}
		children = append(children, childRef{page: page, maxKey: group[len(group)-1].rowID})
	}
	if len(children) == 1 {
		return children[0].page, nil
	}
	root, data, err := b.allocPage()
	if err != nil {
		return 0, err
	}
	if err := b.buildInterior(data, children); err != nil {
		return 0, err
	}
	return root, nil
}

func groupLeafRows[T any](rows []T, cellLen func(T) int) [][]T {
	groups := make([][]T, 0)
	current := make([]T, 0)
	used := 0
	for _, row := range rows {
		size := cellLen(row)
		needed := used + size + 2
		if len(current) > 0 && 8+needed > pageSize {
			groups = append(groups, current)
			current = nil
			used = 0
			needed = size + 2
		}
		current = append(current, row)
		used += size
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

func (b *builder) buildLeaf(page []byte, rows []payloadRow) error {
	writeLeafHeader(page, len(rows))
	content := pageSize
	offsets := make([]int, len(rows))
	for index, row := range rows {
		cell, err := b.encodeLeafCell(row.rowID, row.payload)
		if err != nil {
			return err
		}
		content -= len(cell)
		if content < 8+2*len(rows) {
			return fmt.Errorf("leaf page overflow")
		}
		copy(page[content:], cell)
		offsets[index] = content
	}
	binary.BigEndian.PutUint16(page[5:7], uint16(content))
	for index, offset := range offsets {
		binary.BigEndian.PutUint16(page[8+2*index:10+2*index], uint16(offset))
	}
	return nil
}

func (b *builder) encodeLeafCell(rowID int64, payload []byte) ([]byte, error) {
	local, overflow := localPayloadSize(len(payload))
	cell := appendVarint(nil, uint64(len(payload)))
	cell = appendVarint(cell, uint64(rowID))
	cell = append(cell, payload[:local]...)
	if overflow {
		first, err := b.writeOverflow(payload[local:])
		if err != nil {
			return nil, err
		}
		cell = binary.BigEndian.AppendUint32(cell, first)
	}
	return cell, nil
}

func (b *builder) writeOverflow(rest []byte) (uint32, error) {
	if len(rest) == 0 {
		return 0, nil
	}
	capacity := usableSize - 4
	count := (len(rest) + capacity - 1) / capacity
	pages := make([]uint32, count)
	buffers := make([][]byte, count)
	for index := 0; index < count; index++ {
		page, data, err := b.allocPage()
		if err != nil {
			return 0, err
		}
		pages[index] = page
		buffers[index] = data
	}
	for index := range pages {
		next := uint32(0)
		if index+1 < len(pages) {
			next = pages[index+1]
		}
		binary.BigEndian.PutUint32(buffers[index][0:4], next)
		begin := index * capacity
		end := begin + capacity
		if end > len(rest) {
			end = len(rest)
		}
		copy(buffers[index][4:], rest[begin:end])
	}
	return pages[0], nil
}

func (b *builder) buildInterior(page []byte, children []childRef) error {
	page[0] = tableInterior
	binary.BigEndian.PutUint16(page[3:5], uint16(len(children)-1))
	binary.BigEndian.PutUint32(page[8:12], children[len(children)-1].page)
	cells := make([][]byte, 0, len(children)-1)
	for index := 0; index < len(children)-1; index++ {
		cell := make([]byte, 4, 4+9)
		binary.BigEndian.PutUint32(cell, children[index].page)
		cell = appendVarint(cell, uint64(children[index].maxKey))
		cells = append(cells, cell)
	}
	content := pageSize
	offsets := make([]int, len(cells))
	for index, cell := range cells {
		content -= len(cell)
		if content < 12+2*len(cells) {
			return fmt.Errorf("interior page overflow")
		}
		copy(page[content:], cell)
		offsets[index] = content
	}
	binary.BigEndian.PutUint16(page[5:7], uint16(content))
	for index, offset := range offsets {
		binary.BigEndian.PutUint16(page[12+2*index:14+2*index], uint16(offset))
	}
	return nil
}

func writeLeafHeader(page []byte, cells int) {
	page[0] = tableLeaf
	binary.BigEndian.PutUint16(page[3:5], uint16(cells))
	binary.BigEndian.PutUint16(page[5:7], uint16(pageSize))
}

func (b *builder) buildSchemaPage() error {
	page := make([]byte, pageSize)
	b.pages[1] = page
	type schemaRow struct {
		values []any
	}
	rows := make([]schemaRow, 0, len(b.tableDefs))
	for _, table := range b.tableDefs {
		sql := createSQL(table)
		rows = append(rows, schemaRow{values: []any{
			"table",
			table.Name,
			table.Name,
			int64(b.roots[table.Name]),
			sql,
		}})
	}
	payloads := make([][]byte, len(rows))
	for index, row := range rows {
		payload, err := encodeRecord(row.values)
		if err != nil {
			return err
		}
		payloads[index] = payload
	}
	page[headerSize] = tableLeaf
	binary.BigEndian.PutUint16(page[headerSize+3:headerSize+5], uint16(len(rows)))
	content := pageSize
	offsets := make([]int, len(rows))
	for index, payload := range payloads {
		local, overflow := localPayloadSize(len(payload))
		cell := appendVarint(nil, uint64(len(payload)))
		cell = appendVarint(cell, uint64(index+1))
		cell = append(cell, payload[:local]...)
		if overflow {
			first, err := b.writeOverflow(payload[local:])
			if err != nil {
				return err
			}
			cell = binary.BigEndian.AppendUint32(cell, first)
		}
		content -= len(cell)
		if content < headerSize+8+2*len(rows) {
			return fmt.Errorf("sqlite_schema page overflow")
		}
		copy(page[content:], cell)
		offsets[index] = content
	}
	binary.BigEndian.PutUint16(page[headerSize+5:headerSize+7], uint16(content))
	for index, offset := range offsets {
		binary.BigEndian.PutUint16(page[headerSize+8+2*index:headerSize+10+2*index], uint16(offset))
	}
	return nil
}

func createSQL(table Table) string {
	parts := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		parts = append(parts, column.Name+" "+column.Type)
	}
	return "CREATE TABLE " + table.Name + " (" + strings.Join(parts, ",") + ")"
}

func (b *builder) finish() ([]byte, error) {
	if len(b.pages) == 0 {
		return nil, fmt.Errorf("empty SQLite database")
	}
	maxPage := uint32(0)
	for page := range b.pages {
		if page > maxPage {
			maxPage = page
		}
	}
	if maxPage != b.nextPage-1 {
		return nil, fmt.Errorf("non-contiguous page allocation: max=%d next=%d", maxPage, b.nextPage)
	}
	file := make([]byte, int(maxPage)*pageSize)
	for page := uint32(1); page <= maxPage; page++ {
		data, ok := b.pages[page]
		if !ok {
			return nil, fmt.Errorf("missing page %d", page)
		}
		copy(file[(int(page)-1)*pageSize:int(page)*pageSize], data)
	}
	header := file[:headerSize]
	copy(header[0:16], "SQLite format 3\x00")
	binary.BigEndian.PutUint16(header[16:18], pageSize)
	header[18] = 1
	header[19] = 1
	header[21] = 64
	header[22] = 32
	header[23] = 32
	binary.BigEndian.PutUint32(header[24:28], 1)
	binary.BigEndian.PutUint32(header[28:32], maxPage)
	binary.BigEndian.PutUint32(header[40:44], 1)
	binary.BigEndian.PutUint32(header[44:48], 4)
	// A non-zero "largest root page" makes SQLite infer auto-vacuum pointer
	// maps. We do not build pointer maps, so keep this field at zero (the value
	// produced by ordinary non-autovacuum databases).
	binary.BigEndian.PutUint32(header[52:56], 0)
	binary.BigEndian.PutUint32(header[56:60], 1)
	binary.BigEndian.PutUint32(header[68:72], 0x53455247)
	binary.BigEndian.PutUint32(header[92:96], 1)
	binary.BigEndian.PutUint32(header[96:100], sqliteVersion)
	return file, nil
}

func localPayloadSize(payloadLen int) (int, bool) {
	maxLocal := usableSize - 35
	if payloadLen <= maxLocal {
		return payloadLen, false
	}
	minLocal := (usableSize-12)*32/255 - 23
	local := minLocal + (payloadLen-minLocal)%(usableSize-4)
	if local > maxLocal {
		local = minLocal
	}
	return local, true
}

func Decode(data []byte) (*Database, error) {
	if len(data) < headerSize {
		return nil, fmt.Errorf("%w: header is %d bytes", ErrTruncated, len(data))
	}
	if !bytes.Equal(data[:16], []byte("SQLite format 3\x00")) {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	pageSizeValue := int(binary.BigEndian.Uint16(data[16:18]))
	if pageSizeValue < 512 || pageSizeValue > 65536 || pageSizeValue&(pageSizeValue-1) != 0 {
		return nil, fmt.Errorf("%w: invalid page size %d", ErrCorrupt, pageSizeValue)
	}
	if pageSizeValue != pageSize {
		return nil, fmt.Errorf("%w: unsupported page size %d", ErrCorrupt, pageSizeValue)
	}
	pageCount := int(binary.BigEndian.Uint32(data[28:32]))
	if pageCount <= 0 {
		return nil, fmt.Errorf("%w: zero database size", ErrCorrupt)
	}
	required := pageCount * pageSizeValue
	if len(data) < required {
		return nil, fmt.Errorf("%w: have %d bytes, need %d", ErrTruncated, len(data), required)
	}
	reader := &fileReader{data: data[:required], pageSize: pageSizeValue, pageCount: pageCount, visited: make(map[uint32]bool)}
	schemaRows, err := reader.readTableTree(1, headerSize)
	if err != nil {
		return nil, err
	}
	database := &Database{Tables: make(map[string]*TableData)}
	for _, row := range schemaRows {
		if len(row.Values) < 5 {
			return nil, fmt.Errorf("%w: short sqlite_schema row", ErrCorrupt)
		}
		kind, _ := row.Values[0].(string)
		if kind != "table" {
			continue
		}
		name, _ := row.Values[1].(string)
		root, ok := row.Values[3].(int64)
		if !ok || root <= 0 {
			return nil, fmt.Errorf("%w: invalid root page for %s", ErrCorrupt, name)
		}
		sql, _ := row.Values[4].(string)
		columns, err := parseColumns(sql)
		if err != nil {
			return nil, fmt.Errorf("%w: table %s: %v", ErrCorrupt, name, err)
		}
		tableRows, err := reader.readTableTree(uint32(root), 0)
		if err != nil {
			return nil, fmt.Errorf("%w: table %s: %v", ErrCorrupt, name, err)
		}
		index := make(map[string]int, len(columns))
		for columnIndex, column := range columns {
			index[column] = columnIndex
		}
		database.Tables[name] = &TableData{
			Name:        name,
			Columns:     columns,
			ColumnSQL:   sql,
			RootPage:    uint32(root),
			Rows:        tableRows,
			columnIndex: index,
		}
	}
	return database, nil
}

type fileReader struct {
	data      []byte
	pageSize  int
	pageCount int
	visited   map[uint32]bool
}

func (r *fileReader) page(page uint32) ([]byte, error) {
	if page == 0 || int(page) > r.pageCount {
		return nil, fmt.Errorf("%w: page %d outside 1..%d", ErrCorrupt, page, r.pageCount)
	}
	begin := (int(page) - 1) * r.pageSize
	return r.data[begin : begin+r.pageSize], nil
}

func (r *fileReader) readTableTree(root uint32, pageOneOffset int) ([]Row, error) {
	if r.visited[root] {
		return nil, fmt.Errorf("%w: B-tree cycle at page %d", ErrCorrupt, root)
	}
	r.visited[root] = true
	page, err := r.page(root)
	if err != nil {
		return nil, err
	}
	if pageOneOffset >= len(page) {
		return nil, fmt.Errorf("%w: bad page-one offset", ErrCorrupt)
	}
	kind := page[pageOneOffset]
	switch kind {
	case tableLeaf:
		return r.readLeaf(page, pageOneOffset)
	case tableInterior:
		return r.readInterior(page, pageOneOffset)
	default:
		return nil, fmt.Errorf("%w: unexpected b-tree page type 0x%x", ErrCorrupt, kind)
	}
}

func (r *fileReader) readLeaf(page []byte, offset int) ([]Row, error) {
	if offset+8 > len(page) {
		return nil, fmt.Errorf("%w: short leaf header", ErrTruncated)
	}
	count := int(binary.BigEndian.Uint16(page[offset+3 : offset+5]))
	pointerStart := offset + 8
	if pointerStart+count*2 > len(page) {
		return nil, fmt.Errorf("%w: short leaf pointer array", ErrTruncated)
	}
	rows := make([]Row, 0, count)
	for index := 0; index < count; index++ {
		cellOffset := int(binary.BigEndian.Uint16(page[pointerStart+2*index : pointerStart+2*index+2]))
		if cellOffset < 0 || cellOffset >= len(page) {
			return nil, fmt.Errorf("%w: bad cell offset", ErrCorrupt)
		}
		payloadLen, width, err := readVarint(page[cellOffset:])
		if err != nil {
			return nil, err
		}
		cursor := cellOffset + width
		rowID, width, err := readVarint(page[cursor:])
		if err != nil {
			return nil, err
		}
		cursor += width
		local, overflow := localPayloadSize(int(payloadLen))
		if cursor+local > len(page) {
			return nil, fmt.Errorf("%w: short local payload", ErrTruncated)
		}
		payload := append([]byte(nil), page[cursor:cursor+local]...)
		cursor += local
		if overflow {
			if cursor+4 > len(page) {
				return nil, fmt.Errorf("%w: missing overflow pointer", ErrTruncated)
			}
			first := binary.BigEndian.Uint32(page[cursor : cursor+4])
			overflowBytes, err := r.readOverflow(first, int(payloadLen)-local)
			if err != nil {
				return nil, err
			}
			payload = append(payload, overflowBytes...)
		}
		if len(payload) != int(payloadLen) {
			return nil, fmt.Errorf("%w: payload length mismatch", ErrCorrupt)
		}
		values, err := decodeRecord(payload)
		if err != nil {
			return nil, err
		}
		rows = append(rows, Row{RowID: int64(rowID), Values: values})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].RowID < rows[j].RowID })
	return rows, nil
}

func (r *fileReader) readInterior(page []byte, offset int) ([]Row, error) {
	if offset+12 > len(page) {
		return nil, fmt.Errorf("%w: short interior header", ErrTruncated)
	}
	count := int(binary.BigEndian.Uint16(page[offset+3 : offset+5]))
	pointerStart := offset + 12
	if pointerStart+count*2 > len(page) {
		return nil, fmt.Errorf("%w: short interior pointer array", ErrTruncated)
	}
	var rows []Row
	for index := 0; index < count; index++ {
		cellOffset := int(binary.BigEndian.Uint16(page[pointerStart+2*index : pointerStart+2*index+2]))
		if cellOffset < 0 || cellOffset+4 > len(page) {
			return nil, fmt.Errorf("%w: bad interior cell", ErrCorrupt)
		}
		child := binary.BigEndian.Uint32(page[cellOffset : cellOffset+4])
		childRows, err := r.readTableTree(child, 0)
		if err != nil {
			return nil, err
		}
		rows = append(rows, childRows...)
	}
	rightMost := binary.BigEndian.Uint32(page[offset+8 : offset+12])
	childRows, err := r.readTableTree(rightMost, 0)
	if err != nil {
		return nil, err
	}
	rows = append(rows, childRows...)
	return rows, nil
}

func (r *fileReader) readOverflow(first uint32, count int) ([]byte, error) {
	if count <= 0 {
		if first != 0 {
			return nil, fmt.Errorf("%w: unexpected overflow page", ErrCorrupt)
		}
		return nil, nil
	}
	if first == 0 {
		return nil, fmt.Errorf("%w: missing overflow chain", ErrCorrupt)
	}
	result := make([]byte, 0, count)
	pageNumber := first
	seen := make(map[uint32]bool)
	for {
		if pageNumber == 0 {
			break
		}
		if seen[pageNumber] {
			return nil, fmt.Errorf("%w: overflow cycle", ErrCorrupt)
		}
		seen[pageNumber] = true
		page, err := r.page(pageNumber)
		if err != nil {
			return nil, err
		}
		next := binary.BigEndian.Uint32(page[0:4])
		remaining := count - len(result)
		take := usableSize - 4
		if take > remaining {
			take = remaining
		}
		result = append(result, page[4:4+take]...)
		if len(result) == count {
			break
		}
		pageNumber = next
	}
	if len(result) != count {
		return nil, fmt.Errorf("%w: overflow chain too short", ErrTruncated)
	}
	return result, nil
}

func parseColumns(sql string) ([]string, error) {
	open := strings.IndexByte(sql, '(')
	close := strings.LastIndexByte(sql, ')')
	if open < 0 || close <= open {
		return nil, fmt.Errorf("cannot parse CREATE TABLE statement")
	}
	body := sql[open+1 : close]
	parts := strings.Split(body, ",")
	columns := make([]string, 0, len(parts))
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 {
			continue
		}
		columns = append(columns, fields[0])
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("no columns in CREATE TABLE statement")
	}
	return columns, nil
}

func encodeRecord(values []any) ([]byte, error) {
	serialTypes := make([][]byte, len(values))
	body := make([]byte, 0)
	for index, value := range values {
		serial, data, err := encodeValue(value)
		if err != nil {
			return nil, fmt.Errorf("column %d: %w", index, err)
		}
		serialTypes[index] = appendVarint(nil, serial)
		body = append(body, data...)
	}
	serialBytes := 0
	for _, serial := range serialTypes {
		serialBytes += len(serial)
	}
	headerLen := 1 + serialBytes
	for {
		next := varintLen(uint64(headerLen)) + serialBytes
		if next == headerLen {
			break
		}
		headerLen = next
	}
	header := appendVarint(nil, uint64(headerLen))
	for _, serial := range serialTypes {
		header = append(header, serial...)
	}
	if len(header) != headerLen {
		return nil, fmt.Errorf("record header sizing failed")
	}
	return append(header, body...), nil
}

func encodeValue(value any) (uint64, []byte, error) {
	switch typed := value.(type) {
	case nil:
		return 0, nil, nil
	case int:
		return 6, int64Bytes(int64(typed)), nil
	case int64:
		return 6, int64Bytes(typed), nil
	case uint32:
		return 6, int64Bytes(int64(typed)), nil
	case bool:
		if typed {
			return 6, int64Bytes(1), nil
		}
		return 6, int64Bytes(0), nil
	case string:
		data := []byte(typed)
		if !utf8.Valid(data) {
			return 0, nil, fmt.Errorf("invalid UTF-8 text")
		}
		return uint64(13 + 2*len(data)), data, nil
	case []byte:
		return uint64(12 + 2*len(typed)), typed, nil
	default:
		return 0, nil, fmt.Errorf("unsupported value type %T", value)
	}
}

func int64Bytes(value int64) []byte {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, uint64(value))
	return data
}

func decodeRecord(payload []byte) ([]any, error) {
	headerLen, width, err := readVarint(payload)
	if err != nil {
		return nil, err
	}
	if headerLen < uint64(width) || headerLen > uint64(len(payload)) {
		return nil, fmt.Errorf("%w: invalid record header length", ErrCorrupt)
	}
	header := payload[width:int(headerLen)]
	serialTypes := make([]uint64, 0)
	for len(header) > 0 {
		serial, used, err := readVarint(header)
		if err != nil {
			return nil, err
		}
		serialTypes = append(serialTypes, serial)
		header = header[used:]
	}
	body := payload[int(headerLen):]
	values := make([]any, 0, len(serialTypes))
	for _, serial := range serialTypes {
		switch {
		case serial == 0:
			values = append(values, nil)
		case serial >= 1 && serial <= 6:
			length := []int{0, 1, 2, 3, 4, 6, 8}[serial]
			if len(body) < length {
				return nil, fmt.Errorf("%w: short integer body", ErrTruncated)
			}
			values = append(values, decodeSigned(body[:length]))
			body = body[length:]
		case serial == 7:
			if len(body) < 8 {
				return nil, fmt.Errorf("%w: short float body", ErrTruncated)
			}
			values = append(values, math.Float64frombits(binary.BigEndian.Uint64(body[:8])))
			body = body[8:]
		case serial == 8:
			values = append(values, int64(0))
		case serial == 9:
			values = append(values, int64(1))
		case serial >= 12 && serial%2 == 0:
			length := int((serial - 12) / 2)
			if len(body) < length {
				return nil, fmt.Errorf("%w: short blob body", ErrTruncated)
			}
			values = append(values, append([]byte(nil), body[:length]...))
			body = body[length:]
		case serial >= 13 && serial%2 == 1:
			length := int((serial - 13) / 2)
			if len(body) < length {
				return nil, fmt.Errorf("%w: short text body", ErrTruncated)
			}
			values = append(values, string(body[:length]))
			body = body[length:]
		default:
			return nil, fmt.Errorf("%w: unsupported serial type %d", ErrCorrupt, serial)
		}
	}
	return values, nil
}

func decodeSigned(data []byte) int64 {
	if len(data) == 0 {
		return 0
	}
	var value uint64
	for _, item := range data {
		value = value<<8 | uint64(item)
	}
	shift := uint(64 - 8*len(data))
	return int64(value<<shift) >> shift
}

func appendVarint(dst []byte, value uint64) []byte {
	if value <= 0x7f {
		return append(dst, byte(value))
	}
	if value > 0x00ff_ffff_ffff_ffff {
		var encoded [9]byte
		encoded[8] = byte(value)
		value >>= 8
		for index := 7; index >= 0; index-- {
			encoded[index] = byte(value&0x7f) | 0x80
			value >>= 7
		}
		return append(dst, encoded[:]...)
	}
	var encoded [9]byte
	length := 2
	for value > (uint64(1)<<(7*length))-1 {
		length++
	}
	for index := length - 1; index >= 0; index-- {
		encoded[index] = byte(value & 0x7f)
		value >>= 7
	}
	for index := 0; index < length-1; index++ {
		encoded[index] |= 0x80
	}
	return append(dst, encoded[:length]...)
}

func readVarint(data []byte) (uint64, int, error) {
	var value uint64
	for index := 0; index < 9; index++ {
		if index >= len(data) {
			return 0, 0, fmt.Errorf("%w: varint", ErrTruncated)
		}
		item := data[index]
		if index == 8 {
			return value<<8 | uint64(item), 9, nil
		}
		value = value<<7 | uint64(item&0x7f)
		if item&0x80 == 0 {
			return value, index + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("%w: malformed varint", ErrCorrupt)
}

func varintLen(value uint64) int {
	if value <= 0x7f {
		return 1
	}
	if value > 0x00ff_ffff_ffff_ffff {
		return 9
	}
	length := 2
	for value > (uint64(1)<<(7*length))-1 {
		length++
	}
	return length
}
