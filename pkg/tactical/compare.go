package tactical

import "fmt"

// CompareSnapshots returns the first business-data mismatch between two
// snapshots. Diagnostics such as timings and invalidation notes are ignored.
func CompareSnapshots(incremental, full *Snapshot) error {
	if incremental == nil || full == nil {
		return &Error{Code: CodeDiff, Item: "snapshot", Message: "one snapshot is nil"}
	}
	if incremental.MapHash != full.MapHash {
		return diffError("", 0, "map_hash", incremental.MapHash, full.MapHash)
	}
	if incremental.SlotVersion != full.SlotVersion {
		return diffError("", 0, "slot_version", incremental.SlotVersion, full.SlotVersion)
	}
	if incremental.AlgorithmVersion != full.AlgorithmVersion {
		return diffError("", 0, "algorithm_version", incremental.AlgorithmVersion, full.AlgorithmVersion)
	}
	if incremental.FixedVersion != full.FixedVersion {
		return diffError("", 0, "fixed_version", incremental.FixedVersion, full.FixedVersion)
	}
	if len(incremental.MemberOrder) != len(full.MemberOrder) {
		return diffError("", 0, "member_order", intString(len(incremental.MemberOrder)), intString(len(full.MemberOrder)))
	}
	for i := range incremental.MemberOrder {
		if incremental.MemberOrder[i] != full.MemberOrder[i] {
			return diffError(incremental.MemberOrder[i], 0, "member_order", incremental.MemberOrder[i], full.MemberOrder[i])
		}
	}
	if len(incremental.Paths) != len(full.Paths) || len(incremental.Smooth) != len(full.Smooth) || len(incremental.Traces) != len(full.Traces) {
		return diffError("", 0, "member_cache_counts",
			fmt.Sprintf("%d/%d/%d", len(incremental.Paths), len(incremental.Smooth), len(incremental.Traces)),
			fmt.Sprintf("%d/%d/%d", len(full.Paths), len(full.Smooth), len(full.Traces)))
	}
	for _, memberID := range incremental.MemberOrder {
		a, aOK := incremental.Paths[memberID]
		b, bOK := full.Paths[memberID]
		if aOK != bOK {
			return diffError(memberID, 0, "path.presence", boolString(aOK), boolString(bOK))
		}
		if !aOK {
			continue
		}
		if a.Cost != b.Cost {
			return diffError(memberID, 0, "path.cost", int64String(a.Cost), int64String(b.Cost))
		}
		if len(a.Cells) != len(b.Cells) {
			return diffError(memberID, 0, "path.cells.length", intString(len(a.Cells)), intString(len(b.Cells)))
		}
		for i := range a.Cells {
			if a.Cells[i] != b.Cells[i] {
				return diffError(memberID, 0, fmt.Sprintf("path.cells[%d]", i), pointString(a.Cells[i]), pointString(b.Cells[i]))
			}
		}
		sa, saOK := incremental.Smooth[memberID]
		sb, sbOK := full.Smooth[memberID]
		if saOK != sbOK {
			return diffError(memberID, 0, "smooth.presence", boolString(saOK), boolString(sbOK))
		}
		if !saOK {
			continue
		}
		if sa.SourceCost != sb.SourceCost {
			return diffError(memberID, 0, "smooth.source_cost", int64String(sa.SourceCost), int64String(sb.SourceCost))
		}
		if len(sa.Corners) != len(sb.Corners) {
			return diffError(memberID, 0, "smooth.corners.length", intString(len(sa.Corners)), intString(len(sb.Corners)))
		}
		for i := range sa.Corners {
			if sa.Corners[i].Grid != sb.Corners[i].Grid {
				return diffError(memberID, 0, fmt.Sprintf("smooth.corners[%d].grid", i), pointString(sa.Corners[i].Grid), pointString(sb.Corners[i].Grid))
			}
			if sa.Corners[i].Fixed != sb.Corners[i].Fixed {
				return diffError(memberID, 0, fmt.Sprintf("smooth.corners[%d].fixed", i), fixedString(sa.Corners[i].Fixed), fixedString(sb.Corners[i].Fixed))
			}
			if sa.Corners[i].World != sb.Corners[i].World {
				return diffError(memberID, 0, fmt.Sprintf("smooth.corners[%d].world", i),
					fmt.Sprintf("%.3f,%.3f", sa.Corners[i].World.X, sa.Corners[i].World.Y),
					fmt.Sprintf("%.3f,%.3f", sb.Corners[i].World.X, sb.Corners[i].World.Y))
			}
		}
		ta, taOK := incremental.Traces[memberID]
		tb, tbOK := full.Traces[memberID]
		if taOK != tbOK {
			return diffError(memberID, 0, "trace.presence", boolString(taOK), boolString(tbOK))
		}
		if !taOK {
			continue
		}
		if ta.Stats != tb.Stats {
			if ta.Stats.Samples != tb.Stats.Samples {
				return diffError(memberID, 0, "trace.stats.samples", int64String(ta.Stats.Samples), int64String(tb.Stats.Samples))
			}
			if ta.Stats.MaxDeviationMilli != tb.Stats.MaxDeviationMilli {
				return diffError(memberID, 0, "trace.stats.max_deviation", int64String(ta.Stats.MaxDeviationMilli), int64String(tb.Stats.MaxDeviationMilli))
			}
			return diffError(memberID, 0, "trace.stats.sum_deviation", int64String(ta.Stats.SumDeviationMilli), int64String(tb.Stats.SumDeviationMilli))
		}
		if len(ta.States) != len(tb.States) {
			maxTick := len(ta.States)
			if len(tb.States) < maxTick {
				maxTick = len(tb.States)
			}
			return diffError(memberID, maxTick, "trace.states.length", intString(len(ta.States)), intString(len(tb.States)))
		}
		for tick := range ta.States {
			if err := compareTick(memberID, ta.States[tick], tb.States[tick]); err != nil {
				return err
			}
		}
	}
	return nil
}

func compareTick(memberID string, a, b TickState) error {
	if a.Position != b.Position {
		return diffError(memberID, a.Tick, "trace.position", fixedString(a.Position), fixedString(b.Position))
	}
	if a.HeadingMilli != b.HeadingMilli {
		return diffError(memberID, a.Tick, "trace.heading_milli", int64String(a.HeadingMilli), int64String(b.HeadingMilli))
	}
	if a.Segment != b.Segment {
		return diffError(memberID, a.Tick, "trace.segment", intString(a.Segment), intString(b.Segment))
	}
	if a.FormationDeviationMilli != b.FormationDeviationMilli {
		return diffError(memberID, a.Tick, "trace.formation_deviation_milli", int64String(a.FormationDeviationMilli), int64String(b.FormationDeviationMilli))
	}
	if a.AtCorner != b.AtCorner {
		return diffError(memberID, a.Tick, "trace.at_corner", boolString(a.AtCorner), boolString(b.AtCorner))
	}
	return nil
}

func diffError(memberID string, tick int, item, incremental, full string) *Error {
	return &Error{
		Code: CodeDiff, MemberID: memberID, Tick: tick, Item: item,
		Message: fmt.Sprintf("first mismatch in %s: incremental=%s full=%s", item, incremental, full),
	}
}

func intString(v int) string {
	return fmt.Sprintf("%d", v)
}

func int64String(v int64) string {
	return fmt.Sprintf("%d", v)
}

func boolString(v bool) string {
	return fmt.Sprintf("%t", v)
}

func pointString(p GridPoint) string {
	return fmt.Sprintf("%d,%d", p.X, p.Y)
}

func fixedString(p FixPt) string {
	return fmt.Sprintf("%d,%d", p.X, p.Y)
}
