package engine

import (
	"fmt"
	"sort"

	"replayaudit/internal/model"
)

// Mismatch locates the first field-level difference between two reports.
type Mismatch struct {
	EventID string `json:"event_id,omitempty"`
	Field   string `json:"field"`
	Index   int    `json:"index,omitempty"`
	Want    string `json:"want"`
	Got     string `json:"got"`
}

func (m *Mismatch) Error() string {
	if m.EventID != "" {
		return fmt.Sprintf("mismatch event=%s field=%s: want %s got %s", m.EventID, m.Field, m.Want, m.Got)
	}
	return fmt.Sprintf("mismatch field=%s: want %s got %s", m.Field, m.Want, m.Got)
}

// CompareReports returns nil iff the two paths agree item-by-item on event
// effective times, status, settlement snapshots, sync set and commit sequence.
func CompareReports(want, got *model.Report) *Mismatch {
	if want == nil || got == nil {
		return &Mismatch{Field: "report", Want: fmt.Sprintf("%v", want != nil), Got: fmt.Sprintf("%v", got != nil)}
	}
	if m := compareString("replay_id", "", want.ReplayID, got.ReplayID); m != nil {
		return m
	}
	if len(want.EventResults) != len(got.EventResults) {
		return &Mismatch{Field: "event_results.length", Want: fmt.Sprintf("%d", len(want.EventResults)),
			Got: fmt.Sprintf("%d", len(got.EventResults))}
	}
	for i := 0; i < len(want.EventResults); i++ {
		w, g := want.EventResults[i], got.EventResults[i]
		eid := w.EventID
		if w.EventID != g.EventID {
			return &Mismatch{EventID: w.EventID, Index: i, Field: "event_id", Want: w.EventID, Got: g.EventID}
		}
		if w.EffectiveAt != g.EffectiveAt {
			return &Mismatch{EventID: eid, Index: i, Field: "effective_at",
				Want: fmt.Sprintf("%d", w.EffectiveAt), Got: fmt.Sprintf("%d", g.EffectiveAt)}
		}
		if w.OrderIndex != g.OrderIndex {
			return &Mismatch{EventID: eid, Index: i, Field: "order_index",
				Want: fmt.Sprintf("%d", w.OrderIndex), Got: fmt.Sprintf("%d", g.OrderIndex)}
		}
		if w.Segment != g.Segment {
			return &Mismatch{EventID: eid, Index: i, Field: "segment",
				Want: fmt.Sprintf("%d", w.Segment), Got: fmt.Sprintf("%d", g.Segment)}
		}
		if w.Status != g.Status {
			return &Mismatch{EventID: eid, Index: i, Field: "status", Want: w.Status, Got: g.Status}
		}
		if m := compareState(eid, &w.StateAfter, &g.StateAfter); m != nil {
			m.Index = i
			return m
		}
	}

	ws := syncTimes(want.SyncPoints)
	gs := syncTimes(got.SyncPoints)
	if len(ws) != len(gs) {
		return &Mismatch{Field: "sync_points.length", Want: fmt.Sprintf("%v", ws), Got: fmt.Sprintf("%v", gs)}
	}
	for i := range ws {
		if ws[i] != gs[i] {
			return &Mismatch{Field: fmt.Sprintf("sync_points[%d]", i), Want: fmt.Sprintf("%d", ws[i]),
				Got: fmt.Sprintf("%d", gs[i])}
		}
	}

	if m := compareCommits(want.CommitSequence, got.CommitSequence); m != nil {
		return m
	}

	if want.Winner != got.Winner {
		return &Mismatch{Field: "winner", Want: want.Winner, Got: got.Winner}
	}
	if m := compareState("", &want.Terminal, &got.Terminal); m != nil {
		m.Field = "terminal." + m.Field
		return m
	}
	return nil
}

func compareString(field, eid, want, got string) *Mismatch {
	if want != got {
		return &Mismatch{EventID: eid, Field: field, Want: want, Got: got}
	}
	return nil
}

func syncTimes(in []model.SyncPoint) []int64 {
	out := make([]int64, 0, len(in))
	for _, s := range in {
		out = append(out, s.At)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func compareState(eid string, w, g *model.State) *Mismatch {
	keys := map[string]bool{}
	for k := range w.HP {
		keys["hp."+k] = true
	}
	for k := range g.HP {
		keys["hp."+k] = true
	}
	for k := range w.MP {
		keys["mp."+k] = true
	}
	for k := range g.MP {
		keys["mp."+k] = true
	}
	for k := range w.Cooldown {
		keys["cd."+k] = true
	}
	for k := range g.Cooldown {
		keys["cd."+k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	for _, full := range ordered {
		var wv, gv int64
		switch full[:2] {
		case "hp":
			wv, gv = w.HP[full[3:]], g.HP[full[3:]]
		case "mp":
			wv, gv = w.MP[full[3:]], g.MP[full[3:]]
		case "cd":
			wv, gv = w.Cooldown[full[3:]], g.Cooldown[full[3:]]
		}
		if wv != gv {
			return &Mismatch{EventID: eid, Field: "state." + full,
				Want: fmt.Sprintf("%d", wv), Got: fmt.Sprintf("%d", gv)}
		}
	}
	return nil
}

func compareCommits(w, g []model.SegmentCommit) *Mismatch {
	if len(w) != len(g) {
		return &Mismatch{Field: "commit_sequence.length", Want: fmt.Sprintf("%d", len(w)),
			Got: fmt.Sprintf("%d", len(g))}
	}
	for i := range w {
		if w[i].Segment != g[i].Segment || w[i].CommitSeq != g[i].CommitSeq {
			return &Mismatch{Index: i, Field: fmt.Sprintf("commit_sequence[%d].segment", i),
				Want: fmt.Sprintf("seg=%d seq=%d", w[i].Segment, w[i].CommitSeq),
				Got:  fmt.Sprintf("seg=%d seq=%d", g[i].Segment, g[i].CommitSeq)}
		}
		if !equalStr(w[i].EventIDs, g[i].EventIDs) {
			return &Mismatch{Index: i, Field: fmt.Sprintf("commit_sequence[%d].event_ids", i),
				Want: fmt.Sprintf("%v", w[i].EventIDs), Got: fmt.Sprintf("%v", g[i].EventIDs)}
		}
		if w[i].StateHash != g[i].StateHash {
			return &Mismatch{Index: i, Field: fmt.Sprintf("commit_sequence[%d].state_hash", i),
				Want: w[i].StateHash[:12], Got: g[i].StateHash[:12]}
		}
		if w[i].SegmentHash != g[i].SegmentHash {
			return &Mismatch{Index: i, Field: fmt.Sprintf("commit_sequence[%d].segment_hash", i),
				Want: w[i].SegmentHash[:12], Got: g[i].SegmentHash[:12]}
		}
	}
	return nil
}

func equalStr(a, b []string) bool {
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

// CompareTruth checks a settled report against generator truth within the
// declared inclusive tolerance. It returns the first mismatch.
func CompareTruth(t *model.Truth, rep *model.Report, tolerance int64) *Mismatch {
	byID := rep.ResultByID()
	ids := make([]string, 0, len(t.EffectiveAt))
	for id := range t.EffectiveAt {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		res, ok := byID[id]
		if !ok {
			return &Mismatch{EventID: id, Field: "event_present", Want: "present", Got: "missing"}
		}
		want := t.EffectiveAt[id]
		d := want - res.EffectiveAt
		if d < 0 {
			d = -d
		}
		if d > tolerance {
			return &Mismatch{EventID: id, Field: "effective_at",
				Want: fmt.Sprintf("%d±%d", want, tolerance), Got: fmt.Sprintf("%d", res.EffectiveAt)}
		}
		if t.Status != nil {
			if ws, ok := t.Status[id]; ok && ws != res.Status {
				return &Mismatch{EventID: id, Field: "status", Want: ws, Got: res.Status}
			}
		}
	}
	if m := compareState("", &t.Terminal, &rep.Terminal); m != nil {
		m.Field = "terminal." + m.Field
		return m
	}
	wantSync := append([]int64(nil), t.SyncPoints...)
	gotSync := syncTimes(rep.SyncPoints)
	if len(wantSync) != len(gotSync) {
		return &Mismatch{Field: "truth.sync_points.length",
			Want: fmt.Sprintf("%v", wantSync), Got: fmt.Sprintf("%v", gotSync)}
	}
	for i := range wantSync {
		d := wantSync[i] - gotSync[i]
		if d < 0 {
			d = -d
		}
		if d > tolerance {
			return &Mismatch{Field: fmt.Sprintf("truth.sync_points[%d]", i),
				Want: fmt.Sprintf("%d", wantSync[i]), Got: fmt.Sprintf("%d", gotSync[i])}
		}
	}
	if t.Winner != rep.Winner {
		return &Mismatch{Field: "truth.winner", Want: t.Winner, Got: rep.Winner}
	}
	return nil
}
