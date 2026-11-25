// Package loader normalizes the three local schema versions into the canonical
// domain model. Decoding is strict: unknown fields and unsupported unit/field
// combinations produce stable business codes with a JSON path context.
package loader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"example.com/columnar/domain"
)

const (
	Version1 = "v1"
	Version2 = "v2"
	Version3 = "v3"
)

// Load detects and loads one schema document.
func Load(blob []byte) (domain.Project, error) {
	version, err := DetectVersion(blob)
	if err != nil {
		return domain.Project{}, err
	}
	switch version {
	case Version1:
		return loadV1(blob)
	case Version2:
		return loadV2(blob)
	case Version3:
		return loadV3(blob)
	default:
		return domain.Project{}, domain.Errorf(domain.CodeSchemaVersion, "schema_version", "unsupported schema version %q", version)
	}
}

// LoadAll loads all documents and returns a structural cross-version conflict
// as E_CROSS_VERSION_CONFLICT. Deeper operation-result equivalence is verified
// by the engine's comparison helper.
func LoadAll(blobs ...[]byte) ([]domain.Project, error) {
	projects := make([]domain.Project, 0, len(blobs))
	for i, blob := range blobs {
		p, err := Load(blob)
		if err != nil {
			return nil, err
		}
		p.SchemaVersion = fmt.Sprintf("%s#%d", p.SchemaVersion, i)
		projects = append(projects, p)
	}
	if len(projects) > 1 {
		if err := CompareCanonical(projects); err != nil {
			return nil, err
		}
	}
	for i := range projects {
		projects[i].SchemaVersion = strings.Split(projects[i].SchemaVersion, "#")[0]
	}
	return projects, nil
}

// DetectVersion reads only the discriminator fields.
func DetectVersion(blob []byte) (string, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(blob, &head); err != nil {
		return "", domain.WrapError(domain.CodeSchemaConflict, "$", err)
	}
	if raw, ok := head["schema_version"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", domain.WrapError(domain.CodeSchemaType, "schema_version", err)
		}
		switch v {
		case "1.0":
			return Version1, nil
		case "2.0":
			return Version2, nil
		default:
			return "", domain.Errorf(domain.CodeSchemaVersion, "schema_version", "unsupported schema_version %q", v)
		}
	}
	if raw, ok := head["schema"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", domain.WrapError(domain.CodeSchemaType, "schema", err)
		}
		if v == "2.0" {
			return Version2, nil
		}
		return "", domain.Errorf(domain.CodeSchemaVersion, "schema", "unsupported schema %q", v)
	}
	if raw, ok := head["format_version"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", domain.WrapError(domain.CodeSchemaType, "format_version", err)
		}
		if v == "3.0" {
			return Version3, nil
		}
		return "", domain.Errorf(domain.CodeSchemaVersion, "format_version", "unsupported format_version %q", v)
	}
	return "", domain.NewError(domain.CodeSchemaVersion, "$", "missing schema_version/format_version discriminator")
}

func decodeStrict(blob []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return classifyDecode(err, dec.InputOffset())
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return domain.NewError(domain.CodeSchemaConflict, "$", "multiple JSON documents")
		}
		return classifyDecode(err, dec.InputOffset())
	}
	return nil
}

func classifyDecode(err error, offset int64) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unknown field"):
		field := msg
		if i := strings.Index(msg, `unknown field "`); i >= 0 {
			field = strings.Trim(msg[i+len(`unknown field "`):], `"`)
		}
		return domain.Errorf(domain.CodeSchemaUnknown, fmt.Sprintf("json@%d.%s", offset, field), "unsupported field: %s", msg)
	case strings.Contains(msg, "cannot unmarshal"):
		return domain.WrapError(domain.CodeSchemaType, fmt.Sprintf("json@%d", offset), err)
	case strings.Contains(msg, "unexpected EOF") || strings.Contains(msg, "unexpected end of JSON"):
		return domain.WrapError(domain.CodeSchemaConflict, fmt.Sprintf("json@%d", offset), err)
	default:
		return domain.WrapError(domain.CodeSchemaConflict, fmt.Sprintf("json@%d", offset), err)
	}
}

func require(ok bool, context, message string) error {
	if ok {
		return nil
	}
	return domain.NewError(domain.CodeSchemaMissing, context, message)
}

func checkedKiB(v int64, context string) (int64, error) {
	if v < 0 {
		return 0, domain.Errorf(domain.CodeOverflow, context, "negative KiB value %d", v)
	}
	out, overflow := domain.SatMulInt64(v, 1024)
	if overflow {
		return 0, domain.Errorf(domain.CodeOverflow, context, "KiB value %d overflows int64 bytes", v)
	}
	return out, nil
}

func checkedSignedKiB(v int64, context string) (int64, error) {
	if v >= 0 {
		return checkedKiB(v, context)
	}
	if v == -1<<63 {
		return 0, domain.Errorf(domain.CodeOverflow, context, "KiB delta %d overflows int64 bytes", v)
	}
	out, overflow := domain.SatMulInt64(-v, 1024)
	if overflow {
		return 0, domain.Errorf(domain.CodeOverflow, context, "KiB delta %d overflows int64 bytes", v)
	}
	return -out, nil
}

func checkedTicks(v, tickMillis int64, context string) (int64, error) {
	if v < 0 {
		return 0, domain.Errorf(domain.CodeSchemaConflict, context, "negative tick value %d", v)
	}
	if tickMillis <= 0 {
		return 0, domain.Errorf(domain.CodeUnitConflict, "tick_millis", "tick_millis must be positive, got %d", tickMillis)
	}
	out, overflow := domain.SatMulInt64(v, tickMillis)
	if overflow {
		return 0, domain.Errorf(domain.CodeOverflow, context, "tick value %d overflows milliseconds", v)
	}
	return out, nil
}

func opBase(index, target int, kind string, digest string, bits uint, depends []int, source int, atMillis int64, context string) (domain.Operation, error) {
	k := domain.OperationKind(kind)
	switch k {
	case domain.OpReplace, domain.OpPatch, domain.OpDelete, domain.OpCompact:
	default:
		return domain.Operation{}, domain.Errorf(domain.CodeOperationKind, context+".kind", "unsupported operation kind %q", kind)
	}
	d, err := domain.ParseDigestHex(digest, bits)
	if err != nil {
		var de *domain.Error
		if ok := errorAs(err, &de); ok {
			de.Context = context + ".digest"
			return domain.Operation{}, de
		}
		return domain.Operation{}, err
	}
	return domain.Operation{
		Index: index, Kind: k, Target: target, Digest: d,
		DependsOn: domain.CanonicalIntSet(depends), SourceID: source, AtMillis: atMillis,
	}, nil
}

func errorAs(err error, target **domain.Error) bool {
	for err != nil {
		if de, ok := err.(*domain.Error); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func addBatch(p *domain.Project, seen map[int]string, b domain.Batch, context string) error {
	if b.ID <= 0 {
		return domain.Errorf(domain.CodeSchemaConflict, context+".id", "batch id must be positive, got %d", b.ID)
	}
	if prev, ok := seen[b.ID]; ok {
		return domain.Errorf(domain.CodeBatchDuplicate, context, "batch %d already declared in %s", b.ID, prev)
	}
	if b.RowStart < 0 || b.RowEnd < b.RowStart || b.Rows < 0 {
		return domain.Errorf(domain.CodeSchemaConflict, context+".span", "invalid row span [%d,%d) rows=%d", b.RowStart, b.RowEnd, b.Rows)
	}
	if b.Rows != b.RowEnd-b.RowStart {
		return domain.Errorf(domain.CodeSchemaConflict, context+".rows", "row count %d disagrees with span [%d,%d)", b.Rows, b.RowStart, b.RowEnd)
	}
	if b.CompressedBytes < 0 || b.DecodeBufferBytes < 0 {
		return domain.Errorf(domain.CodeOverflow, context, "negative size")
	}
	seen[b.ID] = context
	b.DependsOn = domain.CanonicalIntSet(b.DependsOn)
	b.Status = domain.BatchActive
	p.Batches = append(p.Batches, b)
	return nil
}

// applyFamilyPredecessors derives the immediately preceding batch in each
// family as an additional predecessor. The operation is idempotent for inputs
// that already list the family edge explicitly.
func applyFamilyPredecessors(p *domain.Project) error {
	index := map[int]int{}
	for i := range p.Batches {
		index[p.Batches[i].ID] = i
	}
	for _, family := range p.Families {
		var prev int
		for _, id := range family.BatchIDs {
			pos, ok := index[id]
			if !ok {
				return &domain.Error{Code: domain.CodeBatchUnknown, Context: "families." + family.ID, BatchID: id}
			}
			deps := append([]int(nil), p.Batches[pos].DependsOn...)
			if prev != 0 {
				deps = append(deps, prev)
			}
			p.Batches[pos].DependsOn = domain.CanonicalIntSet(deps)
			prev = id
		}
	}
	return nil
}

// CompareCanonical checks the version-independent parts of already loaded
// projects. It deliberately tolerates v1's whole-batch encoding of a logical
// patch, because engine equivalence verifies the resulting state.
func CompareCanonical(projects []domain.Project) error {
	if len(projects) < 2 {
		return nil
	}
	base := projects[0]
	for i := 1; i < len(projects); i++ {
		cur := projects[i]
		if len(base.Families) != len(cur.Families) {
			return cross(fmt.Sprintf("families length %d != %d", len(base.Families), len(cur.Families)))
		}
		for f := range base.Families {
			bf, cf := base.Families[f], cur.Families[f]
			if bf.ID != cf.ID || fmt.Sprint(bf.BatchIDs) != fmt.Sprint(cf.BatchIDs) {
				return cross(fmt.Sprintf("families[%d] differs: %s %v != %s %v", f, bf.ID, bf.BatchIDs, cf.ID, cf.BatchIDs))
			}
		}
		bb := batchesByID(base.Batches)
		cb := batchesByID(cur.Batches)
		if len(bb) != len(cb) {
			return cross(fmt.Sprintf("batch count %d != %d", len(bb), len(cb)))
		}
		for id, b := range bb {
			c, ok := cb[id]
			if !ok {
				return cross(fmt.Sprintf("batch %d missing", id))
			}
			if err := compareBatch(b, c); err != nil {
				return cross(fmt.Sprintf("batch %d: %v", id, err))
			}
		}
		if base.TimeUnit != cur.TimeUnit || base.SizeUnit != cur.SizeUnit || base.TickMillis != cur.TickMillis ||
			base.DigestBits != cur.DigestBits || base.DigestToleranceBits != cur.DigestToleranceBits ||
			base.RowTolerance != cur.RowTolerance || base.Budget != cur.Budget {
			return cross("digest/tolerance/budget declarations differ")
		}
		if len(base.Scans) != len(cur.Scans) {
			return cross(fmt.Sprintf("scan count %d != %d", len(base.Scans), len(cur.Scans)))
		}
		for s := range base.Scans {
			if fmt.Sprint(base.Scans[s]) != fmt.Sprint(cur.Scans[s]) {
				return cross(fmt.Sprintf("scans[%d] differs", s))
			}
		}
		if len(base.Operations) != len(cur.Operations) {
			return cross(fmt.Sprintf("operation count %d != %d", len(base.Operations), len(cur.Operations)))
		}
		for o := range base.Operations {
			if err := compareOperation(base.Operations[o], cur.Operations[o]); err != nil {
				return cross(fmt.Sprintf("operations[%d]: %v", o, err))
			}
		}
	}
	return nil
}

func cross(msg string) error {
	return domain.NewError(domain.CodeCrossVersion, "versions", msg)
}

func batchesByID(in []domain.Batch) map[int]domain.Batch {
	out := make(map[int]domain.Batch, len(in))
	for _, b := range in {
		out[b.ID] = b
	}
	return out
}

func compareBatch(a, b domain.Batch) error {
	a.Status, b.Status = domain.BatchActive, domain.BatchActive
	if a.ID != b.ID || a.Family != b.Family || a.Column != b.Column ||
		a.RowStart != b.RowStart || a.RowEnd != b.RowEnd || a.Rows != b.Rows ||
		a.Encoding != b.Encoding || a.CompressedBytes != b.CompressedBytes ||
		a.Digest != b.Digest || a.DecodeBufferBytes != b.DecodeBufferBytes ||
		a.Invalidated != b.Invalidated || a.SourceID != b.SourceID ||
		fmt.Sprint(a.DependsOn) != fmt.Sprint(b.DependsOn) {
		return fmt.Errorf("canonical fields differ")
	}
	return nil
}

func compareOperation(a, b domain.Operation) error {
	if a.Index != b.Index || a.Target != b.Target || a.NewBatchID != b.NewBatchID ||
		a.Family != b.Family || a.AtMillis != b.AtMillis || a.SourceID != b.SourceID ||
		fmt.Sprint(a.DependsOn) != fmt.Sprint(b.DependsOn) {
		return fmt.Errorf("operation identity/dependency differs")
	}
	// A v1 replace and a v2/v3 patch can encode the same logical transition.
	patchVsReplace := (a.Kind == domain.OpReplace && b.Kind == domain.OpPatch) ||
		(a.Kind == domain.OpPatch && b.Kind == domain.OpReplace)
	if patchVsReplace {
		return nil
	}
	if a.Kind != b.Kind || a.Digest != b.Digest || a.CompressedBytes != b.CompressedBytes ||
		a.BytesDelta != b.BytesDelta || a.RowDelta != b.RowDelta || a.Rows != b.Rows ||
		a.Encoding != b.Encoding {
		return fmt.Errorf("operation payload differs")
	}
	if (a.Range == nil) != (b.Range == nil) {
		return fmt.Errorf("operation range presence differs")
	}
	if a.Range != nil && *a.Range != *b.Range {
		return fmt.Errorf("operation range differs")
	}
	return nil
}
