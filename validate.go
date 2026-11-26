package queryopt

import "fmt"

func ValidateProject(p *Project) error {
	if p == nil {
		return newError(CodeInputInvalid, "nil project")
	}
	seenTables := map[string]bool{}
	for i := range p.Tables {
		t := &p.Tables[i]
		if err := requireID("table", t.ID); err != nil {
			return err
		}
		if seenTables[t.ID] {
			return newError(CodeInputInvalid, "duplicate table id").with("table_id", t.ID)
		}
		seenTables[t.ID] = true
		if t.Rows < 0 {
			return newError(CodeNegativeRows, "table row count is negative").
				with("table_id", t.ID).with("rows", t.Rows).with("location", fmt.Sprintf("tables[%s].rows", t.ID))
		}
		if t.NullRows < 0 || t.NullRows > t.Rows {
			return newError(CodeInputInvalid, "null row count must be in [0,rows]").
				with("table_id", t.ID).with("null_rows", t.NullRows).with("rows", t.Rows)
		}
	}
	tableSet := seenTables
	colSet := map[string]bool{}
	for i := range p.Columns {
		c := &p.Columns[i]
		if err := requireID("column", c.ID); err != nil {
			return err
		}
		if colSet[c.ID] {
			return newError(CodeInputInvalid, "duplicate column id").with("column_id", c.ID)
		}
		colSet[c.ID] = true
		if !tableSet[c.TableID] {
			return newError(CodeMissingTable, "column points to missing table").
				with("column_id", c.ID).with("table_id", c.TableID).
				with("location", fmt.Sprintf("columns[%s].table_id", c.ID))
		}
		if c.WidthBytes <= 0 {
			return newError(CodeInputInvalid, "column width must be positive").with("column_id", c.ID)
		}
		if c.NDV <= 0 {
			return newError(CodeInputInvalid, "column NDV must be positive").with("column_id", c.ID)
		}
		if c.NullPermyriad < 0 || c.NullPermyriad > 10_000 {
			return newError(CodeSelectivityScale, "null rate permyriad out of [0,10000]").
				with("column_id", c.ID).with("value", c.NullPermyriad)
		}
	}

	bucketsByCol := map[string][]*Bucket{}
	for i := range p.Buckets {
		b := &p.Buckets[i]
		if err := requireID("bucket", b.ID); err != nil {
			return err
		}
		if !colSet[b.ColumnID] {
			return newError(CodeInputInvalid, "bucket points to missing column").
				with("bucket_id", b.ID).with("column_id", b.ColumnID)
		}
		if b.Lower >= b.Upper {
			return newError(CodeHistogramOrder, "histogram bucket bounds are reversed").
				with("bucket_id", b.ID).with("column_id", b.ColumnID).
				with("bucket_index", b.BucketIndex).with("lower", b.Lower).with("upper", b.Upper).
				with("location", fmt.Sprintf("columns[%s].histogram[%d]", b.ColumnID, b.BucketIndex))
		}
		if b.Rows < 0 || b.Distinct <= 0 {
			return newError(CodeInputInvalid, "bucket rows must be non-negative and distinct positive").
				with("bucket_id", b.ID).with("rows", b.Rows).with("distinct", b.Distinct)
		}
		bucketsByCol[b.ColumnID] = append(bucketsByCol[b.ColumnID], b)
	}
	for colID, buckets := range bucketsByCol {
		// Input order is declaration order; index order is also validated.
		for i := 1; i < len(buckets); i++ {
			prev, cur := buckets[i-1], buckets[i]
			if cur.BucketIndex < prev.BucketIndex || cur.Lower < prev.Lower || cur.Upper < prev.Upper {
				return newError(CodeHistogramOrder, "histogram bucket boundaries are descending").
					with("column_id", colID).with("previous_bucket_id", prev.ID).
					with("bucket_id", cur.ID).with("previous_upper", prev.Upper).
					with("current_lower", cur.Lower).with("current_upper", cur.Upper).
					with("location", fmt.Sprintf("columns[%s].histogram[%s]", colID, cur.ID))
			}
		}
	}

	predSet := map[string]bool{}
	for i := range p.Predicates {
		d := &p.Predicates[i]
		if err := requireID("predicate", d.ID); err != nil {
			return err
		}
		if predSet[d.ID] {
			return newError(CodeInputInvalid, "duplicate predicate id").with("predicate_id", d.ID)
		}
		predSet[d.ID] = true
		if d.QueryID != "" {
			found := false
			for _, q := range p.Queries {
				if q.ID == d.QueryID {
					found = true
					break
				}
			}
			if !found {
				return newError(CodeInputInvalid, "predicate points to missing query").
					with("predicate_id", d.ID).with("query_id", d.QueryID)
			}
		}
		if d.TableID != "" && !tableSet[d.TableID] {
			return newError(CodeMissingTable, "predicate points to missing table").
				with("predicate_id", d.ID).with("table_id", d.TableID)
		}
		if d.ColumnID != "" && !colSet[d.ColumnID] {
			return newError(CodeInputInvalid, "predicate points to missing column").
				with("predicate_id", d.ID).with("column_id", d.ColumnID)
		}
		if d.HasDeclaredSelectivity && (d.DeclaredSelectivity < 0 || d.DeclaredSelectivity > FixedOne) {
			return newError(CodeSelectivityScale, "declared selectivity is outside fixed-point [0,1]").
				with("predicate_id", d.ID).with("raw", int64(d.DeclaredSelectivity))
		}
		if d.WeightPermyriad < 0 || d.WeightPermyriad > 10_000 {
			return newError(CodeSelectivityScale, "weight permyriad out of [0,10000]").
				with("predicate_id", d.ID).with("weight", d.WeightPermyriad)
		}
	}
	for i := range p.Edges {
		e := &p.Edges[i]
		if err := requireID("edge", e.ID); err != nil {
			return err
		}
		if !tableSet[e.LeftTableID] {
			return newError(CodeMissingTable, "join edge points to missing left table").
				with("edge_id", e.ID).with("table_id", e.LeftTableID).
				with("location", fmt.Sprintf("queries[%s].edges[%s].left_table_id", e.QueryID, e.ID))
		}
		if !tableSet[e.RightTableID] {
			return newError(CodeMissingTable, "join edge points to missing right table").
				with("edge_id", e.ID).with("table_id", e.RightTableID).
				with("location", fmt.Sprintf("queries[%s].edges[%s].right_table_id", e.QueryID, e.ID))
		}
		if e.PredicateID != "" && !predSet[e.PredicateID] {
			return newError(CodeInputInvalid, "join edge points to missing predicate").
				with("edge_id", e.ID).with("predicate_id", e.PredicateID)
		}
	}
	if p.Rules.WorkerBudget != 0 || p.Rules.CandidateBuffer != 0 {
		if err := ValidateBudget(p.Rules.WorkerBudget, p.Rules.CandidateBuffer); err != nil {
			return err
		}
	}
	return nil
}
