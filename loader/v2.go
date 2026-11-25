package loader

import (
	"fmt"
	"strings"

	"example.com/columnar/domain"
)

func loadV2(blob []byte) (domain.Project, error) {
	var doc v2Doc
	if err := decodeStrict(blob, &doc); err != nil {
		return domain.Project{}, err
	}
	if err := require(doc.Schema == "2.0", "schema", "schema must be 2.0"); err != nil {
		return domain.Project{}, err
	}
	if strings.TrimSpace(doc.TableName) == "" {
		return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, "table_name", "table_name is required")
	}
	if doc.Units.Time != "ms" {
		return domain.Project{}, domain.Errorf(domain.CodeUnitConflict, "units.time", "v2 requires ms, got %q", doc.Units.Time)
	}
	if doc.Units.Size != "KiB" && doc.Units.Size != "kib" {
		return domain.Project{}, domain.Errorf(domain.CodeUnitConflict, "units.size", "v2 KiB fields require units.size=KiB, got %q", doc.Units.Size)
	}
	if doc.Checksum.Bits == 0 || doc.Checksum.Bits > 64 {
		return domain.Project{}, domain.Errorf(domain.CodeDigestInvalid, "checksum.bits", "digest width must be in [1,64]")
	}
	if len(doc.Families) == 0 {
		return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, "families", "at least one family is required")
	}
	total, err := checkedKiB(doc.Limits.DecodeBufferKiB, "limits.decode_buffer_kib")
	if err != nil {
		return domain.Project{}, err
	}
	perBatch, err := checkedKiB(doc.Limits.BatchDecodeKiB, "limits.batch_decode_kib")
	if err != nil {
		return domain.Project{}, err
	}
	p := domain.Project{
		SchemaVersion: Version2, Table: doc.TableName, TimeUnit: "ms", SizeUnit: "bytes",
		TickMillis: 1, DigestBits: doc.Checksum.Bits,
		DigestToleranceBits: doc.Checksum.ToleranceBits, RowTolerance: doc.RowTolerance,
		Budget: domain.Budget{Workers: doc.Limits.Workers, TotalDecodeBufferBytes: total, PerBatchDecodeBufferBytes: perBatch},
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
		for bi, b := range f.Segments {
			ctx := fmt.Sprintf("families[%d].segments[%d]", fi, bi)
			if strings.TrimSpace(b.Column) == "" {
				return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".column", "column is required")
			}
			if strings.TrimSpace(b.Codec) == "" {
				return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".codec", "codec is required")
			}
			if b.DecodeKiB <= 0 {
				return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".decode_kib", "decode buffer must be positive")
			}
			d, err := domain.ParseDigestHex(b.Checksum, doc.Checksum.Bits)
			if err != nil {
				return domain.Project{}, domain.Errorf(domain.CodeDigestInvalid, ctx+".checksum", "%v", err)
			}
			stored, err := checkedKiB(b.StoredKiB, ctx+".stored_kib")
			if err != nil {
				return domain.Project{}, err
			}
			decode, err := checkedKiB(b.DecodeKiB, ctx+".decode_kib")
			if err != nil {
				return domain.Project{}, err
			}
			nb := domain.Batch{
				ID: b.BatchNo, Family: f.Name, Column: b.Column,
				RowStart: b.Span.Begin, RowEnd: b.Span.End, Rows: b.Rows,
				Encoding: b.Codec, CompressedBytes: stored, Digest: d,
				DecodeBufferBytes: decode, Status: domain.BatchActive,
				DependsOn: append([]int(nil), b.Deps...), SourceID: b.Origin,
			}
			if err := addBatch(&p, seenBatches, nb, ctx); err != nil {
				return domain.Project{}, err
			}
			fam.BatchIDs = append(fam.BatchIDs, b.BatchNo)
		}
		p.Families = append(p.Families, fam)
	}
	last := 0
	seenOps := map[int]bool{}
	for i, op := range doc.Mutations {
		ctx := fmt.Sprintf("mutations[%d]", i)
		if op.Ordinal <= last {
			return domain.Project{}, domain.Errorf(domain.CodeOperationOrder, ctx+".ordinal", "ordinal %d must be greater than %d", op.Ordinal, last)
		}
		last = op.Ordinal
		if op.AtMS < 0 {
			return domain.Project{}, domain.Errorf(domain.CodeSchemaConflict, ctx+".at_ms", "negative timestamp")
		}
		o, err := opBase(op.Ordinal, op.BatchNo, op.Action, op.Checksum, doc.Checksum.Bits, op.Deps, op.Origin, op.AtMS, ctx)
		if err != nil {
			return domain.Project{}, err
		}
		o.NewBatchID = op.ReplacementNo
		o.Family = op.Family
		o.Rows = op.RowCount
		o.RowDelta = op.RowDelta
		o.Encoding = op.Codec
		o.CompressedBytes, err = checkedKiB(op.StoredKiB, ctx+".stored_kib")
		if err != nil {
			return domain.Project{}, err
		}
		o.BytesDelta, err = checkedSignedKiB(op.StoredDeltaKiB, ctx+".stored_delta_kib")
		if err != nil {
			return domain.Project{}, err
		}
		if o.Kind == domain.OpPatch {
			if op.Range == nil || op.Range.From == nil || op.Range.To == nil {
				return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".range", "patch requires range.from/range.to")
			}
			o.Range = &domain.RowRange{From: *op.Range.From, To: *op.Range.To}
		} else if op.Range != nil {
			return domain.Project{}, domain.Errorf(domain.CodeSchemaConflict, ctx+".range", "range is only valid for patch")
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
	for i, scan := range doc.Reads {
		ctx := fmt.Sprintf("reads[%d]", i)
		if scan.ID == "" {
			return domain.Project{}, domain.NewError(domain.CodeSchemaMissing, ctx+".id", "scan id is required")
		}
		if scan.After < 0 || scan.After > len(doc.Mutations) {
			return domain.Project{}, domain.Errorf(domain.CodeScanInvalid, ctx+".after", "after %d outside [0,%d]", scan.After, len(doc.Mutations))
		}
		if scan.IssuedMS < 0 {
			return domain.Project{}, domain.Errorf(domain.CodeSchemaConflict, ctx+".issued_ms", "negative timestamp")
		}
		p.Scans = append(p.Scans, domain.Scan{
			ID: scan.ID, Order: i, AfterOp: scan.After, Family: scan.Family,
			Columns: domain.CanonicalColumns(scan.Projection),
			FromRow: scan.Predicate.Lo, ToRow: scan.Predicate.Hi, AtMillis: scan.IssuedMS,
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
	Begin int64 `json:"begin"`
	End   int64 `json:"end"`
}
type v2Range struct {
	From *int64 `json:"from"`
	To   *int64 `json:"to"`
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
	Deps      []int  `json:"deps"`
	Origin    int    `json:"origin"`
}
type v2Op struct {
	Ordinal        int      `json:"ordinal"`
	Action         string   `json:"action"`
	BatchNo        int      `json:"batch_no"`
	ReplacementNo  int      `json:"replacement_no"`
	Family         string   `json:"family"`
	Range          *v2Range `json:"range"`
	RowCount       int64    `json:"row_count"`
	RowDelta       int64    `json:"row_delta"`
	Checksum       string   `json:"checksum"`
	StoredKiB      int64    `json:"stored_kib"`
	StoredDeltaKiB int64    `json:"stored_delta_kib"`
	Codec          string   `json:"codec"`
	Deps           []int    `json:"deps"`
	Origin         int      `json:"origin"`
	AtMS           int64    `json:"at_ms"`
}
type v2Predicate struct {
	Lo int64 `json:"lo"`
	Hi int64 `json:"hi"`
}
type v2Scan struct {
	ID         string      `json:"id"`
	After      int         `json:"after"`
	Family     string      `json:"family"`
	Projection []string    `json:"projection"`
	Predicate  v2Predicate `json:"predicate"`
	IssuedMS   int64       `json:"issued_ms"`
}
