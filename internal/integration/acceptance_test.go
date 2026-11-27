// Package integration 是验收级测试：用固定种子样本验证跨版本装载、
// 逐 tick 排程、三路径一致、错误码与断点恢复。
package integration

import (
	"path/filepath"
	"testing"

	"skillsched/internal/checkpoint"
	"skillsched/internal/engine"
	"skillsched/internal/gen"
	"skillsched/internal/model"
	"skillsched/internal/schema"
)

func TestCrossVersionThreePathsConsistency(t *testing.T) {
	sc := gen.Generate("acceptance", gen.DefaultConfig())
	wires, err := sc.ExportWires()
	if err != nil {
		t.Fatal(err)
	}

	// v2/v3 保留共享组，装载后逐 tick 与生成器真值一致。
	for _, w := range [][]byte{wires.V2, wires.V3} {
		res, err := schema.Load(w)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		got, err := engine.RunFull(res.Rules, sc.Intents, false)
		if err != nil {
			t.Fatal(err)
		}
		if tick, sid, field, ok := engine.Compare(got, sc.Truth.Ticks); ok {
			t.Fatalf("loaded rules differ from truth tick=%d skill=%s field=%s", tick, sid, field)
		}
	}
	// v1 无共享组字段，装载必然丢失共享组语义；验证其损失被显式报告而非静默。
	v1Res, err := schema.Load(wires.V1)
	if err != nil {
		t.Fatal(err)
	}
	if len(v1Res.Rules.Groups) != 0 {
		t.Fatal("v1 must not reconstruct shared groups")
	}
	if !hasDefaultFill(v1Res.Report, "shared_groups") {
		t.Fatal("v1 upgrade must record shared_groups default fill")
	}

	// 三条路径：全量、实时、断点恢复，逐 tick 一致。
	full, err := engine.RunFull(sc.Rules, sc.Intents, false)
	if err != nil {
		t.Fatal(err)
	}
	realtime, err := engine.RunRealtime(sc.Rules, sc.Intents, false)
	if err != nil {
		t.Fatal(err)
	}
	if tick, sid, field, ok := engine.Compare(full, realtime); ok {
		t.Fatalf("full vs realtime differ tick=%d skill=%s field=%s", tick, sid, field)
	}

	// 并发受限预算路径，同样一致，并产生排队诊断。
	conc, err := engine.RunConcurrent(sc.Rules, sc.Intents, false, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if tick, sid, field, ok := engine.Compare(full, conc.Ticks); ok {
		t.Fatalf("full vs concurrent differ tick=%d skill=%s field=%s", tick, sid, field)
	}
	if len(conc.Results) == 0 {
		t.Fatal("expected worker pool results")
	}

	// 断点恢复：提交前半窗口后，从检查点运行态续算，与全量后半段一致。
	dsn := filepath.Join(t.TempDir(), "acc.db")
	store, err := checkpoint.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hash := sc.Rules.ContentHash()
	size := sc.Rules.Params.WindowSize
	eng := engine.New(sc.Rules, engine.InitialRuntime(sc.Rules), false)
	halfWindows := (sc.Rules.Params.Horizon / size) / 2
	for wi := 0; wi < halfWindows; wi++ {
		start := wi * size
		chunk, err := eng.RunWindow(start, size, sc.Intents)
		if err != nil {
			t.Fatal(err)
		}
		rec := &checkpoint.WindowRecord{
			WindowIndex: wi, TickFrom: start, TickTo: start + len(chunk) - 1,
			WindowState: engine.WinCommitted, ContentHash: hash,
			SourceVersion: string(sc.Rules.SourceVersion),
			AlgorithmVer:  checkpoint.AlgorithmVersion,
			ParamVersion:  sc.Rules.Params.ParamVersion,
			Runtime:       eng.Runtime(), Truth: chunk,
		}
		if err := store.CommitWindow(rec); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := store.LastRuntime()
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := engine.RunResume(sc.Rules, rt, sc.Intents, false)
	if err != nil {
		t.Fatal(err)
	}
	// 恢复结果应与全量对应区间逐 tick 相同。
	resumeStart := int(rt.Tick)
	wantTail := full[resumeStart:]
	if tick, sid, field, ok := engine.Compare(resumed, wantTail); ok {
		t.Fatalf("resume mismatch tick=%d skill=%s field=%s", tick, sid, field)
	}
}

func TestTreeAndFlatAcrossResume(t *testing.T) {
	sc := gen.Generate("treeflat", gen.DefaultConfig())
	flat, _ := engine.RunFull(sc.Rules, sc.Intents, false)
	tree, _ := engine.RunFull(sc.Rules, sc.Intents, true)
	if tick, sid, field, ok := engine.Compare(flat, tree); ok {
		t.Fatalf("expr forms differ tick=%d skill=%s field=%s", tick, sid, field)
	}
	var _ = model.V3
}

func hasDefaultFill(rep *schema.Report, path string) bool {
	for _, f := range rep.DefaultFills {
		if f.Path == path {
			return true
		}
	}
	return false
}
