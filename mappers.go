package queryopt

func argToV1(a PredicateArg) v1Arg {
	return v1Arg{Literal: a.Literal, GlobalPlaceID: a.PlaceholderGlobal}
}

func argToV2(a PredicateArg, q Query) v2Arg {
	out := v2Arg{Literal: a.Literal}
	if a.PlaceholderGlobal > 0 {
		rel := a.PlaceholderGlobal - q.PlaceholderBase - 1
		out.RelativeIndex = &rel
	}
	return out
}

func selToPermille(f Fixed) *int64 {
	v := f.Permyriad() / 10
	return &v
}

func selToBps(f Fixed) *int64 {
	v := f.Permyriad()
	return &v
}

func toV1(p *Project) *wireV1 {
	idx := p.Index()
	w := &wireV1{
		Schema:     SchemaV1,
		Seed:       p.Seed,
		TicksPerMS: TicksPerMillisecond,
		Weights: v1Weights{
			ScanPermille:         p.Weights.ScanPermyriad / 10,
			IntermediatePermille: p.Weights.IntermediatePermyriad / 10,
			WidthPermille:        p.Weights.WidthPermyriad / 10,
		},
		Rules: wireV1Rules{
			TieRelativePermille: p.Rules.TieRelativePermyriad / 10,
			WorkerBudget:        p.Rules.WorkerBudget,
			CandidateBuffer:     p.Rules.CandidateBuffer,
		},
	}
	for _, t := range p.Tables {
		wt := v1Table{
			ID: t.ID, Name: t.Name, Rows: t.Rows, NullRows: t.NullRows,
			LatencyTicks:    t.SampleLatencyMS * TicksPerMillisecond,
			AllocationBytes: t.AllocationBytes, DeclaredOrder: t.DeclaredOrder,
		}
		for _, c := range p.Columns {
			if c.TableID != t.ID {
				continue
			}
			wt.Columns = append(wt.Columns, v1Column{
				ID: c.ID, Name: c.Name, WidthBytes: c.WidthBytes, NDV: c.NDV,
				NullPermille: c.NullPermyriad / 10, DeclaredOrder: c.DeclaredOrder,
			})
			for _, b := range idx.BucketsByColumn[c.ID] {
				w.Histogram = append(w.Histogram, v1Bucket{
					ID: b.ID, ColumnID: b.ColumnID, BucketIndex: b.BucketIndex,
					Lower: b.Lower, Upper: b.Upper, Rows: b.Rows, Distinct: b.Distinct,
				})
			}
		}
		w.Tables = append(w.Tables, wt)
	}
	for _, q := range p.Queries {
		wq := v1Query{
			ID: q.ID, Ordinal: q.Ordinal, Name: q.Name, TableIDs: append([]string{}, q.TableIDs...),
			RootPredicateIDs:  append([]string{}, q.RootPredicateIDs...),
			RequiredColumnIDs: append([]string{}, q.RequiredColumnIDs...),
			PlaceholderBase:   q.PlaceholderBase, PlaceholderCount: q.PlaceholderCount,
			IndexThresholdRows: q.IndexThresholdRows,
			CandidateIDs:       append([]string{}, q.DeclaredCandidateIDs...),
		}
		for _, pred := range p.Predicates {
			if pred.QueryID != q.ID {
				continue
			}
			wp := v1Predicate{
				ID: pred.ID, Kind: pred.Kind, Logic: pred.Logic,
				Children: append([]string{}, pred.Children...), TableID: pred.TableID,
				ColumnID: pred.ColumnID, LeftColumnID: pred.LeftColumnID,
				RightColumnID: pred.RightColumnID, Op: pred.Op,
				Low: argToV1(pred.Low), High: argToV1(pred.High), Value: argToV1(pred.Value),
				InclusiveLow: pred.InclusiveLow, InclusiveHigh: pred.InclusiveHigh,
				WeightPermille: pred.WeightPermyriad / 10, DeclaredOrder: pred.DeclaredOrder,
			}
			if pred.HasDeclaredSelectivity {
				wp.DeclaredSelPermille = selToPermille(pred.DeclaredSelectivity)
			}
			wq.Predicates = append(wq.Predicates, wp)
		}
		for _, e := range p.Edges {
			if e.QueryID != q.ID {
				continue
			}
			wq.Edges = append(wq.Edges, v1Edge{
				ID: e.ID, LeftTableID: e.LeftTableID, RightTableID: e.RightTableID,
				PredicateID: e.PredicateID, Cartesian: e.Cartesian,
				BreakPriority: e.BreakPriority, DeclaredOrder: e.DeclaredOrder,
				WeightPermille: e.WeightPermyriad / 10,
			})
		}
		w.Queries = append(w.Queries, wq)
	}
	return w
}

func toV2(p *Project) *wireV2 {
	w := &wireV2{
		Schema: SchemaV2, Seed: p.Seed,
		Weights: v2Weights{
			ScanPermyriad:         p.Weights.ScanPermyriad,
			IntermediatePermyriad: p.Weights.IntermediatePermyriad,
			WidthPermyriad:        p.Weights.WidthPermyriad,
		},
		Rules: wireV2Rules{
			TieRelativePermyriad: p.Rules.TieRelativePermyriad,
			WorkerBudget:         p.Rules.WorkerBudget, CandidateBuffer: p.Rules.CandidateBuffer,
		},
	}
	for _, t := range p.Tables {
		wt := v2Table{
			ID: t.ID, Name: t.Name, RowCount: t.Rows, NullRowCount: t.NullRows,
			LatencyMS: t.SampleLatencyMS, AllocationKiB: t.AllocationBytes / BytesPerKiB,
			DeclaredOrder: t.DeclaredOrder,
		}
		for _, c := range p.Columns {
			if c.TableID != t.ID {
				continue
			}
			wc := v2Column{
				ID: c.ID, Name: c.Name, WidthBytes: c.WidthBytes, DeclaredOrder: c.DeclaredOrder,
				Statistics: v2Statistics{NDV: c.NDV, NullBps: c.NullPermyriad},
			}
			for _, b := range p.Index().BucketsByColumn[c.ID] {
				wc.Statistics.Histogram.Buckets = append(wc.Statistics.Histogram.Buckets, v2Bucket{
					ID: b.ID, BucketIndex: b.BucketIndex, Lower: b.Lower, Upper: b.Upper,
					Rows: b.Rows, Distinct: b.Distinct,
				})
			}
			wt.Columns = append(wt.Columns, wc)
		}
		w.Tables = append(w.Tables, wt)
	}
	for _, q := range p.Queries {
		wq := v2Query{
			ID: q.ID, Ordinal: q.Ordinal, Name: q.Name,
			TableIDs:          append([]string{}, q.TableIDs...),
			RootPredicateIDs:  append([]string{}, q.RootPredicateIDs...),
			RequiredColumnIDs: append([]string{}, q.RequiredColumnIDs...),
			PlaceholderBase:   q.PlaceholderBase, PlaceholderCount: q.PlaceholderCount,
			IndexThresholdRows: q.IndexThresholdRows,
			CandidateIDs:       append([]string{}, q.DeclaredCandidateIDs...),
		}
		for _, pred := range p.Predicates {
			if pred.QueryID != q.ID {
				continue
			}
			wp := v2Predicate{
				ID: pred.ID, Kind: pred.Kind, Logic: pred.Logic,
				Children: append([]string{}, pred.Children...), TableID: pred.TableID,
				ColumnID: pred.ColumnID, LeftColumnID: pred.LeftColumnID,
				RightColumnID: pred.RightColumnID, Operator: pred.Op,
				Low: argToV2(pred.Low, q), High: argToV2(pred.High, q), Value: argToV2(pred.Value, q),
				InclusiveLow: pred.InclusiveLow, InclusiveHigh: pred.InclusiveHigh,
				WeightBps: pred.WeightPermyriad, DeclaredOrder: pred.DeclaredOrder,
			}
			if pred.HasDeclaredSelectivity {
				wp.DeclaredSelBps = selToBps(pred.DeclaredSelectivity)
			}
			wq.Predicates = append(wq.Predicates, wp)
		}
		for _, e := range p.Edges {
			if e.QueryID != q.ID {
				continue
			}
			wq.Edges = append(wq.Edges, v2Edge{
				ID: e.ID, LeftTableID: e.LeftTableID, RightTableID: e.RightTableID,
				PredicateID: e.PredicateID, Cartesian: e.Cartesian,
				BreakPriority: e.BreakPriority, DeclaredOrder: e.DeclaredOrder,
				WeightBps: e.WeightPermyriad,
			})
		}
		w.Queries = append(w.Queries, wq)
	}
	return w
}

func argFromV1(a v1Arg) PredicateArg {
	return PredicateArg{Literal: a.Literal, PlaceholderGlobal: a.GlobalPlaceID}
}

func argFromV2Query(a v2Arg, q v2Query) PredicateArg {
	out := PredicateArg{Literal: a.Literal}
	if a.RelativeIndex != nil {
		out.PlaceholderGlobal = q.PlaceholderBase + *a.RelativeIndex + 1
	}
	return out
}

func scalePermille(v int64, field, loc string) (int64, error) {
	if v < 0 || v > 1000 {
		return 0, newError(CodeSelectivityScale, "%s permille value out of [0,1000]", field).
			with("field", field).with("location", loc).with("value", v)
	}
	return v * 10, nil
}

func scaleBps(v int64, field, loc string) (int64, error) {
	if v < 0 || v > 10_000 {
		return 0, newError(CodeSelectivityScale, "%s permyriad value out of [0,10000]", field).
			with("field", field).with("location", loc).with("value", v)
	}
	return v, nil
}

func fromV1(w *wireV1) (*Project, error) {
	p := &Project{SchemaVersion: SchemaV1, Seed: w.Seed}
	var err error
	p.Weights.ScanPermyriad, err = scalePermille(w.Weights.ScanPermille, "cost_weights.scan", "project")
	if err != nil {
		return nil, err
	}
	p.Weights.IntermediatePermyriad, err = scalePermille(w.Weights.IntermediatePermille, "cost_weights.intermediate", "project")
	if err != nil {
		return nil, err
	}
	p.Weights.WidthPermyriad, err = scalePermille(w.Weights.WidthPermille, "cost_weights.width", "project")
	if err != nil {
		return nil, err
	}
	p.Rules.TieRelativePermyriad, err = scalePermille(w.Rules.TieRelativePermille, "rules.tie", "project")
	if err != nil {
		return nil, err
	}
	p.Rules.WorkerBudget = w.Rules.WorkerBudget
	p.Rules.CandidateBuffer = w.Rules.CandidateBuffer

	for _, t := range w.Tables {
		p.Tables = append(p.Tables, Table{
			ID: t.ID, Name: t.Name, Rows: t.Rows, NullRows: t.NullRows,
			SampleLatencyMS: t.LatencyTicks / TicksPerMillisecond,
			AllocationBytes: t.AllocationBytes, DeclaredOrder: t.DeclaredOrder,
		})
		for _, c := range t.Columns {
			nullBps, nerr := scalePermille(c.NullPermille, "column.nullRate", c.ID)
			if nerr != nil {
				return nil, nerr
			}
			p.Columns = append(p.Columns, Column{
				ID: c.ID, TableID: t.ID, Name: c.Name, WidthBytes: c.WidthBytes,
				NDV: c.NDV, NullPermyriad: nullBps, DeclaredOrder: c.DeclaredOrder,
			})
		}
	}
	for _, b := range w.Histogram {
		p.Buckets = append(p.Buckets, Bucket{
			ID: b.ID, ColumnID: b.ColumnID, BucketIndex: b.BucketIndex,
			Lower: b.Lower, Upper: b.Upper, Rows: b.Rows, Distinct: b.Distinct,
		})
	}
	for _, q := range w.Queries {
		pq := Query{
			ID: q.ID, Ordinal: q.Ordinal, Name: q.Name,
			TableIDs:          append([]string{}, q.TableIDs...),
			RootPredicateIDs:  append([]string{}, q.RootPredicateIDs...),
			RequiredColumnIDs: append([]string{}, q.RequiredColumnIDs...),
			PlaceholderBase:   q.PlaceholderBase, PlaceholderCount: q.PlaceholderCount,
			IndexThresholdRows:   q.IndexThresholdRows,
			DeclaredCandidateIDs: append([]string{}, q.CandidateIDs...),
		}
		p.Queries = append(p.Queries, pq)
		for _, pred := range q.Predicates {
			pp := Predicate{
				ID: pred.ID, QueryID: q.ID, Kind: pred.Kind, Logic: pred.Logic,
				Children: append([]string{}, pred.Children...), TableID: pred.TableID,
				ColumnID: pred.ColumnID, LeftColumnID: pred.LeftColumnID,
				RightColumnID: pred.RightColumnID, Op: pred.Op,
				Low: argFromV1(pred.Low), High: argFromV1(pred.High), Value: argFromV1(pred.Value),
				InclusiveLow: pred.InclusiveLow, InclusiveHigh: pred.InclusiveHigh,
				DeclaredOrder: pred.DeclaredOrder,
			}
			pp.WeightPermyriad, err = scalePermille(pred.WeightPermille, "predicate.weight", pred.ID)
			if err != nil {
				return nil, err
			}
			if pred.DeclaredSelPermille != nil {
				bps, serr := scalePermille(*pred.DeclaredSelPermille, "predicate.selectivity", pred.ID)
				if serr != nil {
					return nil, serr
				}
				raw, _ := FixedFromPermyriad(bps)
				pp.DeclaredSelectivity = raw
				pp.HasDeclaredSelectivity = true
			}
			p.Predicates = append(p.Predicates, pp)
		}
		for _, e := range q.Edges {
			weightBps, werr := scalePermille(e.WeightPermille, "edge.weight", e.ID)
			if werr != nil {
				return nil, werr
			}
			p.Edges = append(p.Edges, JoinEdge{
				ID: e.ID, QueryID: q.ID, LeftTableID: e.LeftTableID, RightTableID: e.RightTableID,
				PredicateID: e.PredicateID, Cartesian: e.Cartesian,
				BreakPriority: e.BreakPriority, DeclaredOrder: e.DeclaredOrder,
				WeightPermyriad: weightBps,
			})
		}
	}
	return p, nil
}

func fromV2(w *wireV2) (*Project, error) {
	p := &Project{SchemaVersion: SchemaV2, Seed: w.Seed}
	var err error
	p.Weights.ScanPermyriad, err = scaleBps(w.Weights.ScanPermyriad, "costWeights.scan", "project")
	if err != nil {
		return nil, err
	}
	p.Weights.IntermediatePermyriad, err = scaleBps(w.Weights.IntermediatePermyriad, "costWeights.intermediate", "project")
	if err != nil {
		return nil, err
	}
	p.Weights.WidthPermyriad, err = scaleBps(w.Weights.WidthPermyriad, "costWeights.width", "project")
	if err != nil {
		return nil, err
	}
	p.Rules.TieRelativePermyriad, err = scaleBps(w.Rules.TieRelativePermyriad, "rules.tieRelative", "project")
	if err != nil {
		return nil, err
	}
	p.Rules.WorkerBudget = w.Rules.WorkerBudget
	p.Rules.CandidateBuffer = w.Rules.CandidateBuffer
	for _, t := range w.Tables {
		p.Tables = append(p.Tables, Table{
			ID: t.ID, Name: t.Name, Rows: t.RowCount, NullRows: t.NullRowCount,
			SampleLatencyMS: t.LatencyMS, AllocationBytes: t.AllocationKiB * BytesPerKiB,
			DeclaredOrder: t.DeclaredOrder,
		})
		for _, c := range t.Columns {
			nullBps, nerr := scaleBps(c.Statistics.NullBps, "column.nullRate", c.ID)
			if nerr != nil {
				return nil, nerr
			}
			p.Columns = append(p.Columns, Column{
				ID: c.ID, TableID: t.ID, Name: c.Name, WidthBytes: c.WidthBytes,
				NDV: c.Statistics.NDV, NullPermyriad: nullBps, DeclaredOrder: c.DeclaredOrder,
			})
			for _, b := range c.Statistics.Histogram.Buckets {
				p.Buckets = append(p.Buckets, Bucket{
					ID: b.ID, ColumnID: c.ID, BucketIndex: b.BucketIndex,
					Lower: b.Lower, Upper: b.Upper, Rows: b.Rows, Distinct: b.Distinct,
				})
			}
		}
	}
	for _, q := range w.Queries {
		p.Queries = append(p.Queries, Query{
			ID: q.ID, Ordinal: q.Ordinal, Name: q.Name,
			TableIDs:          append([]string{}, q.TableIDs...),
			RootPredicateIDs:  append([]string{}, q.RootPredicateIDs...),
			RequiredColumnIDs: append([]string{}, q.RequiredColumnIDs...),
			PlaceholderBase:   q.PlaceholderBase, PlaceholderCount: q.PlaceholderCount,
			IndexThresholdRows:   q.IndexThresholdRows,
			DeclaredCandidateIDs: append([]string{}, q.CandidateIDs...),
		})
		for _, pred := range q.Predicates {
			pp := Predicate{
				ID: pred.ID, QueryID: q.ID, Kind: pred.Kind, Logic: pred.Logic,
				Children: append([]string{}, pred.Children...), TableID: pred.TableID,
				ColumnID: pred.ColumnID, LeftColumnID: pred.LeftColumnID,
				RightColumnID: pred.RightColumnID, Op: pred.Operator,
				Low: argFromV2Query(pred.Low, q), High: argFromV2Query(pred.High, q),
				Value:        argFromV2Query(pred.Value, q),
				InclusiveLow: pred.InclusiveLow, InclusiveHigh: pred.InclusiveHigh,
				DeclaredOrder: pred.DeclaredOrder,
			}
			pp.WeightPermyriad, err = scaleBps(pred.WeightBps, "predicate.weight", pred.ID)
			if err != nil {
				return nil, err
			}
			if pred.DeclaredSelBps != nil {
				bps, serr := scaleBps(*pred.DeclaredSelBps, "predicate.selectivity", pred.ID)
				if serr != nil {
					return nil, serr
				}
				pp.DeclaredSelectivity, _ = FixedFromPermyriad(bps)
				pp.HasDeclaredSelectivity = true
			}
			p.Predicates = append(p.Predicates, pp)
		}
		for _, e := range q.Edges {
			weightBps, werr := scaleBps(e.WeightBps, "edge.weight", e.ID)
			if werr != nil {
				return nil, werr
			}
			p.Edges = append(p.Edges, JoinEdge{
				ID: e.ID, QueryID: q.ID, LeftTableID: e.LeftTableID, RightTableID: e.RightTableID,
				PredicateID: e.PredicateID, Cartesian: e.Cartesian,
				BreakPriority: e.BreakPriority, DeclaredOrder: e.DeclaredOrder,
				WeightPermyriad: weightBps,
			})
		}
	}
	return p, nil
}
