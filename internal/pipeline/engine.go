package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"

	"audiobus/domain"
	"audiobus/errs"
	"audiobus/internal/graph"
	"audiobus/internal/packet"
	"audiobus/internal/state"
	"audiobus/internal/store"
	"audiobus/internal/synth"
)

type Fault struct {
	Window  int
	Channel int
	Seq     int
}

type Engine struct {
	Manifest        domain.Manifest
	Events          []domain.Event
	Pool            *ResourcePool
	Cache           *graph.Cache
	Store           *store.Store
	Fault           *Fault
	StopAfterWindow int
	States          map[string]*state.Machine
	mu              sync.Mutex
}

type RunResult struct {
	Report       domain.Report
	Reused       []graph.Key
	QueueRecords []domain.QueueRecord
}

func NewEngine(m domain.Manifest, events []domain.Event, st *store.Store) *Engine {
	return &Engine{
		Manifest: m, Events: events,
		Pool:            NewResourcePool(2, 8192),
		Cache:           graph.NewCache(),
		Store:           st,
		StopAfterWindow: -1,
		States:          map[string]*state.Machine{},
	}
}

func (e *Engine) key(kind string, w, ch, seq int) string {
	return kind + ":" + itoa(w) + ":" + itoa(ch) + ":" + itoa(seq)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func (e *Engine) machine(kind string, w, ch, seq int) *state.Machine {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := e.key(kind, w, ch, seq)
	if m, ok := e.States[k]; ok {
		return m
	}
	m := state.New(kind, ch, w, seq)
	e.States[k] = m
	return m
}

func (e *Engine) RunRealtime(ctx context.Context) (*RunResult, error) {
	channels := e.Manifest.Channels
	frameByCh := make(map[int][]int64, channels)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for ch := 0; ch < channels; ch++ {
		wg.Add(1)
		go func(ch int) {
			defer wg.Done()
			release, err := e.Pool.Acquire(ch, e.Manifest.TotalSamples)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			defer release()
			result, err := synth.Synthesize(ch, e.Manifest.TotalSamples, e.Events, &e.Manifest)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
			mu.Lock()
			frameByCh[ch] = result.Frames
			mu.Unlock()
		}(ch)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	packets := []domain.Packet{}
	reused := []graph.Key{}
	windows := e.Manifest.TotalSamples / e.Manifest.WindowSamples
	for w := 0; w < windows; w++ {
		commitPackets := []domain.Packet{}
		for ch := 0; ch < channels; ch++ {
			chEvents := channelEvents(e.Events, ch)
			templateHash := channelTemplateHash(e.Manifest, ch)
			windowPackets, err := packet.BuildWindow(w, frameByCh[ch], chEvents, templateHash, &e.Manifest, e.Manifest.BranchVersion)
			if err != nil {
				return nil, err
			}
			for _, built := range windowPackets {
				p := built
				pm := e.machine("packet", p.Window, p.Channel, p.Seq)
				cached, cacheHit := e.Cache.Get(p)
				if cacheHit {
					p = cached
					reused = append(reused, graph.Key{Window: p.Window, Channel: p.Channel, Seq: p.Seq})
				} else {
					if pm.Current == state.Pending || pm.Current == state.Failed {
						if err := pm.Transition(state.Quantizing); err != nil {
							return nil, err
						}
					}
				}
				if e.Fault != nil && e.Fault.Window == p.Window && e.Fault.Channel == p.Channel && e.Fault.Seq == p.Seq {
					_ = pm.Transition(state.Failed)
					ce := &errs.Error{Code: errs.EStage, Window: p.Window, Channel: p.Channel, Packet: p.Seq, Reason: "injected packet quantization/commit failure"}
					return nil, ce
				}
				if !cacheHit {
					if err := pm.Transition(state.Sliced); err != nil {
						return nil, err
					}
					e.Cache.Put(p)
				}
				commitPackets = append(commitPackets, p)
				packets = append(packets, p)
			}
			cm := e.machine("window", w, ch, 0)
			if cm.Current == state.Pending || cm.Current == state.Failed {
				if err := cm.Transition(state.Quantizing); err != nil {
					return nil, err
				}
				if err := cm.Transition(state.Sliced); err != nil {
					return nil, err
				}
			}
		}
		sort.SliceStable(commitPackets, func(i, j int) bool {
			if commitPackets[i].Channel != commitPackets[j].Channel {
				return commitPackets[i].Channel < commitPackets[j].Channel
			}
			return commitPackets[i].Seq < commitPackets[j].Seq
		})
		if e.Store != nil {
			if err := e.Store.CommitWindow(ctx, e.Manifest.BranchVersion, domain.AlgoVersion, store.WindowCommit{Window: w, Packets: commitPackets}); err != nil {
				return nil, err
			}
		}
		for _, p := range commitPackets {
			pm := e.machine("packet", p.Window, p.Channel, p.Seq)
			if err := pm.Transition(state.Committed); err != nil && pm.Current != state.Committed {
				return nil, err
			}
			cm := e.machine("window", w, p.Channel, 0)
			if err := cm.Transition(state.Committed); err != nil && cm.Current != state.Committed {
				return nil, err
			}
		}
		if e.StopAfterWindow == w {
			sortPackets(packets)
			frames := map[int][]int64{}
			for ch := 0; ch < channels; ch++ {
				frames[ch] = frameByCh[ch]
			}
			return &RunResult{Report: domain.Report{Frames: frames, Packets: packets, Events: e.Events}, Reused: reused, QueueRecords: e.Pool.QueueRecords()}, nil
		}
	}
	sortPackets(packets)
	frames := map[int][]int64{}
	for ch := 0; ch < channels; ch++ {
		frames[ch] = frameByCh[ch]
	}
	return &RunResult{Report: domain.Report{Frames: frames, Packets: packets, Events: e.Events}, Reused: reused, QueueRecords: e.Pool.QueueRecords()}, nil
}

func channelEvents(events []domain.Event, ch int) []domain.Event {
	out := []domain.Event{}
	for _, e := range events {
		if e.Channel == ch {
			out = append(out, e)
		}
	}
	return out
}

func channelTemplateHash(m domain.Manifest, ch int) string {
	h := ""
	for _, t := range m.Templates[ch] {
		h += packet.TemplateHash(t)
	}
	sum := sha256.Sum256([]byte(h))
	return hex.EncodeToString(sum[:])
}

func sha256Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) Invalidate(invalidations []domain.Invalidation) {
	e.Cache.Apply(invalidations)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, inv := range invalidations {
		pk := e.key("packet", inv.Window, inv.Channel, inv.Seq)
		e.States[pk] = state.New("packet", inv.Channel, inv.Window, inv.Seq)
		wk := e.key("window", inv.Window, inv.Channel, 0)
		e.States[wk] = state.New("window", inv.Channel, inv.Window, 0)
	}
}

func sortPackets(p []domain.Packet) {
	sort.SliceStable(p, func(i, j int) bool {
		if p[i].Window != p[j].Window {
			return p[i].Window < p[j].Window
		}
		if p[i].Channel != p[j].Channel {
			return p[i].Channel < p[j].Channel
		}
		return p[i].Seq < p[j].Seq
	})
}
