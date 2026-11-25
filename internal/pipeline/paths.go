package pipeline

import (
	"context"
	"fmt"

	"audiobus/domain"
	"audiobus/errs"
	"audiobus/internal/graph"
	"audiobus/internal/state"
	"audiobus/internal/store"
)

func ReplayFromStore(ctx context.Context, st *store.Store) (*RunResult, error) {
	packets, err := st.LoadPackets(ctx)
	if err != nil {
		return nil, err
	}
	return ReplayPackets(packets), nil
}

func ReplayPackets(packets []domain.Packet) *RunResult {
	sortPackets(packets)
	maxCh := 0
	for _, p := range packets {
		if p.Channel+1 > maxCh {
			maxCh = p.Channel + 1
		}
	}
	frames := map[int][]int64{}
	byChWindow := map[int]map[int][]domain.Packet{}
	maxEnd := 0
	for _, p := range packets {
		if p.Start+p.Samples > maxEnd {
			maxEnd = p.Start + p.Samples
		}
		if byChWindow[p.Channel] == nil {
			byChWindow[p.Channel] = map[int][]domain.Packet{}
		}
		byChWindow[p.Channel][p.Window] = append(byChWindow[p.Channel][p.Window], p)
	}
	for ch := 0; ch < maxCh; ch++ {
		frames[ch] = make([]int64, maxEnd)
		for _, windowPackets := range byChWindow[ch] {
			for _, p := range windowPackets {
				copy(frames[ch][p.Start:p.Start+p.Samples], p.Frames)
			}
		}
	}
	return &RunResult{Report: domain.Report{Frames: frames, Packets: packets}}
}

func Resume(ctx context.Context, m domain.Manifest, events []domain.Event, dbPath string) (*RunResult, error) {
	st, recovered, err := store.OpenValidated(ctx, dbPath, m.BranchVersion, domain.AlgoVersion)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	engine := NewEngine(m, events, st)
	if !recovered {
		persisted, err := st.LoadPackets(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range persisted {
			engine.Cache.Put(p)
			pm := engine.machine("packet", p.Window, p.Channel, p.Seq)
			pm.Current = state.Committed
			wm := engine.machine("window", p.Window, p.Channel, 0)
			wm.Current = state.Committed
		}
	}
	return engine.RunRealtime(ctx)
}

func CompareReports(a, b, c *RunResult, capSamples int) error {
	if err := comparePair("realtime/resume", a.Report, b.Report, capSamples); err != nil {
		return err
	}
	if err := comparePair("realtime/replay", a.Report, c.Report, capSamples); err != nil {
		return err
	}
	return nil
}

func comparePair(label string, a, b domain.Report, capSamples int) error {
	for ch, fa := range a.Frames {
		fb, ok := b.Frames[ch]
		if !ok {
			return diffError(ch, 0, 0, "missing channel in "+label)
		}
		n := len(fa)
		if len(fb) < n {
			n = len(fb)
		}
		for i := 0; i < n; i++ {
			if fa[i] != fb[i] {
				seq := 0
				if capSamples > 0 {
					withinWindow := i % 1600
					seq = withinWindow / capSamples
				}
				return diffError(ch, int64(i), seq, fmt.Sprintf("%s frame mismatch: %d != %d", label, fa[i], fb[i]))
			}
		}
		if len(fa) != len(fb) {
			return diffError(ch, int64(n), n/capSamples, label+" frame length mismatch")
		}
	}
	if len(a.Packets) != len(b.Packets) {
		return diffError(0, 0, len(a.Packets), fmt.Sprintf("%s packet count mismatch", label))
	}
	for i := range a.Packets {
		pa, pb := a.Packets[i], b.Packets[i]
		if pa.Window != pb.Window || pa.Channel != pb.Channel || pa.Seq != pb.Seq || pa.Start != pb.Start || pa.Samples != pb.Samples || pa.ContentHash != pb.ContentHash {
			e := diffError(pa.Channel, int64(pa.Start), pa.Seq, fmt.Sprintf("%s packet boundary/hash mismatch at ordered index %d", label, i))
			e.Window = pa.Window
			return e
		}
	}
	return nil
}

func diffError(ch int, offset int64, seq int, reason string) *errs.Error {
	return &errs.Error{Code: errs.EDiff, Channel: ch, First: offset, FirstPack: seq, Reason: reason}
}

func CacheKeys(packets []domain.Packet) []graph.Key {
	out := make([]graph.Key, 0, len(packets))
	for _, p := range packets {
		out = append(out, graph.Key{Window: p.Window, Channel: p.Channel, Seq: p.Seq})
	}
	return out
}
