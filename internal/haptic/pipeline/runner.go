package pipeline

import (
	"github.com/liang/hapticfeedback/internal/haptic/checkpoint"
	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
	"github.com/liang/hapticfeedback/internal/haptic/packetstate"
)

type RunOptions struct {
	DBPath        string
	Resume        bool
	StopAfter     int
	FailPacketSeq int
	Store         *checkpoint.Store
	Tracker       *packetstate.Tracker
	OnCommitted   func(model.Packet)
}

type RunResult struct {
	Path             string
	Packets          []model.Packet
	Plan             *Plan
	Tracker          *packetstate.Tracker
	CheckpointReset  bool
	LoadedCheckpoint int
	CommittedThisRun int
	Store            *checkpoint.Store
	ownsStore        bool
}

func RunOneShot(stream *model.Stream, capability model.Capability, branch int) (*RunResult, error) {
	plan, err := BuildPlan(stream, capability, branch)
	if err != nil {
		return nil, err
	}
	tracker := packetstate.NewTracker()
	for _, pkt := range plan.Packets {
		tracker.Add(pkt.GlobalSeq, pkt.Channel)
		if err := tracker.Transition(pkt.GlobalSeq, packetstate.Quantizing, nil); err != nil {
			return nil, err
		}
		if err := tracker.Transition(pkt.GlobalSeq, packetstate.Split, nil); err != nil {
			return nil, err
		}
		if err := tracker.Transition(pkt.GlobalSeq, packetstate.Committed, nil); err != nil {
			return nil, err
		}
	}
	return &RunResult{Path: "full_recompute", Packets: plan.Packets, Plan: plan, Tracker: tracker}, nil
}

func RunCheckpointed(stream *model.Stream, capability model.Capability, branch int, opts RunOptions) (*RunResult, error) {
	plan, err := BuildPlan(stream, capability, branch)
	if err != nil {
		return nil, err
	}
	tracker := opts.Tracker
	if tracker == nil {
		tracker = packetstate.NewTracker()
	}
	for _, pkt := range plan.Packets {
		tracker.Add(pkt.GlobalSeq, pkt.Channel)
	}

	result := &RunResult{Plan: plan, Tracker: tracker, Path: "realtime_checkpoint"}
	store := opts.Store
	if store == nil && opts.DBPath != "" {
		if !opts.Resume {
			if err := checkpoint.RemoveArtifacts(opts.DBPath); err != nil {
				return nil, err
			}
		}
		store, err = checkpoint.Open(opts.DBPath)
		if err != nil {
			return nil, err
		}
		result.Store = store
		result.ownsStore = true
	} else {
		result.Store = store
	}

	var loaded []model.Packet
	if opts.Resume && store != nil {
		records, loadErr := store.LoadPackets()
		if loadErr != nil {
			_ = result.closeIfOwned()
			return nil, loadErr
		}
		meta, _ := store.LoadMeta()
		if validateErr := checkpoint.Validate(records, stream.Fingerprint()); validateErr != nil || prefixMismatch(records, plan.Packets) {
			result.CheckpointReset = true
			loaded = nil
			if resetErr := store.Reset(); resetErr != nil {
				return nil, resetErr
			}
		} else {
			loaded = records
			result.LoadedCheckpoint = len(loaded)
			for _, pkt := range loaded {
				tracker.RestoreCommitted(pkt.GlobalSeq, pkt.Channel)
			}
		}
		_ = meta
	}

	result.Packets = append([]model.Packet(nil), loaded...)
	for seq := len(loaded); seq < len(plan.Packets); seq++ {
		pkt := plan.Packets[seq]
		if err := tracker.Transition(seq, packetstate.Quantizing, nil); err != nil {
			_ = result.closeIfOwned()
			return result, err
		}
		if opts.FailPacketSeq > 0 && opts.FailPacketSeq == seq {
			stageErr := errs.New(errs.EStage, "injected single-packet quantization failure")
			stageErr.Seq = seq
			stageErr.Channel = pkt.Channel
			stageErr.FrameNo = firstFrameNo(pkt)
			stageErr.Details = map[string]any{"stage": "quantizing", "retryable": true}
			_ = tracker.Transition(seq, packetstate.Failed, stageErr)
			_ = result.closeIfOwned()
			return result, stageErr
		}
		if err := tracker.Transition(seq, packetstate.Split, nil); err != nil {
			_ = result.closeIfOwned()
			return result, err
		}
		if store != nil {
			meta := checkpoint.Meta{Fingerprint: stream.Fingerprint(), StreamSeed: stream.Seed}
			if err := store.CommitPacket(pkt, meta); err != nil {
				stageErr := errs.Wrap(errs.EStage, "atomic checkpoint commit failed", err)
				stageErr.Seq = seq
				stageErr.Channel = pkt.Channel
				_ = tracker.Transition(seq, packetstate.Failed, stageErr)
				_ = result.closeIfOwned()
				return result, stageErr
			}
		}
		if err := tracker.Transition(seq, packetstate.Committed, nil); err != nil {
			_ = result.closeIfOwned()
			return result, err
		}
		result.Packets = append(result.Packets, pkt)
		result.CommittedThisRun++
		if opts.OnCommitted != nil {
			opts.OnCommitted(pkt)
		}
		if opts.StopAfter > 0 && result.CommittedThisRun >= opts.StopAfter {
			_ = result.closeIfOwned()
			return result, nil
		}
	}
	_ = result.closeIfOwned()
	return result, nil
}

func (r *RunResult) closeIfOwned() error {
	if r != nil && r.ownsStore && r.Store != nil {
		err := r.Store.Close()
		r.Store = nil
		return err
	}
	return nil
}

func prefixMismatch(stored, expected []model.Packet) bool {
	if len(stored) > len(expected) {
		return true
	}
	for i := range stored {
		if stored[i].ContentHash != expected[i].ContentHash {
			return true
		}
	}
	return false
}

func firstFrameNo(pkt model.Packet) int {
	for _, ev := range pkt.Events {
		if ev.Channel == pkt.Channel && ev.StartSample >= pkt.StartSample && ev.StartSample < pkt.EndSample {
			return ev.FrameNo
		}
	}
	return 0
}
