package gen

import "testing"

func TestDeterministicSameSeed(t *testing.T) {
	a := Generate("a", DefaultConfig())
	b := Generate("b", DefaultConfig())
	if a.Rules.ContentHash() != b.Rules.ContentHash() {
		t.Fatal("same seed must produce identical rule content hash")
	}
	if len(a.Intents) != len(b.Intents) {
		t.Fatal("intent length differs")
	}
	for i := range a.Intents {
		if a.Intents[i] != b.Intents[i] {
			t.Fatalf("intent[%d] differs", i)
		}
	}
	if len(a.Truth.Ticks) != len(b.Truth.Ticks) {
		t.Fatal("truth length differs")
	}
	for i := range a.Truth.Ticks {
		if len(a.Truth.Ticks[i].Arbitrations) != len(b.Truth.Ticks[i].Arbitrations) {
			t.Fatalf("arbitration differs at tick %d", i)
		}
	}
}

func TestWiresThreeVersions(t *testing.T) {
	sc := Generate("w", DefaultConfig())
	wires, err := sc.ExportWires()
	if err != nil {
		t.Fatal(err)
	}
	if len(wires.V1) == 0 || len(wires.V2) == 0 || len(wires.V3) == 0 {
		t.Fatal("all three wire versions must be produced")
	}
	// v3 导出应无降级损失，v1 必须包含共享组/上界损失。
	if len(wires.Reports["v3"].Losses) != 0 {
		t.Fatal("v3 export must be lossless")
	}
	if len(wires.Reports["v1"].Losses) == 0 {
		t.Fatal("v1 export must report losses")
	}
}
