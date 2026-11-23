package synth

import "testing"

func TestConcurrentMatchesSerial(t *testing.T) {
	m := testManifest()
	events := []Event{
		{ID: "a", Frame: 0, FrameIndex: 0, Type: TypeHit, Channel: 0,
			Strength: 900, Priority: 5, Template: "pulse"},
		{ID: "b", Frame: 2, FrameIndex: 0, Type: TypeShoot, Channel: 0,
			Strength: 300, Priority: 7, NoInterrupt: true, Template: "pulse"},
	}
	serial := SynthesizeAll(events, m, 16)
	par := SynthesizeConcurrent(events, m, 16, SchedulerConfig{
		MaxWorkers: 2, MaxBufferFrames: 8,
	})
	if len(par.Backpressure) == 0 {
		t.Fatal("缓冲上限触发时应记录背压排队事件")
	}
	for c := range serial {
		for k := range serial[c].Frames {
			if serial[c].Frames[k].Value != par.Curves[c].Frames[k].Value {
				t.Fatalf("通道 %d 网格 %d 串并行结果不一致", c, k)
			}
		}
	}
}

func TestSaturationClampsTo16Bit(t *testing.T) {
	m := testManifest()
	events := []Event{
		{ID: "a", Frame: 0, FrameIndex: 0, Type: TypeBlast, Channel: 0,
			Strength: 1000, Priority: 5, Template: "pulse"},
		{ID: "b", Frame: 0, FrameIndex: 0, Type: TypeHit, Channel: 0,
			Strength: 1000, Priority: 5, Template: "pulse"},
		{ID: "c", Frame: 0, FrameIndex: 0, Type: TypeShoot, Channel: 0,
			Strength: 1000, Priority: 5, Template: "pulse"},
	}
	curve := Synthesize(0, events, m, 2)
	for _, f := range curve.Frames {
		if f.Value > OutputMax || f.Value < OutputMin {
			t.Fatalf("饱和限幅失效: %d", f.Value)
		}
	}
	if !curve.Decisions[0].Saturated {
		t.Fatal("叠加溢出应记录饱和标记")
	}
}
