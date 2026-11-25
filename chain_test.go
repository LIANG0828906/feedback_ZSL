package acoustic_test

import (
	"testing"

	acoustic "levelacoustic"
)

func TestSampleChainsMatchTruth(t *testing.T) {
	project := sampleProject(t)
	truthCounts := map[int]int{}
	for _, truth := range project.Truth {
		truthCounts[truth.ListenerID] = truth.ValidPathCount
	}
	expectedCounts := map[int]int{21: 1, 22: 3, 23: 4}
	for listenerID, expected := range expectedCounts {
		if truthCounts[listenerID] != expected {
			t.Fatalf("listener %d truth count=%d want %d", listenerID, truthCounts[listenerID], expected)
		}
	}
	for _, truth := range project.Truth {
		for _, sourceTruth := range truth.Sources {
			chain, err := acoustic.FindChain(project, sourceTruth.SourceID, truth.ListenerID)
			if err != nil {
				t.Fatalf("chain source=%d listener=%d: %v", sourceTruth.SourceID, truth.ListenerID, err)
			}
			if len(chain.RoomIDs) != len(sourceTruth.ChainRoomIDs) {
				t.Fatalf("source=%d listener=%d rooms=%v want=%v", sourceTruth.SourceID, truth.ListenerID, chain.RoomIDs, sourceTruth.ChainRoomIDs)
			}
			for i := range chain.RoomIDs {
				if chain.RoomIDs[i] != sourceTruth.ChainRoomIDs[i] {
					t.Fatalf("source=%d listener=%d rooms=%v want=%v", sourceTruth.SourceID, truth.ListenerID, chain.RoomIDs, sourceTruth.ChainRoomIDs)
				}
			}
			if chain.LengthTicks != sourceTruth.ChainLengthTicks {
				t.Fatalf("source=%d listener=%d chain ticks=%d want=%d", sourceTruth.SourceID, truth.ListenerID, chain.LengthTicks, sourceTruth.ChainLengthTicks)
			}
		}
	}
}

func TestNoChainReturnsSourceAndListener(t *testing.T) {
	project := cloneProject(t, sampleProject(t))
	project.Openings = nil
	_, err := acoustic.FindChain(project, 11, 22)
	if err == nil {
		t.Fatal("expected no-chain error")
	}
	domain := domainError(t, err)
	if domain.Code != acoustic.CodeNoChain {
		t.Fatalf("expected E_NOCHAIN, got %v", domain)
	}
	if len(domain.SourceIDs) != 1 || domain.SourceIDs[0] != 11 || len(domain.ListenerIDs) != 1 || domain.ListenerIDs[0] != 22 {
		t.Fatalf("missing source/listener context: %v", domain)
	}
}

func TestChainPathSegmentsUseActualDoorCenters(t *testing.T) {
	project := sampleProject(t)
	chain, err := acoustic.FindChain(project, 11, 23)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.RoomIDs) != 3 || len(chain.OpeningIDs) != 2 || len(chain.Segments) != 3 {
		t.Fatalf("unexpected chain shape: rooms=%v openings=%v segments=%d", chain.RoomIDs, chain.OpeningIDs, len(chain.Segments))
	}
	if chain.Segments[0].To != (acoustic.Point{X: 4000, Y: 1500}) {
		t.Fatalf("first door center=%v", chain.Segments[0].To)
	}
	if chain.Segments[1].To != (acoustic.Point{X: 8000, Y: 2100}) {
		t.Fatalf("second door center=%v", chain.Segments[1].To)
	}
}
