package soundfield

import (
	"testing"

	"spatialir/internal/model"
)

func TestITDAndLayoutMap(t *testing.T) {
	est := []model.ChannelEstimate{
		{Channel: "L", DirectSample: 100, Events: []model.Event{{Sample: 100, IsDirect: true}}},
		{Channel: "R", DirectSample: 103, Events: []model.Event{{Sample: 103, IsDirect: true}}},
	}
	itd := Recompute(est, "L")
	if itd["L"] != 0 || itd["R"] != 3 {
		t.Fatalf("bad ITD: %+v", itd)
	}
	layout := model.ChannelLayout{Names: []string{"L", "R"}, Channels: 2}
	m, err := LayoutMap(layout, map[string]string{"L": "R", "R": "L"})
	if err != nil || m["L"] != "R" {
		t.Fatalf("layout map failed: %v %v", m, err)
	}
	if _, err := LayoutMap(layout, map[string]string{"L": "X"}); model.AsAppError(err).Code != model.CodeChan {
		t.Fatalf("expect E_CHAN, got %v", err)
	}
}
