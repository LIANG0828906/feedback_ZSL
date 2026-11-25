package generator

import (
	"encoding/json"
	"fmt"

	"example.com/columnar/domain"
)

func encodeV1(seed int64, p domain.Project, ops []logicalOp, scans []domain.Scan, truth Truth) ([]byte, error) {
	doc := v1Doc{
		SchemaVersion: "1.0", Table: p.Table, TimeUnit: "tick", TickMillis: p.TickMillis,
		SizeUnit: "byte", DigestBits: p.DigestBits,
		DigestToleranceBits: p.DigestToleranceBits, RowTolerance: p.RowTolerance,
		Budget: v1Budget{
			Workers:                   p.Budget.Workers,
			TotalDecodeBufferBytes:    p.Budget.TotalDecodeBufferBytes,
			PerBatchDecodeBufferBytes: p.Budget.PerBatchDecodeBufferBytes,
		},
	}
	byFamily := map[string][]domain.Batch{}
	for _, b := range p.Batches {
		byFamily[b.Family] = append(byFamily[b.Family], b)
	}
	for _, f := range p.Families {
		vf := v1Family{Name: f.ID}
		for _, id := range f.BatchIDs {
			b, ok := findBatch(p.Batches, id)
			if !ok {
				return nil, fmt.Errorf("missing batch %d", id)
			}
			vf.Batches = append(vf.Batches, v1Batch{
				BatchID: b.ID, Column: b.Column, RowStart: b.RowStart, RowEnd: b.RowEnd,
				Encoding: b.Encoding, CompressedBytes: b.CompressedBytes,
				Digest: domain.FormatDigest(b.Digest), DecodeBufferBytes: b.DecodeBufferBytes,
				Invalidated: false, DependsOn: append([]int(nil), b.DependsOn...),
				SourceBatch: b.SourceID,
			})
		}
		doc.Families = append(doc.Families, vf)
	}
	for i, op := range ops {
		vop, err := encodeV1Op(seed, op, truth.Operations[i])
		if err != nil {
			return nil, err
		}
		doc.Operations = append(doc.Operations, vop)
	}
	for _, scan := range scans {
		doc.Scans = append(doc.Scans, v1Scan{
			ScanID: scan.ID, AfterOperation: scan.AfterOp, Family: scan.Family,
			Columns:      append([]string(nil), scan.Columns...),
			Predicate:    v1Predicate{From: scan.FromRow, To: scan.ToRow},
			IssuedAtTick: scan.AtMillis / p.TickMillis,
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func encodeV1Op(seed int64, op logicalOp, post OperationTruth) (v1Operation, error) {
	v := v1Operation{
		Seq: op.index, BatchID: op.target, NewBatchID: op.newBatchID, Family: op.family,
		Rows: op.rows, RowDelta: op.rowDelta, Digest: domain.FormatDigest(op.digest),
		CompressedBytes: op.bytes, BytesDelta: op.bytesDelta, Encoding: op.encoding,
		SourceBatch: op.sourceID, AtTick: op.atMillis / TickMillis,
	}
	if op.setDeps {
		v.DependsOn = append([]int(nil), op.deps...)
	}
	switch op.kind {
	case domain.OpPatch:
		// Schema v1 has no partial update expression. The same logical result is
		// represented by a whole-batch replacement of the post-patch state.
		b, ok := findBatch(post.Batches, op.target)
		if !ok {
			return v, fmt.Errorf("post-patch batch %d not found", op.target)
		}
		v.Kind = string(domain.OpReplace)
		v.Rows = b.Rows
		v.Digest = domain.FormatDigest(b.Digest)
		v.CompressedBytes = b.CompressedBytes
		v.Encoding = b.Encoding
		v.RowDelta = 0
		v.BytesDelta = 0
	case domain.OpReplace:
		v.Kind = string(domain.OpReplace)
	case domain.OpDelete:
		v.Kind = string(domain.OpDelete)
	case domain.OpCompact:
		v.Kind = string(domain.OpCompact)
	default:
		return v, fmt.Errorf("unknown op kind %s", op.kind)
	}
	return v, nil
}

func encodeV2(seed int64, p domain.Project, ops []logicalOp, scans []domain.Scan, _ Truth) ([]byte, error) {
	doc := v2Doc{
		Schema: "2.0", TableName: p.Table,
		Units:        v2Units{Time: "ms", Size: "KiB"},
		Checksum:     v2Checksum{Bits: p.DigestBits, ToleranceBits: p.DigestToleranceBits},
		RowTolerance: p.RowTolerance,
		Limits:       v2Limits{Workers: p.Budget.Workers, DecodeBufferKiB: mustKiB(p.Budget.TotalDecodeBufferBytes), BatchDecodeKiB: mustKiB(p.Budget.PerBatchDecodeBufferBytes)},
	}
	for _, f := range p.Families {
		vf := v2Family{Name: f.ID}
		for _, id := range f.BatchIDs {
			b, ok := findBatch(p.Batches, id)
			if !ok {
				return nil, fmt.Errorf("missing batch %d", id)
			}
			kb, err := toKiB(b.CompressedBytes)
			if err != nil {
				return nil, err
			}
			db, err := toKiB(b.DecodeBufferBytes)
			if err != nil {
				return nil, err
			}
			vf.Segments = append(vf.Segments, v2Batch{
				BatchNo: b.ID, Column: b.Column, Span: v2Span{Begin: b.RowStart, End: b.RowEnd},
				Rows: b.Rows, Codec: b.Encoding, StoredKiB: kb, Checksum: domain.FormatDigest(b.Digest),
				DecodeKiB: db, Deps: append([]int(nil), b.DependsOn...), Origin: b.SourceID,
			})
		}
		doc.Families = append(doc.Families, vf)
	}
	for _, op := range ops {
		vop, err := encodeV2Op(op)
		if err != nil {
			return nil, err
		}
		doc.Mutations = append(doc.Mutations, vop)
	}
	for _, scan := range scans {
		doc.Reads = append(doc.Reads, v2Scan{
			ID: scan.ID, After: scan.AfterOp, Family: scan.Family,
			Projection: append([]string(nil), scan.Columns...),
			Predicate:  v2Predicate{Lo: scan.FromRow, Hi: scan.ToRow},
			IssuedMS:   scan.AtMillis,
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func encodeV2Op(op logicalOp) (v2Op, error) {
	v := v2Op{
		Ordinal: op.index, Action: string(op.kind), BatchNo: op.target,
		ReplacementNo: op.newBatchID, Family: op.family,
		RowCount: op.rows, RowDelta: op.rowDelta, Checksum: domain.FormatDigest(op.digest),
		StoredKiB: mustKiB(op.bytes), StoredDeltaKiB: mustKiB(op.bytesDelta),
		Codec: op.encoding, Origin: op.sourceID, AtMS: op.atMillis,
	}
	if op.setDeps {
		v.Deps = append([]int(nil), op.deps...)
	}
	if op.rowRange != nil {
		from, to := op.rowRange.From, op.rowRange.To
		v.Range = &v2Span{From: &from, To: &to}
	}
	return v, nil
}

func encodeV3(seed int64, p domain.Project, ops []logicalOp, scans []domain.Scan, _ Truth) ([]byte, error) {
	doc := v3Doc{
		FormatVersion: "3.0", TableName: p.Table,
		Clock:        v3Clock{Unit: "tick", TickMillis: p.TickMillis},
		Storage:      v3Storage{Unit: "byte", BatchDecodeLimitBytes: p.Budget.PerBatchDecodeBufferBytes, TotalDecodeBudgetBytes: p.Budget.TotalDecodeBufferBytes},
		DigestSpec:   v3DigestSpec{Bits: p.DigestBits, ToleranceBits: p.DigestToleranceBits},
		RowTolerance: p.RowTolerance,
		Limits:       v3Limits{Threads: p.Budget.Workers, TotalBufferBytes: p.Budget.TotalDecodeBufferBytes, PerBatchBufferBytes: p.Budget.PerBatchDecodeBufferBytes},
	}
	for _, f := range p.Families {
		for _, id := range f.BatchIDs {
			b, ok := findBatch(p.Batches, id)
			if !ok {
				return nil, fmt.Errorf("missing batch %d", id)
			}
			doc.BatchTable = append(doc.BatchTable, v3Batch{
				BatchNo: b.ID, FamilyName: b.Family, ColumnName: b.Column,
				StartRow: b.RowStart, EndRow: b.RowEnd, Codec: b.Encoding,
				StoredBytes: b.CompressedBytes, Checksum: domain.FormatDigest(b.Digest),
				DecodeBytes: b.DecodeBufferBytes, Stale: false,
				Parents: append([]int(nil), b.DependsOn...), SourceBatch: b.SourceID,
			})
		}
	}
	for _, op := range ops {
		v := v3Op{
			Position: op.index, Change: string(op.kind), BatchNo: op.target,
			ReplacementNo: op.newBatchID, Family: op.family,
			RowCount: op.rows, RowsDelta: op.rowDelta, Checksum: domain.FormatDigest(op.digest),
			StoredBytes: op.bytes, StoredDeltaBytes: op.bytesDelta, Codec: op.encoding,
			SourceBatch: op.sourceID, AtTick: op.atMillis / p.TickMillis,
		}
		if op.setDeps {
			v.Parents = append([]int(nil), op.deps...)
		}
		if op.rowRange != nil {
			from, to := op.rowRange.From, op.rowRange.To
			v.RowWindow = &v3Window{From: &from, To: &to}
		}
		doc.ChangeLog = append(doc.ChangeLog, v)
	}
	for _, scan := range scans {
		doc.ScanPlan = append(doc.ScanPlan, v3Scan{
			ScanID: scan.ID, AfterPosition: scan.AfterOp, Family: scan.Family,
			Columns: append([]string(nil), scan.Columns...),
			Window:  v3Window{Lo: &scan.FromRow, Hi: &scan.ToRow},
			AtTick:  scan.AtMillis / p.TickMillis,
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func findBatch(batches []domain.Batch, id int) (domain.Batch, bool) {
	for _, b := range batches {
		if b.ID == id {
			return b, true
		}
	}
	return domain.Batch{}, false
}

func toKiB(v int64) (int64, error) {
	if v < 0 || v%1024 != 0 {
		return 0, fmt.Errorf("byte value %d is not exactly representable in KiB", v)
	}
	return v / 1024, nil
}

func mustKiB(v int64) int64 {
	if v < 0 || v%1024 != 0 {
		panic(fmt.Sprintf("non-KiB value %d in deterministic fixture", v))
	}
	return v / 1024
}

type v1Doc struct {
	SchemaVersion       string        `json:"schema_version"`
	Table               string        `json:"table"`
	TimeUnit            string        `json:"time_unit"`
	TickMillis          int64         `json:"tick_millis"`
	SizeUnit            string        `json:"size_unit"`
	DigestBits          uint          `json:"digest_bits"`
	DigestToleranceBits uint          `json:"digest_tolerance_bits"`
	RowTolerance        int64         `json:"row_tolerance"`
	Budget              v1Budget      `json:"budget"`
	Families            []v1Family    `json:"families"`
	Operations          []v1Operation `json:"operations"`
	Scans               []v1Scan      `json:"scans"`
}
type v1Budget struct {
	Workers                   int   `json:"workers"`
	TotalDecodeBufferBytes    int64 `json:"total_decode_buffer_bytes"`
	PerBatchDecodeBufferBytes int64 `json:"per_batch_decode_buffer_bytes"`
}
type v1Family struct {
	Name    string    `json:"name"`
	Batches []v1Batch `json:"batches"`
}
type v1Batch struct {
	BatchID           int    `json:"batch_id"`
	Column            string `json:"column"`
	RowStart          int64  `json:"row_start"`
	RowEnd            int64  `json:"row_end"`
	Encoding          string `json:"encoding"`
	CompressedBytes   int64  `json:"compressed_bytes"`
	Digest            string `json:"digest"`
	DecodeBufferBytes int64  `json:"decode_buffer_bytes"`
	Invalidated       bool   `json:"invalidated"`
	DependsOn         []int  `json:"depends_on,omitempty"`
	SourceBatch       int    `json:"source_batch,omitempty"`
}
type v1Operation struct {
	Seq             int    `json:"seq"`
	Kind            string `json:"kind"`
	BatchID         int    `json:"batch_id"`
	NewBatchID      int    `json:"new_batch_id,omitempty"`
	Family          string `json:"family,omitempty"`
	From            *int64 `json:"from,omitempty"`
	To              *int64 `json:"to,omitempty"`
	Rows            int64  `json:"rows,omitempty"`
	RowDelta        int64  `json:"row_delta,omitempty"`
	Digest          string `json:"digest"`
	CompressedBytes int64  `json:"compressed_bytes,omitempty"`
	BytesDelta      int64  `json:"bytes_delta,omitempty"`
	Encoding        string `json:"encoding,omitempty"`
	DependsOn       []int  `json:"depends_on,omitempty"`
	SourceBatch     int    `json:"source_batch,omitempty"`
	AtTick          int64  `json:"at_tick"`
}
type v1Predicate struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}
type v1Scan struct {
	ScanID         string      `json:"scan_id"`
	AfterOperation int         `json:"after_operation"`
	Family         string      `json:"family,omitempty"`
	Columns        []string    `json:"columns"`
	Predicate      v1Predicate `json:"predicate"`
	IssuedAtTick   int64       `json:"issued_at_tick"`
}

type v2Doc struct {
	Schema       string     `json:"schema"`
	TableName    string     `json:"table_name"`
	Units        v2Units    `json:"units"`
	Checksum     v2Checksum `json:"checksum"`
	RowTolerance int64      `json:"row_tolerance"`
	Limits       v2Limits   `json:"limits"`
	Families     []v2Family `json:"families"`
	Mutations    []v2Op     `json:"mutations"`
	Reads        []v2Scan   `json:"reads"`
}
type v2Units struct {
	Time string `json:"time"`
	Size string `json:"size"`
}
type v2Checksum struct {
	Bits          uint `json:"bits"`
	ToleranceBits uint `json:"tolerance_bits"`
}
type v2Limits struct {
	Workers         int   `json:"workers"`
	DecodeBufferKiB int64 `json:"decode_buffer_kib"`
	BatchDecodeKiB  int64 `json:"batch_decode_kib"`
}
type v2Family struct {
	Name     string    `json:"name"`
	Segments []v2Batch `json:"segments"`
}
type v2Span struct {
	Begin int64  `json:"begin,omitempty"`
	End   int64  `json:"end,omitempty"`
	From  *int64 `json:"from,omitempty"`
	To    *int64 `json:"to,omitempty"`
}
type v2Batch struct {
	BatchNo   int    `json:"batch_no"`
	Column    string `json:"column"`
	Span      v2Span `json:"span"`
	Rows      int64  `json:"rows"`
	Codec     string `json:"codec"`
	StoredKiB int64  `json:"stored_kib"`
	Checksum  string `json:"checksum"`
	DecodeKiB int64  `json:"decode_kib"`
	Deps      []int  `json:"deps,omitempty"`
	Origin    int    `json:"origin,omitempty"`
}
type v2Op struct {
	Ordinal        int     `json:"ordinal"`
	Action         string  `json:"action"`
	BatchNo        int     `json:"batch_no"`
	ReplacementNo  int     `json:"replacement_no,omitempty"`
	Family         string  `json:"family,omitempty"`
	Range          *v2Span `json:"range,omitempty"`
	RowCount       int64   `json:"row_count,omitempty"`
	RowDelta       int64   `json:"row_delta,omitempty"`
	Checksum       string  `json:"checksum"`
	StoredKiB      int64   `json:"stored_kib,omitempty"`
	StoredDeltaKiB int64   `json:"stored_delta_kib,omitempty"`
	Codec          string  `json:"codec,omitempty"`
	Deps           []int   `json:"deps,omitempty"`
	Origin         int     `json:"origin,omitempty"`
	AtMS           int64   `json:"at_ms"`
}
type v2Predicate struct {
	Lo int64 `json:"lo"`
	Hi int64 `json:"hi"`
}
type v2Scan struct {
	ID         string      `json:"id"`
	After      int         `json:"after"`
	Family     string      `json:"family,omitempty"`
	Projection []string    `json:"projection"`
	Predicate  v2Predicate `json:"predicate"`
	IssuedMS   int64       `json:"issued_ms"`
}

type v3Doc struct {
	FormatVersion string       `json:"format_version"`
	TableName     string       `json:"table_name"`
	Clock         v3Clock      `json:"clock"`
	Storage       v3Storage    `json:"storage"`
	DigestSpec    v3DigestSpec `json:"digest_spec"`
	RowTolerance  int64        `json:"row_tolerance"`
	Limits        v3Limits     `json:"limits"`
	BatchTable    []v3Batch    `json:"batch_table"`
	ChangeLog     []v3Op       `json:"change_log"`
	ScanPlan      []v3Scan     `json:"scan_plan"`
}
type v3Clock struct {
	Unit       string `json:"unit"`
	TickMillis int64  `json:"tick_millis"`
}
type v3Storage struct {
	Unit                   string `json:"unit"`
	BatchDecodeLimitBytes  int64  `json:"batch_decode_limit_bytes"`
	TotalDecodeBudgetBytes int64  `json:"total_decode_budget_bytes"`
}
type v3DigestSpec struct {
	Bits          uint `json:"bits"`
	ToleranceBits uint `json:"tolerance_bits"`
}
type v3Limits struct {
	Threads             int   `json:"threads"`
	TotalBufferBytes    int64 `json:"total_buffer_bytes"`
	PerBatchBufferBytes int64 `json:"per_batch_buffer_bytes"`
}
type v3Batch struct {
	BatchNo     int    `json:"batch_no"`
	FamilyName  string `json:"family_name"`
	ColumnName  string `json:"column_name"`
	StartRow    int64  `json:"start_row"`
	EndRow      int64  `json:"end_row"`
	Codec       string `json:"codec"`
	StoredBytes int64  `json:"stored_bytes"`
	Checksum    string `json:"checksum"`
	DecodeBytes int64  `json:"decode_bytes"`
	Stale       bool   `json:"stale"`
	Parents     []int  `json:"parents,omitempty"`
	SourceBatch int    `json:"source_batch,omitempty"`
}
type v3Window struct {
	From *int64 `json:"from,omitempty"`
	To   *int64 `json:"to,omitempty"`
	Lo   *int64 `json:"lo,omitempty"`
	Hi   *int64 `json:"hi,omitempty"`
}
type v3Op struct {
	Position         int       `json:"position"`
	Change           string    `json:"change"`
	BatchNo          int       `json:"batch_no"`
	ReplacementNo    int       `json:"replacement_no,omitempty"`
	Family           string    `json:"family,omitempty"`
	RowWindow        *v3Window `json:"row_window,omitempty"`
	RowCount         int64     `json:"row_count,omitempty"`
	RowsDelta        int64     `json:"rows_delta,omitempty"`
	Checksum         string    `json:"checksum"`
	StoredBytes      int64     `json:"stored_bytes,omitempty"`
	StoredDeltaBytes int64     `json:"stored_delta_bytes,omitempty"`
	Codec            string    `json:"codec,omitempty"`
	Parents          []int     `json:"parents,omitempty"`
	SourceBatch      int       `json:"source_batch,omitempty"`
	AtTick           int64     `json:"at_tick"`
}
type v3Scan struct {
	ScanID        string   `json:"scan_id"`
	AfterPosition int      `json:"after_position"`
	Family        string   `json:"family,omitempty"`
	Columns       []string `json:"columns"`
	Window        v3Window `json:"window"`
	AtTick        int64    `json:"at_tick"`
}
