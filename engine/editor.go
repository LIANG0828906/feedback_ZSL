package engine

import (
	"sort"

	"example.com/columnar/domain"
)

// Editor applies source-immutable project edits used by incremental replay and
// the demo's anomaly-injection path.
type Editor struct {
	p domain.Project
}

func NewEditor(p domain.Project) *Editor {
	return &Editor{p: p.Clone()}
}

func (e *Editor) Project() domain.Project { return e.p.Clone() }

func (e *Editor) SetPatchRange(index int, from, to int64) error {
	for i := range e.p.Operations {
		if e.p.Operations[i].Index == index {
			if e.p.Operations[i].Kind != domain.OpPatch {
				return &domain.Error{Code: domain.CodeOperationKind, Op: index, Context: "editor.patch_range", Message: "operation is not a patch"}
			}
			e.p.Operations[i].Range = &domain.RowRange{From: from, To: to}
			return nil
		}
	}
	return &domain.Error{Code: domain.CodeOperationOrder, Op: index, Context: "editor.patch_range", Message: "operation not found"}
}

func (e *Editor) SetOperationEncoding(index int, encoding string) error {
	for i := range e.p.Operations {
		if e.p.Operations[i].Index == index {
			e.p.Operations[i].Encoding = encoding
			return nil
		}
	}
	return &domain.Error{Code: domain.CodeOperationOrder, Op: index, Context: "editor.encoding", Message: "operation not found"}
}

func (e *Editor) SetBatchEncoding(batchID int, encoding string) error {
	for i := range e.p.Batches {
		if e.p.Batches[i].ID == batchID {
			e.p.Batches[i].Encoding = encoding
			return nil
		}
	}
	return &domain.Error{Code: domain.CodeBatchUnknown, Context: "editor.batch_encoding", BatchID: batchID}
}

func (e *Editor) SetOperationDependencies(index int, deps []int) error {
	for i := range e.p.Operations {
		if e.p.Operations[i].Index == index {
			e.p.Operations[i].DependsOn = domain.CanonicalIntSet(deps)
			return nil
		}
	}
	return &domain.Error{Code: domain.CodeOperationOrder, Op: index, Context: "editor.dependencies", Message: "operation not found"}
}

func (e *Editor) MoveBatchFamily(batchID int, family string, position int) error {
	if family == "" {
		return &domain.Error{Code: domain.CodeSchemaMissing, Context: "editor.family", BatchID: batchID, Message: "family is empty"}
	}
	found := false
	for i := range e.p.Batches {
		if e.p.Batches[i].ID == batchID {
			e.p.Batches[i].Family = family
			found = true
			break
		}
	}
	if !found {
		return &domain.Error{Code: domain.CodeBatchUnknown, Context: "editor.family", BatchID: batchID}
	}
	for i := range e.p.Families {
		e.p.Families[i].BatchIDs = removeInt(e.p.Families[i].BatchIDs, batchID)
	}
	target := -1
	for i := range e.p.Families {
		if e.p.Families[i].ID == family {
			target = i
			break
		}
	}
	if target < 0 {
		e.p.Families = append(e.p.Families, domain.Family{ID: family, Order: len(e.p.Families)})
		target = len(e.p.Families) - 1
	}
	ids := e.p.Families[target].BatchIDs
	if position < 0 || position > len(ids) {
		position = len(ids)
	}
	ids = append(ids, 0)
	copy(ids[position+1:], ids[position:])
	ids[position] = batchID
	e.p.Families[target].BatchIDs = ids
	for i := range e.p.Families {
		e.p.Families[i].Order = i
	}
	return nil
}

func (e *Editor) AddScan(scan domain.Scan) error {
	for _, existing := range e.p.Scans {
		if existing.ID == scan.ID {
			return &domain.Error{Code: domain.CodeScanInvalid, Context: "editor.scan", Message: "duplicate scan id " + scan.ID}
		}
	}
	scan.Order = len(e.p.Scans)
	e.p.Scans = append(e.p.Scans, scan.Clone())
	return nil
}

func (e *Editor) RemoveScan(id string) error {
	out := e.p.Scans[:0]
	found := false
	for _, s := range e.p.Scans {
		if s.ID == id {
			found = true
			continue
		}
		out = append(out, s)
	}
	if !found {
		return &domain.Error{Code: domain.CodeScanInvalid, Context: "editor.scan", Message: "scan not found " + id}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	for i := range out {
		out[i].Order = i
	}
	e.p.Scans = out
	return nil
}

func removeInt(in []int, target int) []int {
	out := in[:0]
	for _, v := range in {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}
