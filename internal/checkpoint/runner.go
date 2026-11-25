package checkpoint

import (
	"context"

	"tunnelworkbench/internal/forward"
)

type RunResult struct {
	Report        *forward.Report `json:"report"`
	LastCommitted int             `json:"last_committed_seq"`
	Interrupted   bool            `json:"interrupted"`
}

func RunAndCheckpointPrefix(ctx context.Context, store *Store, p *forward.Project, events []forward.Event, upto int) (*RunResult, error) {
	if upto > 0 && upto < len(events) {
		events = append([]forward.Event(nil), events[:upto]...)
	}
	return RunAndCheckpoint(ctx, store, p, events)
}

// RunAndCheckpoint commits one event at a time. Tunnel states/cache are written first and the
// global event sequence is advanced only afterwards, making the sequence the commit marker.
func RunAndCheckpoint(ctx context.Context, store *Store, p *forward.Project, events []forward.Event) (*RunResult, error) {
	return RunAndCheckpointWithStates(ctx, store, p, events, nil)
}

func RunAndCheckpointWithStates(ctx context.Context, store *Store, p *forward.Project, events []forward.Event, initial []forward.TunnelStatus) (*RunResult, error) {
	meta := Meta{ProjectID: p.ID, SchemaVersion: p.SourceSchema, ProjectHash: p.ContentHash}
	if err := store.InitializeProject(ctx, meta); err != nil {
		return nil, err
	}
	engine, err := forward.NewEngine(p)
	if err != nil {
		return nil, err
	}
	engine.RestoreStates(initial)
	out := &forward.Report{ProjectID: p.ID, SchemaVersion: p.SourceSchema, ContentHash: p.ContentHash}
	last := 0
	interrupted := false
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			interrupted = true
			break
		}
		er := engine.Handle(ev)
		out.Events = append(out.Events, er)
		records := []CacheRecord{}
		for _, pe := range er.Packets {
			records = append(records,
				CacheRecord{Key: "verdict:" + pe.Packet.ID, EventSeq: ev.Seq, Kind: "verdict", Payload: pe.Verdict, Deps: pe.Verdict.Dependencies},
				CacheRecord{Key: "fragment:" + pe.Packet.ID, EventSeq: ev.Seq, Kind: "fragment", Payload: pe.Plan, Deps: pe.Dependencies},
			)
		}
		states := engine.SnapshotStates()
		// Commit with an uncanceled context: if the event finished, its state must reach
		// the durable commit marker even when SIGINT/SIGTERM arrived during handling.
		commitCtx := context.Background()
		if err := store.SaveStateBundle(commitCtx, ev.Seq, states, records); err != nil {
			return &RunResult{Report: out, LastCommitted: last}, err
		}
		if err := store.CommitSequence(commitCtx, meta, ev.Seq); err != nil {
			return &RunResult{Report: out, LastCommitted: last}, err
		}
		last = ev.Seq
		if ctx.Err() != nil {
			interrupted = true
			break
		}
	}
	out.EventCount = len(out.Events)
	forward.FillEventStateSnapshots(out, p)
	out.FinalStates = engine.SnapshotStates()
	out.RejectSet = forward.CollectRejectsPublic(out.Events)
	return &RunResult{Report: out, LastCommitted: last, Interrupted: interrupted}, nil
}

// ReplayFromRecovery discards checkpoint-derived state when identity does not match and replays all.
func ReplayFromRecovery(ctx context.Context, store *Store, p *forward.Project, events []forward.Event) (*RunResult, *Recovery, bool, error) {
	recovery, biz := store.LoadCommitted(ctx, p.SourceSchema, p.ContentHash)
	fullReplay := false
	if biz != nil {
		if biz.Code != forward.CodeCheckpointVersionMismatch {
			return nil, nil, false, biz
		}
		fullReplay = true
		recovery = &Recovery{Meta: Meta{LastEventSeq: 0}}
	}
	if fullReplay {
		result, err := RunAndCheckpoint(ctx, store, p, events)
		return result, recovery, true, err
	}
	startSeq := recovery.Meta.LastEventSeq
	filtered := make([]forward.Event, 0, len(events))
	for _, ev := range events {
		if ev.Seq > startSeq {
			filtered = append(filtered, ev)
		}
	}
	result, err := RunAndCheckpointWithStates(ctx, store, p, filtered, recovery.States)
	if result != nil {
		result.LastCommitted = startSeq
		for _, ev := range filtered {
			result.LastCommitted = ev.Seq
		}
	}
	return result, recovery, false, err
}
