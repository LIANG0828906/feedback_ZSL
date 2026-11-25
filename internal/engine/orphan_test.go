package engine

import (
	"testing"

	"multiplexgw/internal/model"
)

func orphanProject(policy string) *model.Project {
	return &model.Project{
		ProjectID: "orphan", FixedPointBits: 16,
		Connection: model.ConnectionSpec{ID: "C", InitialWindowBytes: 65536, MaxWindowBytes: 65536},
		Streams: []model.StreamSpec{
			{ID: 1, ParentID: 0, WeightPermyriad: 2000, InitialWindowBytes: 4096, MaxWindowBytes: 16384, MaxFrameBytes: 1024, OrphanPolicy: policy},
			{ID: 3, ParentID: 1, WeightPermyriad: 3000, InitialWindowBytes: 4096, MaxWindowBytes: 16384, MaxFrameBytes: 1024},
			{ID: 5, ParentID: 1, WeightPermyriad: 2000, InitialWindowBytes: 4096, MaxWindowBytes: 16384, MaxFrameBytes: 1024},
		},
		Events: []model.Event{
			{Index: 0, TimeMS: 10, Type: model.EventHeaders, StreamID: 1},
			{Index: 1, TimeMS: 20, Type: model.EventHeaders, StreamID: 3},
			{Index: 2, TimeMS: 30, Type: model.EventHeaders, StreamID: 5},
			{Index: 3, TimeMS: 40, Type: model.EventRSTStream, StreamID: 1},
		},
	}
}

func TestResetParentReparentsChildren(t *testing.T) {
	res, err := Run(orphanProject(model.OrphanReparent), Options{MaxPendingFrames: 4})
	if err != nil {
		t.Fatal(err)
	}
	last := res.FinalStreams
	byID := map[int]model.StreamSnapshot{}
	for _, s := range last {
		byID[s.StreamID] = s
	}
	if byID[3].EffectiveParentID != 0 || byID[5].EffectiveParentID != 0 {
		t.Fatalf("children were not reparented: %+v %+v", byID[3], byID[5])
	}
	if byID[3].WeightPermyriad != 3000 {
		t.Fatalf("reparent changed weight: %d", byID[3].WeightPermyriad)
	}
}

func TestResetParentInheritWeightToLowestChild(t *testing.T) {
	e, err := New(orphanProject(model.OrphanInherit), Options{MaxPendingFrames: 4})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := e.Step(); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.Streams[3].EffectiveParentID; got != 0 {
		t.Fatalf("heir parent=%d, want 0", got)
	}
	if got := e.Streams[3].WeightPermyriad; got != 5000 {
		t.Fatalf("heir weight=%d, want 5000", got)
	}
	if got := e.Streams[5].EffectiveParentID; got != 0 {
		t.Fatalf("second child parent=%d, want 0", got)
	}
}
