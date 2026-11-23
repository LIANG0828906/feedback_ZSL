package synth

import "testing"

func testManifest() *Manifest {
	return &Manifest{
		ManifestVersion: "test", ChannelCount: 1, SampleRateHz: 8000,
		QuantUnit: 1, SamplesPerPacket: 8, FrameGrid: 4,
		Channels: []ChannelCap{{
			Channel: 0, MinEventGap: 0,
			TimingWindow: TimingWindow{MinOffsetUS: 0, MaxOffsetUS: 100000},
			Templates: []Template{{
				Name: "pulse", Duration: 4,
				Points: []EnvelopePoint{{At: 0, Level: 1000}, {At: 4, Level: 0}},
			}},
		}},
	}
}

func TestNoInterruptBlocksLowerPriority(t *testing.T) {
	m := testManifest()
	events := []Event{
		{ID: "a", Frame: 0, FrameIndex: 0, Type: TypeHit, Channel: 0,
			Strength: 1000, Priority: 5, NoInterrupt: true, Template: "pulse"},
		{ID: "b", Frame: 0, FrameIndex: 1, Type: TypeShoot, Channel: 0,
			Strength: 1000, Priority: 4, NoInterrupt: false, Template: "pulse"},
	}
	curve := Synthesize(0, events, m, 8)
	got := map[string]Decision{}
	for _, d := range curve.Events {
		got[d.EventID] = d.Decision
	}
	if got["a"] != DecisionAccepted || got["b"] != DecisionDropped {
		t.Fatalf("禁止打断仲裁错误: %+v", got)
	}
}

func TestHigherPriorityReplaces(t *testing.T) {
	m := testManifest()
	events := []Event{
		{ID: "a", Frame: 0, FrameIndex: 0, Type: TypeHit, Channel: 0,
			Strength: 1000, Priority: 5, NoInterrupt: true, Template: "pulse"},
		{ID: "b", Frame: 0, FrameIndex: 1, Type: TypeBlast, Channel: 0,
			Strength: 1000, Priority: 9, NoInterrupt: false, Template: "pulse"},
	}
	curve := Synthesize(0, events, m, 8)
	got := map[string]Decision{}
	for _, d := range curve.Events {
		got[d.EventID] = d.Decision
	}
	if got["a"] != DecisionReplaced || got["b"] != DecisionAccepted {
		t.Fatalf("高优先级替换错误: %+v", got)
	}
}

func TestDeterministicAcrossOrder(t *testing.T) {
	m := testManifest()
	events := []Event{
		{ID: "a", Frame: 0, FrameIndex: 0, Type: TypeHit, Channel: 0,
			Strength: 800, Priority: 5, Template: "pulse"},
		{ID: "b", Frame: 1, FrameIndex: 0, Type: TypeStep, Channel: 0,
			Strength: 400, Priority: 6, Template: "pulse"},
		{ID: "c", Frame: 1, FrameIndex: 2, Type: TypeUIFeed, Channel: 0,
			Strength: 200, Priority: 3, Template: "pulse"},
	}
	c1 := Synthesize(0, events, m, 12)
	// 打乱输入顺序，结果必须一致。
	shuffled := []Event{events[2], events[0], events[1]}
	c2 := Synthesize(0, shuffled, m, 12)
	if len(c1.Frames) != len(c2.Frames) {
		t.Fatal("帧数不一致")
	}
	for i := range c1.Frames {
		if c1.Frames[i].Value != c2.Frames[i].Value {
			t.Fatalf("网格 %d 量化值不一致 %d != %d", i, c1.Frames[i].Value, c2.Frames[i].Value)
		}
	}
	// 每个采样点都有量化决策记录。
	for _, sd := range c1.Decisions {
		if sd.Output != c1.Frames[sd.GridOffset].Value {
			t.Fatal("决策输出与帧不一致")
		}
	}
}

func TestTemplateLevelIntegerInterp(t *testing.T) {
	tpl := &Template{Duration: 4, Points: []EnvelopePoint{
		{At: 0, Level: 1000}, {At: 4, Level: 0},
	}}
	got := templateLevelAt(tpl, 2)
	if got != 500 {
		t.Fatalf("整型插值错误: %d", got)
	}
	if templateLevelAt(tpl, 5) != 0 {
		t.Fatal("模板外电平应为 0")
	}
}
