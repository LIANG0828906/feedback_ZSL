package engine

import (
	"fmt"
	"sort"

	"roomreverb/internal/errs"
	"roomreverb/internal/solver"
)

type RunStats struct {
	Pairs          int   `json:"pairs"`
	Reachable      int   `json:"reachable"`
	Unreachable    int   `json:"unreachable"`
	DecaySumFixed  int64 `json:"decay_sum_fixed"`
	EnergySumFixed int64 `json:"energy_sum_fixed"`
}

func Stats(r *RunResult) RunStats {
	var s RunStats
	for _, room := range r.Rooms {
		s.Pairs += room.Stats.Pairs
		s.Reachable += room.Stats.Reachable
		s.Unreachable += room.Stats.Unreachable
		s.DecaySumFixed += room.Stats.DecaySumFixed
		s.EnergySumFixed += room.Stats.EnergySumFixed
	}
	return s
}

// Compare returns the first deterministic difference and maps it to E_DIFF.
func Compare(a, b *RunResult) error {
	if a == nil || b == nil {
		return &errs.Error{Code: errs.EDiff, Field: "run", Message: "one run result is nil"}
	}
	if a.LevelID != b.LevelID {
		return diff("", "", "", "level_id", "%q != %q", a.LevelID, b.LevelID)
	}
	if a.LevelHash != b.LevelHash {
		return diff("", "", "", "level_hash", "%q != %q", a.LevelHash, b.LevelHash)
	}
	if a.ParamsVersion != b.ParamsVersion {
		return diff("", "", "", "params_version", "%d != %d", a.ParamsVersion, b.ParamsVersion)
	}
	if a.AlgorithmVersion != b.AlgorithmVersion {
		return diff("", "", "", "algorithm_version", "%q != %q", a.AlgorithmVersion, b.AlgorithmVersion)
	}
	if a.Precision != b.Precision {
		return diff("", "", "", "precision", "%q != %q", a.Precision, b.Precision)
	}
	if len(a.CommitOrder) != len(b.CommitOrder) {
		room := ""
		if len(a.CommitOrder) > 0 {
			room = a.CommitOrder[0]
		} else if len(b.CommitOrder) > 0 {
			room = b.CommitOrder[0]
		}
		return diff(room, "", "", "commit_order", "%v != %v", a.CommitOrder, b.CommitOrder)
	}
	for i := range a.CommitOrder {
		if a.CommitOrder[i] != b.CommitOrder[i] {
			return diff(a.CommitOrder[i], "", "", "commit_order", "index %d: %q != %q", i, a.CommitOrder[i], b.CommitOrder[i])
		}
	}
	if a.Progress != b.Progress {
		return diff("", "", "", "progress", "%+v != %+v", a.Progress, b.Progress)
	}
	sa, sb := Stats(a), Stats(b)
	if sa.Pairs != sb.Pairs {
		return diff("", "", "", "stats.pairs", "%d != %d", sa.Pairs, sb.Pairs)
	}
	if sa.Reachable != sb.Reachable {
		return diff("", "", "", "stats.reachable", "%d != %d", sa.Reachable, sb.Reachable)
	}
	if sa.Unreachable != sb.Unreachable {
		return diff("", "", "", "stats.unreachable", "%d != %d", sa.Unreachable, sb.Unreachable)
	}
	if sa.DecaySumFixed != sb.DecaySumFixed {
		return diff("", "", "", "stats.decay_sum_fixed", "%d != %d", sa.DecaySumFixed, sb.DecaySumFixed)
	}
	if sa.EnergySumFixed != sb.EnergySumFixed {
		return diff("", "", "", "stats.energy_sum_fixed", "%d != %d", sa.EnergySumFixed, sb.EnergySumFixed)
	}
	roomIDs := make([]string, 0, len(a.Rooms)+len(b.Rooms))
	seen := map[string]bool{}
	for id := range a.Rooms {
		if !seen[id] {
			seen[id] = true
			roomIDs = append(roomIDs, id)
		}
	}
	for id := range b.Rooms {
		if !seen[id] {
			seen[id] = true
			roomIDs = append(roomIDs, id)
		}
	}
	sort.Strings(roomIDs)
	for _, id := range roomIDs {
		ra, oka := a.Rooms[id]
		rb, okb := b.Rooms[id]
		if !oka || !okb {
			return diff(id, "", "", "room", "present=%v/%v", oka, okb)
		}
		if err := compareRoom(ra, rb); err != nil {
			return err
		}
	}
	return nil
}

func compareRoom(a, b solver.RoomResult) error {
	id := a.RoomID
	if a.Params != b.Params {
		switch {
		case a.Params.T60MS != b.Params.T60MS:
			return diff(id, "", "", "params.t60_ms", "%g != %g", a.Params.T60MS, b.Params.T60MS)
		case a.Params.EarlyDensity != b.Params.EarlyDensity:
			return diff(id, "", "", "params.early_density", "%g != %g", a.Params.EarlyDensity, b.Params.EarlyDensity)
		default:
			return diff(id, "", "", "params.reverb_gain", "%g != %g", a.Params.ReverbGain, b.Params.ReverbGain)
		}
	}
	if fmt.Sprint(a.Neighbors) != fmt.Sprint(b.Neighbors) {
		return diff(id, "", "", "neighbors", "%v != %v", a.Neighbors, b.Neighbors)
	}
	if fmt.Sprint(a.PortalIDs) != fmt.Sprint(b.PortalIDs) {
		return diff(id, "", "", "portal_ids", "%v != %v", a.PortalIDs, b.PortalIDs)
	}
	if len(a.Portals) != len(b.Portals) {
		return diff(id, "", "", "portals", "%d != %d", len(a.Portals), len(b.Portals))
	}
	for i := range a.Portals {
		pa, pb := a.Portals[i], b.Portals[i]
		if pa.ID != pb.ID || pa.WallID != pb.WallID || pa.Center != pb.Center || pa.Width != pb.Width || fmt.Sprint(pa.Rooms) != fmt.Sprint(pb.Rooms) {
			return diff(id, "", "", "portals", "portal %q differs", pa.ID)
		}
	}
	if a.Stats != b.Stats {
		switch {
		case a.Stats.Pairs != b.Stats.Pairs:
			return diff(id, "", "", "stats.pairs", "%d != %d", a.Stats.Pairs, b.Stats.Pairs)
		case a.Stats.Reachable != b.Stats.Reachable:
			return diff(id, "", "", "stats.reachable", "%d != %d", a.Stats.Reachable, b.Stats.Reachable)
		case a.Stats.Unreachable != b.Stats.Unreachable:
			return diff(id, "", "", "stats.unreachable", "%d != %d", a.Stats.Unreachable, b.Stats.Unreachable)
		case a.Stats.DecaySumFixed != b.Stats.DecaySumFixed:
			return diff(id, "", "", "stats.decay_sum_fixed", "%d != %d", a.Stats.DecaySumFixed, b.Stats.DecaySumFixed)
		default:
			return diff(id, "", "", "stats.energy_sum_fixed", "%d != %d", a.Stats.EnergySumFixed, b.Stats.EnergySumFixed)
		}
	}
	pairsA := append([]solver.PairResult(nil), a.Pairs...)
	pairsB := append([]solver.PairResult(nil), b.Pairs...)
	sort.Slice(pairsA, func(i, j int) bool {
		if pairsA[i].ListenerID != pairsA[j].ListenerID {
			return pairsA[i].ListenerID < pairsA[j].ListenerID
		}
		return pairsA[i].SourceID < pairsA[j].SourceID
	})
	sort.Slice(pairsB, func(i, j int) bool {
		if pairsB[i].ListenerID != pairsB[j].ListenerID {
			return pairsB[i].ListenerID < pairsB[j].ListenerID
		}
		return pairsB[i].SourceID < pairsB[j].SourceID
	})
	if len(pairsA) != len(pairsB) {
		return diff(id, "", "", "pair_count", "%d != %d", len(pairsA), len(pairsB))
	}
	for i := range pairsA {
		if err := comparePair(id, pairsA[i], pairsB[i]); err != nil {
			return err
		}
	}
	return nil
}

func comparePair(room string, a, b solver.PairResult) error {
	if a.ListenerID != b.ListenerID || a.SourceID != b.SourceID {
		return diff(room, a.ListenerID, a.SourceID, "pair_identity", "%s/%s != %s/%s", a.ListenerID, a.SourceID, b.ListenerID, b.SourceID)
	}
	if a.Reachable != b.Reachable {
		return diff(room, a.ListenerID, a.SourceID, "reachable", "%v != %v", a.Reachable, b.Reachable)
	}
	if fmt.Sprint(a.RoomPath) != fmt.Sprint(b.RoomPath) {
		return diff(room, a.ListenerID, a.SourceID, "room_path", "%v != %v", a.RoomPath, b.RoomPath)
	}
	if fmt.Sprint(a.PortalPath) != fmt.Sprint(b.PortalPath) {
		return diff(room, a.ListenerID, a.SourceID, "portal_path", "%v != %v", a.PortalPath, b.PortalPath)
	}
	if a.PathFixed != b.PathFixed {
		return diff(room, a.ListenerID, a.SourceID, "path_fixed", "%d != %d", a.PathFixed, b.PathFixed)
	}
	if a.DecayFixed != b.DecayFixed {
		return diff(room, a.ListenerID, a.SourceID, "decay_fixed", "%d != %d", a.DecayFixed, b.DecayFixed)
	}
	if a.EnergyFixed != b.EnergyFixed {
		return diff(room, a.ListenerID, a.SourceID, "energy_fixed", "%d != %d", a.EnergyFixed, b.EnergyFixed)
	}
	return nil
}

func diff(room, listener, source, field, format string, args ...any) *errs.Error {
	return &errs.Error{
		Code: errs.EDiff, Room: room, Listener: listener, Source: source,
		Field: field, Message: fmt.Sprintf(format, args...),
	}
}
