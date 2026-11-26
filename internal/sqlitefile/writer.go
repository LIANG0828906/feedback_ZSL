// Package sqlitefile is a dependency-free minimal SQLite 3 KV writer/reader.
//
// It intentionally supports the checkpoint kernel's needs: one table named
// records(k TEXT, v TEXT), a schema leaf page and one table leaf page. The
// resulting file starts with the official SQLite magic and can be inspected by
// an external sqlite3 implementation. Checkpoint commits replace the whole
// file atomically through atomicfile, giving a clean crash boundary.
package sqlitefile

import (
	"encoding/binary"
	"fmt"
)

const (
	PageSize       = 65536
	schemaRootPage = 1
	dataRootPage   = 2
	schemaSQL      = "CREATE TABLE records(k TEXT, v TEXT)"
)

// Row is one ordered key/value checkpoint row.
type Row struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// BuildKVDB builds a valid minimal SQLite database containing all rows.
func BuildKVDB(rows []Row) ([]byte, error) {
	schemaRecord, err := EncodeRecord([]Value{
		{Text: "table"}, {Text: "records"}, {Text: "records"},
		{Integer: dataRootPage, IsInteger: true}, {Text: schemaSQL},
	})
	if err != nil {
		return nil, err
	}
	schemaCell := EncodeCell(1, schemaRecord)
	page1 := make([]byte, PageSize)
	if err := writeLeafPage(page1, 100, [][]byte{schemaCell}); err != nil {
		return nil, fmt.Errorf("write schema page: %w", err)
	}

	indexed := make([]indexedCell, 0, len(rows))
	for i, row := range rows {
		record, err := EncodeRecord([]Value{{Text: row.Key}, {Text: row.Value}})
		if err != nil {
			return nil, err
		}
		indexed = append(indexed, indexedCell{cell: EncodeCell(int64(i+1), record), rowID: int64(i + 1)})
	}

	groups, err := packIndexedCells(indexed)
	if err != nil {
		return nil, err
	}

	if len(groups) == 1 {
		page2 := make([]byte, PageSize)
		if err := writeLeafPage(page2, 0, groups[0].cells); err != nil {
			return nil, fmt.Errorf("write checkpoint page: %w", err)
		}
		writeDatabaseHeader(page1, 2)
		return append(page1, page2...), nil
	}

	leafStartPage := uint32(3)
	totalPages := 2 + len(groups) // page1 schema, page2 interior, remaining leaves
	interior := make([]byte, PageSize)
	children := make([]interiorChild, 0, len(groups)-1)
	leafPages := make([][]byte, 0, len(groups))
	for i, group := range groups {
		pageNo := leafStartPage + uint32(i)
		leaf := make([]byte, PageSize)
		if err := writeLeafPage(leaf, 0, group.cells); err != nil {
			return nil, fmt.Errorf("write checkpoint leaf page %d: %w", pageNo, err)
		}
		leafPages = append(leafPages, leaf)
		if i < len(groups)-1 {
			children = append(children, interiorChild{leftPage: pageNo, maxRowID: group.maxRowID})
		}
	}
	rightMost := leafStartPage + uint32(len(groups)-1)
	if err := writeInteriorPage(interior, children, rightMost); err != nil {
		return nil, err
	}
	writeDatabaseHeader(page1, uint32(totalPages))

	db := append([]byte{}, page1...)
	db = append(db, interior...)
	for _, leaf := range leafPages {
		db = append(db, leaf...)
	}
	return db, nil
}

type cellGroup struct {
	cells    [][]byte
	maxRowID int64
}

type indexedCell struct {
	cell  []byte
	rowID int64
}

func packIndexedCells(indexed []indexedCell) ([]cellGroup, error) {
	groups := []cellGroup{}
	current := [][]byte{}
	cellBytes := 0
	var maxRowID int64
	flush := func() {
		if len(current) == 0 {
			return
		}
		groups = append(groups, cellGroup{cells: current, maxRowID: maxRowID})
		current = [][]byte{}
		cellBytes = 0
	}
	for _, item := range indexed {
		cell := item.cell
		if 8+2+len(cell) >= PageSize {
			return nil, fmt.Errorf("%w: one checkpoint row is too large for a 65536-byte leaf", ErrTooLarge)
		}
		neededIfAdded := cellBytes + len(cell) + 2*(len(current)+1) + 8
		if len(current) > 0 && neededIfAdded >= PageSize {
			flush()
		}
		current = append(current, cell)
		cellBytes += len(cell)
		maxRowID = item.rowID
	}
	flush()
	if len(groups) == 0 {
		groups = append(groups, cellGroup{cells: [][]byte{}, maxRowID: 0})
	}
	return groups, nil
}

type interiorChild struct {
	leftPage uint32
	maxRowID int64
}

func writeInteriorPage(page []byte, children []interiorChild, rightMost uint32) error {
	cells := make([][]byte, 0, len(children))
	totalBytes := 0
	for _, child := range children {
		cell := make([]byte, 4)
		binary.BigEndian.PutUint32(cell[0:4], child.leftPage)
		cell = append(cell, EncodeVarint(uint64(child.maxRowID))...)
		cells = append(cells, cell)
		totalBytes += len(cell)
	}
	pointerBytes := len(cells) * 2
	cellStart := PageSize - totalBytes
	pointerEnd := 12 + pointerBytes
	if cellStart <= pointerEnd {
		return fmt.Errorf("%w: interior page cannot hold %d children", ErrTooLarge, len(children))
	}
	page[0] = 0x05 // table b-tree interior
	binary.BigEndian.PutUint16(page[3:5], uint16(len(cells)))
	binary.BigEndian.PutUint16(page[5:7], uint16(cellStart))
	page[7] = 0
	binary.BigEndian.PutUint32(page[8:12], rightMost)
	cursor := cellStart
	for i, cell := range cells {
		pointerOffset := 12 + i*2
		binary.BigEndian.PutUint16(page[pointerOffset:pointerOffset+2], uint16(cursor))
		copy(page[cursor:cursor+len(cell)], cell)
		cursor += len(cell)
	}
	return nil
}

func writeDatabaseHeader(page []byte, pageCount uint32) {
	copy(page[0:16], "SQLite format 3\x00")
	// Encoded page size 1 means 65536.
	binary.BigEndian.PutUint16(page[16:18], 1)
	page[18] = 1 // file format write version: legacy rollback journal
	page[19] = 1 // file format read version
	page[20] = 0 // reserved bytes per page
	page[21] = 64
	page[22] = 32
	page[23] = 32
	binary.BigEndian.PutUint32(page[24:28], 1) // file change counter
	binary.BigEndian.PutUint32(page[28:32], pageCount)
	binary.BigEndian.PutUint32(page[32:36], 0) // first freelist page
	binary.BigEndian.PutUint32(page[36:40], 0) // freelist count
	binary.BigEndian.PutUint32(page[40:44], 1) // schema cookie
	binary.BigEndian.PutUint32(page[44:48], 4) // schema format
	binary.BigEndian.PutUint32(page[48:52], 0) // default page cache size
	binary.BigEndian.PutUint32(page[52:56], 0) // largest root b-tree page
	binary.BigEndian.PutUint32(page[56:60], 1) // text encoding UTF-8
	binary.BigEndian.PutUint32(page[60:64], 0) // user version
	binary.BigEndian.PutUint32(page[64:68], 0) // incremental vacuum
	binary.BigEndian.PutUint32(page[68:72], 0) // application id
	// bytes 72..92 reserved zero expansion space
	binary.BigEndian.PutUint32(page[92:96], 3046001) // version-valid-for
	binary.BigEndian.PutUint32(page[96:100], 3046001)
}

func writeLeafPage(page []byte, headerOffset int, cells [][]byte) error {
	total := 0
	for _, cell := range cells {
		total += len(cell)
	}
	pointerBytes := len(cells) * 2
	cellStart := PageSize - total
	pointerEnd := headerOffset + 8 + pointerBytes
	if cellStart <= pointerEnd {
		return fmt.Errorf("%w: need %d cell bytes plus %d pointer bytes; first offset %d <= header end %d",
			ErrTooLarge, total, pointerBytes, cellStart, pointerEnd)
	}
	if cellStart > 0xffff {
		return fmt.Errorf("cell start %d does not fit uint16", cellStart)
	}

	page[headerOffset] = 0x0d // table b-tree leaf
	page[headerOffset+1] = 0
	page[headerOffset+2] = 0 // first freeblock
	binary.BigEndian.PutUint16(page[headerOffset+3:headerOffset+5], uint16(len(cells)))
	binary.BigEndian.PutUint16(page[headerOffset+5:headerOffset+7], uint16(cellStart))
	page[headerOffset+7] = 0 // fragmented free bytes

	cursor := cellStart
	for i, cell := range cells {
		pointerOffset := headerOffset + 8 + i*2
		binary.BigEndian.PutUint16(page[pointerOffset:pointerOffset+2], uint16(cursor))
		copy(page[cursor:cursor+len(cell)], cell)
		cursor += len(cell)
	}
	return nil
}

// Value is a tiny record value used by the SQLite writer.
type Value struct {
	Text      string
	Integer   int64
	IsText    bool
	IsInteger bool
}

// EncodeCell encodes a table leaf cell with payload length and rowid.
func EncodeCell(rowid int64, record []byte) []byte {
	out := append([]byte{}, EncodeVarint(uint64(len(record)))...)
	out = append(out, EncodeVarint(uint64(rowid))...)
	out = append(out, record...)
	return out
}

// EncodeRecord encodes a SQLite table record.
func EncodeRecord(values []Value) ([]byte, error) {
	serialBytes := []byte{}
	body := []byte{}
	for _, value := range values {
		switch {
		case value.IsInteger:
			serial := integerSerial(value.Integer)
			serialBytes = append(serialBytes, EncodeVarint(uint64(serial))...)
			body = append(body, encodeInteger(value.Integer, serial)...)
		default:
			text := value.Text
			serial := int64(13 + 2*len(text))
			serialBytes = append(serialBytes, EncodeVarint(uint64(serial))...)
			body = append(body, text...)
		}
	}

	headerLength := 1 + len(serialBytes)
	lengthEncoding := EncodeVarint(uint64(headerLength))
	if len(lengthEncoding) > 1 {
		headerLength = len(lengthEncoding) + len(serialBytes)
		lengthEncoding = EncodeVarint(uint64(headerLength))
	}
	header := append(lengthEncoding, serialBytes...)
	return append(header, body...), nil
}

func integerSerial(v int64) int64 {
	switch {
	case v >= -128 && v <= 127:
		return 1
	case v >= -32768 && v <= 32767:
		return 2
	case v >= -(1<<23) && v <= (1<<23)-1:
		return 3
	case v >= -(1<<31) && v <= (1<<31)-1:
		return 4
	default:
		return 6
	}
}

func encodeInteger(v int64, serial int64) []byte {
	size := map[int64]int{1: 1, 2: 2, 3: 3, 4: 4, 6: 8}[serial]
	out := make([]byte, size)
	u := uint64(v)
	for i := size - 1; i >= 0; i-- {
		out[i] = byte(u & 0xff)
		u >>= 8
	}
	return out
}
