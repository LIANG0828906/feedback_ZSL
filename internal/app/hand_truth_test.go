package app_test

import (
	"testing"

	"multiplexgw/internal/engine"
	"multiplexgw/internal/model"
	"multiplexgw/internal/sample"
)

func TestHandDerivedKeyTruth(t *testing.T) {
	bundle, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(bundle.Canonical, engine.Options{MaxPendingFrames: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sends) != 29 {
		t.Fatalf("sends=%d, want 29", len(res.Sends))
	}
	firstSix := []struct {
		stream int
		bytes  int64
	}{{1, 1024}, {3, 1024}, {5, 1024}, {7, 1024}, {9, 1024}, {1, 1024}}
	for i, want := range firstSix {
		if res.Sends[i].StreamID != want.stream || res.Sends[i].BytesSent != want.bytes {
			t.Fatalf("send %d = stream %d bytes %d; want %+v", i, res.Sends[i].StreamID, res.Sends[i].BytesSent, want)
		}
	}
	if res.Traces[10].ConnectionWindow != 0 {
		t.Fatalf("event 10 connection window=%d, want 0", res.Traces[10].ConnectionWindow)
	}
	var sawBothReasons bool
	for _, b := range res.Traces[11].Blocked {
		if b.StreamID == 3 {
			hasConn, hasStream := false, false
			for _, r := range b.Reasons {
				hasConn = hasConn || r == model.ReasonConnectionWindow
				hasStream = hasStream || r == model.ReasonStreamWindow
			}
			sawBothReasons = hasConn && hasStream
		}
	}
	if !sawBothReasons {
		t.Fatalf("event 11 missing dual window attribution: %+v", res.Traces[11].Blocked)
	}
	rst := res.Traces[37]
	if rst.EventType != model.EventRSTStream || rst.RemovedStreamID != 3 || rst.ReleasedCredit != 4096 {
		t.Fatalf("RST truth = type %s removed %d release %d", rst.EventType, rst.RemovedStreamID, rst.ReleasedCredit)
	}
}
