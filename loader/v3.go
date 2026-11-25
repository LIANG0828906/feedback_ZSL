package loader

import (
	"fmt"
	"strings"

	"example.com/columnar/domain"
)

func loadV3(blob []byte) (domain.Project, error) {
	var doc v3Doc
	if err := decodeStrict(blob, &doc); err != nil {
		return domain.Project{}, err
	}
	if err := require(doc.FormatVersion == "3.0", "format_version", "format_version must be 3.0"); err != nil {
		return domain.Project{}, err
	}
	if strings.TrimSpace(doc.TableName) == "" {
		return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, "table_name", "table_name is required")
	}
	if doc.Clock.Unit != "tick" {
		return domain.Project{}, domain.Errorf(domain.CodeUnitConflict, "clock.unit", "v3 clock fields require tick, got %q", doc.Clock.Unit)
	}
	if doc.Storage.Unit != "byte" && doc.Storage.Unit != "bytes" {
		return domain.Project{}, domain.Errorf(domain.CodeUnitConflict, "storage.unit", "v3 byte fields require byte, got %q", doc.Storage.Unit)
	}
	if doc.Clock.TickMillis <= 0 {
		return domain.Project{}, domain.Errorf(domain.CodeUnitConflict, "clock.tick_millis", "tick_millis must be positive")
	}
	if doc.DigestSpec.Bits == 0 || doc.DigestSpec.Bits > 64 {
		return domain.Project{}, domain.Errorf(domain.CodeDigestInvalid, "digest_spec.bits", "digest width must be in [1,64]")
	}
	if len(doc.BatchTable) == 0 {
		return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, "batch_table", "at least one batch is required")
	}
	p := domain.Project{
		SchemaVersion: Version3, Table: doc.TableName, TimeUnit: "ms", SizeUnit: "bytes",
		TickMillis: 1, DigestBits: doc.DigestSpec.Bits,
		DigestToleranceBits: doc.DigestSpec.ToleranceBits, RowTolerance: doc.RowTolerance,
		Budget: domain.Budget{
			Workers: doc.Limits.Threads, TotalDecodeBufferBytes: doc.Limits.TotalBufferBytes,
			PerBatchDecodeBufferBytes: doc.Limits.PerBatchBufferBytes,
		},
	}
	seenBatches := map[int]string{}
	seenFamilyOrder := []string{}
	seenFamilies := map[string]bool{}
	batchIDsByFamily := map[string][]int{}
	for i, b := range doc.BatchTable {
		ctx := fmt.Sprintf("batch_table[%d]", i)
		d, err := domain.ParseDigestHex(b.Checksum, doc.DigestSpec.Bits)
		if err != nil {
			return domain.Project{}, domain.Errorf(domain.CodeDigestInvalid, ctx+".checksum", "%v", err)
		}
		if b.FamilyName == "" {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".family_name", "family_name is required")
		}
		if strings.TrimSpace(b.ColumnName) == "" {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".column_name", "column_name is required")
		}
		if strings.TrimSpace(b.Codec) == "" {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".codec", "codec is required")
		}
		if b.DecodeBytes <= 0 {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".decode_bytes", "decode buffer must be positive")
		}
		if !seenFamilies[b.FamilyName] {
			seenFamilies[b.FamilyName] = true
			seenFamilyOrder = append(seenFamilyOrder, b.FamilyName)
		}
		nb := domain.Batch{
			ID: b.BatchNo, Family: b.FamilyName, Column: b.ColumnName,
			RowStart: b.StartRow, RowEnd: b.EndRow, Rows: b.EndRow - b.StartRow,
			Encoding: b.Codec, CompressedBytes: b.StoredBytes, Digest: d,
			DecodeBufferBytes: b.DecodeBytes, Status: domain.BatchActive,
			Invalidated: b.Stale, DependsOn: append([]int(nil), b.Parents...),
			SourceID: b.SourceBatch,
		}
		if err := addBatch(&p, seenBatches, nb, ctx); err != nil {
			return domain.Project{}, err
		}
		batchIDsByFamily[b.FamilyName] = append(batchIDsByFamily[b.FamilyName], b.BatchNo)
	}
	for i, name := range seenFamilyOrder {
		p.Families = append(p.Families, domain.Family{ID: name, Order: i, BatchIDs: batchIDsByFamily[name]})
	}
	last := 0
	seenOps := map[int]bool{}
	for i, op := range doc.ChangeLog {
		ctx := fmt.Sprintf("change_log[%d]", i)
		if op.Position <= last {
			return domain.Project{}, domain.Errorf(domain.CodeOperationOrder, ctx+".position", "position %d must be greater than %d", op.Position, last)
		}
		last = op.Position
		at, err := checkedTicks(op.AtTick, doc.Clock.TickMillis, ctx+".at_tick")
		if err != nil {
			return domain.Project{}, err
		}
		o, err := opBase(op.Position, op.BatchNo, op.Change, op.Checksum, doc.DigestSpec.Bits, op.Parents, op.SourceBatch, at, ctx)
		if err != nil {
			return domain.Project{}, err
		}
		o.NewBatchID = op.ReplacementNo
		o.Family = op.Family
		o.Rows = op.RowCount
		o.RowDelta = op.RowsDelta
		o.CompressedBytes = op.StoredBytes
		o.BytesDelta = op.StoredDeltaBytes
		o.Encoding = op.Codec
		if o.Kind == domain.OpPatch {
			if op.RowWindow == nil || op.RowWindow.From == nil || op.RowWindow.To == nil {
				return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".row_window", "patch requires row_window.from/to")
			}
			o.Range = &domain.RowRange{From: *op.RowWindow.From, To: *op.RowWindow.To}
		} else if op.RowWindow != nil {
			return domain.Project{}, domain.Errorf(domain.CodeSchemaConflict, ctx+".row_window", "row_window is only valid for patch")
		}
		if o.Kind == domain.OpCompact && o.NewBatchID == 0 {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".replacement_no", "compact requires replacement_no")
		}
		if seenOps[o.Index] {
			return domain.Project{}, domain.Errorf(domain.CodeOperationOrder, ctx, "duplicate operation %d", o.Index)
		}
		seenOps[o.Index] = true
		p.Operations = append(p.Operations, o)
	}
	for i, scan := range doc.ScanPlan {
		ctx := fmt.Sprintf("scan_plan[%d]", i)
		if scan.ScanID == "" {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".scan_id", "scan_id is required")
		}
		if scan.AfterPosition < 0 || scan.AfterPosition > len(doc.ChangeLog) {
			return domain.Project{}, domain.Errorf(domain.CodeScanInvalid, ctx+".after_position", "after_position %d outside [0,%d]", scan.AfterPosition, len(doc.ChangeLog))
		}
		if scan.Window.Lo == nil || scan.Window.Hi == nil {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".window", "scan window lo/hi are required")
		}
		at, err := checkedTicks(scan.AtTick, doc.Clock.TickMillis, ctx+".at_tick")
		if err != nil {
			return domain.Project{}, err
		}
		p.Scans = append(p.Scans, domain.Scan{
			ID: scan.ScanID, Order: i, AfterOp: scan.AfterPosition, Family: scan.Family,
			Columns: domain.CanonicalColumns(scan.Columns),
			FromRow: *scan.Window.Lo, ToRow: *scan.Window.Hi, AtMillis: at,
		})
	}
	if err := validateBudget(p.Budget); err != nil {
		return domain.Project{}, err
	}
	if err := applyFamilyPredecessors(&p); err != nil {
		return domain.Project{}, err
	}
	return p, nil
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
	Parents     []int  `json:"parents"`
	SourceBatch int    `json:"source_batch"`
}
type v3OpWindow struct {
	From *int64 `json:"from"`
	To   *int64 `json:"to"`
}
type v3ScanWindow struct {
	Lo *int64 `json:"lo"`
	Hi *int64 `json:"hi"`
}
type v3Op struct {
	Position         int         `json:"position"`
	Change           string      `json:"change"`
	BatchNo          int         `json:"batch_no"`
	ReplacementNo    int         `json:"replacement_no"`
	Family           string      `json:"family"`
	RowWindow        *v3OpWindow `json:"row_window"`
	RowCount         int64       `json:"row_count"`
	RowsDelta        int64       `json:"rows_delta"`
	Checksum         string      `json:"checksum"`
	StoredBytes      int64       `json:"stored_bytes"`
	StoredDeltaBytes int64       `json:"stored_delta_bytes"`
	Codec            string      `json:"codec"`
	Parents          []int       `json:"parents"`
	SourceBatch      int         `json:"source_batch"`
	AtTick           int64       `json:"at_tick"`
}
type v3Scan struct {
	ScanID        string       `json:"scan_id"`
	AfterPosition int          `json:"after_position"`
	Family        string       `json:"family"`
	Columns       []string     `json:"columns"`
	Window        v3ScanWindow `json:"window"`
	AtTick        int64        `json:"at_tick"`
}
