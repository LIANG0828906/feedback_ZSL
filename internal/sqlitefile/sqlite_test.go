package sqlitefile

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeDecodeMultiPageAndOverflow(t *testing.T) {
	rows := make([][]any, 0, 40)
	for index := 0; index < 40; index++ {
		payload, _ := json.Marshal(map[string]any{
			"index": index,
			"text":  strings.Repeat("abcdef0123456789", 300+index),
		})
		rows = append(rows, []any{int64(index), "row-" + string(rune('A'+index%26)), string(payload)})
	}
	data, err := Encode([]Table{
		{
			Name: "payloads",
			Columns: []Column{
				{Name: "sequence", Type: "INTEGER"},
				{Name: "name", Type: "TEXT"},
				{Name: "body", Type: "TEXT"},
			},
			Rows: rows,
		},
		{
			Name: "meta",
			Columns: []Column{
				{Name: "id", Type: "INTEGER"},
				{Name: "value", Type: "TEXT"},
			},
			Rows: [][]any{{int64(1), "complete"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
		t.Fatal("missing SQLite magic")
	}
	if binary.BigEndian.Uint32(data[52:56]) != 0 {
		t.Fatal("largest-root-page/auto-vacuum field must be zero for this subset")
	}
	database, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	table := database.Tables["payloads"]
	if table == nil {
		t.Fatal("payloads table missing")
	}
	if len(table.Rows) != len(rows) {
		t.Fatalf("decoded %d rows, want %d", len(table.Rows), len(rows))
	}
	bodyIndex := table.Column("body")
	for index, row := range table.Rows {
		value, ok := row.String(bodyIndex)
		if !ok {
			t.Fatalf("row %d body is not text", index)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(value), &decoded); err != nil {
			t.Fatalf("row %d body JSON: %v", index, err)
		}
		if int(decoded["index"].(float64)) != index {
			t.Fatalf("row %d index mismatch", index)
		}
	}
	if _, err := Decode(data[:len(data)/2]); err == nil {
		t.Fatal("truncated database decoded successfully")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	values := []any{nil, int64(-42), "hello", []byte{0, 1, 2, 3}, int64(1 << 40)}
	payload, err := encodeRecord(values)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRecord(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(values) {
		t.Fatalf("decoded %d values, want %d", len(decoded), len(values))
	}
	if decoded[1].(int64) != -42 || decoded[2].(string) != "hello" || decoded[4].(int64) != 1<<40 {
		t.Fatalf("unexpected decoded values: %#v", decoded)
	}
}
