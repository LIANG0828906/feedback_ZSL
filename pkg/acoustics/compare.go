package acoustics

import (
	"fmt"
	"math"
)

func VerifyTruth(level *Level, report *Report, truth *TruthFile) (*VerificationReport, error) {
	vr := &VerificationReport{}
	if err := truth.MatchesListeners(level); err != nil {
		return nil, err
	}
	pairMap := map[string]PairResult{}
	for _, pr := range report.Pairs {
		pairMap[pr.SourceID+"\x00"+pr.ListenerID] = pr
	}
	roomByListener := map[string]string{}
	for _, l := range level.Listeners {
		roomByListener[l.ID] = l.RoomID
	}

	for _, lt := range truth.ListenerTruths {
		vr.CheckedRooms++
		for _, st := range lt.Sources {
			vr.CheckedPairs++
			pr, ok := pairMap[st.SourceID+"\x00"+lt.ListenerID]
			roomID := lt.RoomID
			if !ok {
				addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "pair_result", Expected: "present", Actual: "missing"})
				continue
			}
			if st.Connected {
				if pr.Chain == nil {
					addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "chain", Expected: st.RoomChain, Actual: nil})
					continue
				}
				if !equalStrings(st.RoomChain, pr.Chain.RoomIDs) {
					addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "room_chain", Expected: st.RoomChain, Actual: pr.Chain.RoomIDs})
				}
				if !equalStrings(st.DoorChain, pr.Chain.DoorIDs) {
					addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "door_chain", Expected: st.DoorChain, Actual: pr.Chain.DoorIDs})
				}
				if d := math.Abs(st.PathLengthM - pr.Chain.LengthM); d > level.Params.LengthToleranceM {
					addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "path_length_m", Expected: st.PathLengthM, Actual: pr.Chain.LengthM, Difference: d})
				}
			} else if pr.Error == nil || pr.Error.Code != CodeUnreach {
				code := ""
				if pr.Error != nil {
					code = pr.Error.Code
				}
				addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "unreachable", Expected: CodeUnreach, Actual: code})
			}

			if pr.ValidPathCount != st.ValidPathCount {
				addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "valid_path_count", Expected: st.ValidPathCount, Actual: pr.ValidPathCount})
			}
			actualHits := map[string]ReflectionHit{}
			for _, h := range pr.Reflections {
				actualHits[h.ReflectorID] = h
			}
			for _, th := range st.Reflections {
				h, ok := actualHits[th.ReflectorID]
				if !ok {
					addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "reflection:" + th.ReflectorID, Expected: "present", Actual: "missing"})
					continue
				}
				if d := math.Max(math.Abs(th.ReflectionPoint.X-h.ReflectionPoint.X), math.Abs(th.ReflectionPoint.Y-h.ReflectionPoint.Y)); d > level.Params.CoordToleranceM {
					addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "reflection_point:" + th.ReflectorID, Expected: th.ReflectionPoint, Actual: h.ReflectionPoint, Difference: d})
				}
				if d := math.Abs(th.PathLengthM - h.PathLengthM); d > level.Params.LengthToleranceM {
					addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "reflection_path_length:" + th.ReflectorID, Expected: th.PathLengthM, Actual: h.PathLengthM, Difference: d})
				}
				if d := math.Abs(th.IncidenceAngleDeg - h.IncidenceAngleDeg); d > 0.01 {
					addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "incidence_angle_deg:" + th.ReflectorID, Expected: th.IncidenceAngleDeg, Actual: h.IncidenceAngleDeg, Difference: d})
				}
				if d := math.Abs(th.SPLDB - h.SPLDB); d > level.Params.SPLToleranceDB {
					addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "spl_db:" + th.ReflectorID, Expected: th.SPLDB, Actual: h.SPLDB, Difference: d})
				}
			}
			if st.ExpectedSPLDB == nil && pr.ExpectedSPLDB != nil {
				addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "expected_spl_db", Expected: nil, Actual: *pr.ExpectedSPLDB})
			}
			if st.ExpectedSPLDB != nil && pr.ExpectedSPLDB == nil {
				addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "expected_spl_db", Expected: *st.ExpectedSPLDB, Actual: nil})
			}
			if st.ExpectedSPLDB != nil && pr.ExpectedSPLDB != nil {
				if d := math.Abs(*st.ExpectedSPLDB - *pr.ExpectedSPLDB); d > level.Params.SPLToleranceDB {
					addMismatch(vr, DiffMismatch{RoomID: roomID, ListenerID: lt.ListenerID, SourceID: st.SourceID, Field: "expected_spl_db", Expected: *st.ExpectedSPLDB, Actual: *pr.ExpectedSPLDB, Difference: d})
				}
			}
		}
	}

	if len(vr.Mismatches) > 0 {
		vr.OK = false
		vr.FirstMismatch = &vr.Mismatches[0]
		return vr, diffError(vr.FirstMismatch)
	}
	vr.OK = true
	return vr, nil
}

// CompareReports checks the externally observable acoustic result and stable
// room commit sequence across preview, recovery and full recomputation.
func CompareReports(expected, actual *Report) error {
	tol := expected.Params
	if tol.CoordToleranceM == 0 {
		tol = DefaultParams()
	}
	if !equalStrings(expected.CommitSequence, actual.CommitSequence) {
		d := DiffMismatch{Field: "commit_sequence", Expected: expected.CommitSequence, Actual: actual.CommitSequence}
		return diffError(&d)
	}
	if !equalStrings(roomIDsFromReport(expected), roomIDsFromReport(actual)) {
		d := DiffMismatch{Field: "room_order", Expected: roomIDsFromReport(expected), Actual: roomIDsFromReport(actual)}
		return diffError(&d)
	}
	expPairs := map[string]PairResult{}
	for _, p := range expected.Pairs {
		expPairs[p.SourceID+"\x00"+p.ListenerID] = p
	}
	for _, ap := range actual.Pairs {
		key := ap.SourceID + "\x00" + ap.ListenerID
		ep, ok := expPairs[key]
		if !ok {
			d := DiffMismatch{RoomID: ap.ListenerRoomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "pair_result", Expected: "present", Actual: "missing in expected"}
			return diffError(&d)
		}
		roomID := ap.ListenerRoomID
		if (ep.Chain == nil) != (ap.Chain == nil) {
			d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "chain", Expected: ep.Chain, Actual: ap.Chain}
			return diffError(&d)
		}
		if ep.Chain != nil {
			if !equalStrings(ep.Chain.RoomIDs, ap.Chain.RoomIDs) {
				d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "room_chain", Expected: ep.Chain.RoomIDs, Actual: ap.Chain.RoomIDs}
				return diffError(&d)
			}
			if !equalStrings(ep.Chain.DoorIDs, ap.Chain.DoorIDs) {
				d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "door_chain", Expected: ep.Chain.DoorIDs, Actual: ap.Chain.DoorIDs}
				return diffError(&d)
			}
			if ep.Chain.LengthFixed != ap.Chain.LengthFixed {
				d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "chain_length_fixed", Expected: ep.Chain.LengthFixed, Actual: ap.Chain.LengthFixed, Difference: float64(abs64(ep.Chain.LengthFixed - ap.Chain.LengthFixed))}
				return diffError(&d)
			}
		}
		if ep.ValidPathCount != ap.ValidPathCount {
			d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "valid_path_count", Expected: ep.ValidPathCount, Actual: ap.ValidPathCount}
			return diffError(&d)
		}
		expHits := map[string]ReflectionHit{}
		for _, h := range ep.Reflections {
			expHits[h.ReflectorID] = h
		}
		for _, ah := range ap.Reflections {
			eh, ok := expHits[ah.ReflectorID]
			if !ok {
				d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "reflection:" + ah.ReflectorID, Expected: "present", Actual: "unexpected"}
				return diffError(&d)
			}
			if eh.ReflectionPoint != ah.ReflectionPoint {
				// Exact fixed-derived JSON values should be identical; tolerance is for preview rounding.
				diff := math.Max(math.Abs(eh.ReflectionPoint.X-ah.ReflectionPoint.X), math.Abs(eh.ReflectionPoint.Y-ah.ReflectionPoint.Y))
				if diff > tol.CoordToleranceM {
					d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "reflection_point:" + ah.ReflectorID, Expected: eh.ReflectionPoint, Actual: ah.ReflectionPoint, Difference: diff}
					return diffError(&d)
				}
			}
			if math.Abs(eh.PathLengthM-ah.PathLengthM) > tol.LengthToleranceM {
				d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "path_length_m:" + ah.ReflectorID, Expected: eh.PathLengthM, Actual: ah.PathLengthM, Difference: math.Abs(eh.PathLengthM - ah.PathLengthM)}
				return diffError(&d)
			}
			if math.Abs(eh.SPLDB-ah.SPLDB) > tol.SPLToleranceDB {
				d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "spl_db:" + ah.ReflectorID, Expected: eh.SPLDB, Actual: ah.SPLDB, Difference: math.Abs(eh.SPLDB - ah.SPLDB)}
				return diffError(&d)
			}
		}
		if (ep.ExpectedSPLDB == nil) != (ap.ExpectedSPLDB == nil) {
			d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "expected_spl_db", Expected: ep.ExpectedSPLDB, Actual: ap.ExpectedSPLDB}
			return diffError(&d)
		}
		if ep.ExpectedSPLDB != nil && math.Abs(*ep.ExpectedSPLDB-*ap.ExpectedSPLDB) > tol.SPLToleranceDB {
			d := DiffMismatch{RoomID: roomID, ListenerID: ap.ListenerID, SourceID: ap.SourceID, Field: "expected_spl_db", Expected: *ep.ExpectedSPLDB, Actual: *ap.ExpectedSPLDB, Difference: math.Abs(*ep.ExpectedSPLDB - *ap.ExpectedSPLDB)}
			return diffError(&d)
		}
	}
	return nil
}

func addMismatch(vr *VerificationReport, m DiffMismatch) {
	if len(vr.Mismatches) < 100 {
		vr.Mismatches = append(vr.Mismatches, m)
	}
}

func diffError(m *DiffMismatch) error {
	return &DomainError{Code: CodeDiff, ObjectID: m.RoomID, Field: m.Field, Message: fmt.Sprintf("first mismatch in %s/%s/%s: %s", m.RoomID, m.ListenerID, m.SourceID, m.Field), Details: m}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func roomIDsFromReport(r *Report) []string {
	out := make([]string, 0, len(r.Rooms))
	for _, room := range r.Rooms {
		out = append(out, room.RoomID)
	}
	return out
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
