package loader

import (
	"fmt"
	"strings"

	"example.com/columnar/domain"
)

func loadV1(blob []byte) (domain.Project, error) {
	var doc v1Doc
	if err := decodeStrict(blob, &doc); err != nil {
		return domain.Project{}, err
	}
	if err := require(doc.SchemaVersion == "1.0", "schema_version", "schema_version must be 1.0"); err != nil {
		return domain.Project{}, err
	}
	if err := require(strings.TrimSpace(doc.Table) != "", "table", "table is required"); err != nil {
		return domain.Project{}, err
	}
	if doc.TimeUnit != "tick" {
		return domain.Project{}, domain.Errorf(domain.CodeUnitConflict, "time_unit", "v1 requires time_unit=tick, got %q", doc.TimeUnit)
	}
	if doc.SizeUnit != "byte" && doc.SizeUnit != "bytes" {
		return domain.Project{}, domain.Errorf(domain.CodeUnitConflict, "size_unit", "v1 byte fields require size_unit=byte, got %q", doc.SizeUnit)
	}
	if doc.TickMillis <= 0 {
		return domain.Project{}, domain.Errorf(domain.CodeUnitConflict, "tick_millis", "tick_millis must be positive")
	}
	if doc.DigestBits == 0 || doc.DigestBits > 64 {
		return domain.Project{}, domain.Errorf(domain.CodeDigestInvalid, "digest_bits", "digest width must be in [1,64], got %d", doc.DigestBits)
	}
	if len(doc.Families) == 0 {
		return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, "families", "at least one family is required")
	}
	p := domain.Project{
		SchemaVersion: Version1, Table: doc.Table, TimeUnit: "ms", SizeUnit: "bytes",
		TickMillis: 1, DigestBits: doc.DigestBits,
		DigestToleranceBits: doc.DigestToleranceBits, RowTolerance: doc.RowTolerance,
		Budget: domain.Budget{
			Workers: doc.Budget.Workers, TotalDecodeBufferBytes: doc.Budget.TotalDecodeBufferBytes,
			PerBatchDecodeBufferBytes: doc.Budget.PerBatchDecodeBufferBytes,
		},
	}
	seenFamilies := map[string]bool{}
	seenBatches := map[int]string{}
	for fi, f := range doc.Families {
		if strings.TrimSpace(f.Name) == "" {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, fmt.Sprintf("families[%d].name", fi), "family name is required")
		}
		if seenFamilies[f.Name] {
			return domain.Project{}, domain.Errorf(domain.CodeFamilyDuplicate, fmt.Sprintf("families[%d]", fi), "duplicate family %q", f.Name)
		}
		seenFamilies[f.Name] = true
		fam := domain.Family{ID: f.Name, Order: fi}
		for bi, b := range f.Batches {
			ctx := fmt.Sprintf("families[%d].batches[%d]", fi, bi)
			if strings.TrimSpace(b.Column) == "" {
				return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".column", "column is required")
			}
			if strings.TrimSpace(b.Encoding) == "" {
				return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".encoding", "encoding is required")
			}
			if b.DecodeBufferBytes <= 0 {
				return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".decode_buffer_bytes", "decode buffer must be positive")
			}
			d, err := domain.ParseDigestHex(b.Digest, doc.DigestBits)
			if err != nil {
				return domain.Project{}, domain.Errorf(domain.CodeDigestInvalid, ctx+".digest", "%v", err)
			}
			rows := b.RowEnd - b.RowStart
			nb := domain.Batch{
				ID: b.BatchID, Family: f.Name, Column: b.Column,
				RowStart: b.RowStart, RowEnd: b.RowEnd, Rows: rows,
				Encoding: b.Encoding, CompressedBytes: b.CompressedBytes, Digest: d,
				DecodeBufferBytes: b.DecodeBufferBytes, Status: domain.BatchActive,
				Invalidated: b.Invalidated, DependsOn: append([]int(nil), b.DependsOn...),
				SourceID: b.SourceBatch,
			}
			if err := addBatch(&p, seenBatches, nb, ctx); err != nil {
				return domain.Project{}, err
			}
			fam.BatchIDs = append(fam.BatchIDs, b.BatchID)
		}
		p.Families = append(p.Families, fam)
	}
	seenOps := map[int]bool{}
	last := 0
	for i, op := range doc.Operations {
		ctx := fmt.Sprintf("operations[%d]", i)
		if op.Seq <= last {
			return domain.Project{}, domain.Errorf(domain.CodeOperationOrder, ctx+".seq", "sequence %d must be greater than %d", op.Seq, last)
		}
		last = op.Seq
		at, err := checkedTicks(op.AtTick, doc.TickMillis, ctx+".at_tick")
		if err != nil {
			return domain.Project{}, err
		}
		o, err := opBase(op.Seq, op.BatchID, op.Kind, op.Digest, doc.DigestBits, op.DependsOn, op.SourceBatch, at, ctx)
		if err != nil {
			return domain.Project{}, err
		}
		o.NewBatchID = op.NewBatchID
		o.Family = op.Family
		o.Rows = op.Rows
		o.RowDelta = op.RowDelta
		o.CompressedBytes = op.CompressedBytes
		o.BytesDelta = op.BytesDelta
		o.Encoding = op.Encoding
		if o.Kind == domain.OpPatch {
			if op.From == nil || op.To == nil {
				return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".from", "patch requires from and to")
			}
			o.Range = &domain.RowRange{From: *op.From, To: *op.To}
		} else if op.From != nil || op.To != nil {
			return domain.Project{}, domain.Errorf(domain.CodeSchemaConflict, ctx, "range fields are only valid for patch")
		}
		if o.Kind == domain.OpCompact && o.NewBatchID == 0 {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".new_batch_id", "compact requires new_batch_id")
		}
		if seenOps[o.Index] {
			return domain.Project{}, domain.Errorf(domain.CodeOperationOrder, ctx, "duplicate operation %d", o.Index)
		}
		seenOps[o.Index] = true
		p.Operations = append(p.Operations, o)
	}
	for i, scan := range doc.Scans {
		ctx := fmt.Sprintf("scans[%d]", i)
		at, err := checkedTicks(scan.IssuedAtTick, doc.TickMillis, ctx+".issued_at_tick")
		if err != nil {
			return domain.Project{}, err
		}
		if scan.ScanID == "" {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".scan_id", "scan_id is required")
		}
		if scan.AfterOperation < 0 || scan.AfterOperation > len(doc.Operations) {
			return domain.Project{}, domain.Errorf(domain.CodeScanInvalid, ctx+".after_operation", "after_operation %d outside [0,%d]", scan.AfterOperation, len(doc.Operations))
		}
		p.Scans = append(p.Scans, domain.Scan{
			ID: scan.ScanID, Order: i, AfterOp: scan.AfterOperation, Family: scan.Family,
			Columns: domain.CanonicalColumns(scan.Columns),
			FromRow: scan.Predicate.From, ToRow: scan.Predicate.To, AtMillis: at,
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

func validateBudget(b domain.Budget) error {
	if b.Workers <= 0 {
		return domain.Errorf(domain.CodeBudgetWorkers, "budget.workers", "workers must be positive, got %d", b.Workers)
	}
	if b.TotalDecodeBufferBytes <= 0 || b.PerBatchDecodeBufferBytes <= 0 {
		return domain.Errorf(domain.CodeBudgetBuffer, "budget", "buffer limits must be positive")
	}
	return nil
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
	DependsOn         []int  `json:"depends_on"`
	SourceBatch       int    `json:"source_batch"`
}
type v1Operation struct {
	Seq             int    `json:"seq"`
	Kind            string `json:"kind"`
	BatchID         int    `json:"batch_id"`
	NewBatchID      int    `json:"new_batch_id"`
	Family          string `json:"family"`
	From            *int64 `json:"from"`
	To              *int64 `json:"to"`
	Rows            int64  `json:"rows"`
	RowDelta        int64  `json:"row_delta"`
	Digest          string `json:"digest"`
	CompressedBytes int64  `json:"compressed_bytes"`
	BytesDelta      int64  `json:"bytes_delta"`
	Encoding        string `json:"encoding"`
	DependsOn       []int  `json:"depends_on"`
	SourceBatch     int    `json:"source_batch"`
	AtTick          int64  `json:"at_tick"`
}
type v1Predicate struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}
type v1Scan struct {
	ScanID         string      `json:"scan_id"`
	AfterOperation int         `json:"after_operation"`
	Family         string      `json:"family"`
	Columns        []string    `json:"columns"`
	Predicate      v1Predicate `json:"predicate"`
	IssuedAtTick   int64       `json:"issued_at_tick"`
}
