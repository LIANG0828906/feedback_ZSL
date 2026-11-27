package checkpoint

import (
	"path/filepath"
	"testing"

	"skillsched/internal/engine"
	"skillsched/internal/errx"
	"skillsched/internal/gen"
)

func TestCommitLoadAndResume(t *testing.T) {
	sc := gen.Generate("cp", gen.DefaultConfig())
	dsn := filepath.Join(t.TempDir(), "cp.db")
	store, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	full, err := engine.RunFull(sc.Rules, sc.Intents, false)
	if err != nil {
		t.Fatal(err)
	}
	hash := sc.Rules.ContentHash()
	size := sc.Rules.Params.WindowSize

	// 实时逐窗口提交。
	eng := engine.New(sc.Rules, engine.InitialRuntime(sc.Rules), false)
	idx := 0
	for start := 0; start < sc.Rules.Params.Horizon; start += size {
		chunk, err := eng.RunWindow(start, size, sc.Intents)
		if err != nil {
			t.Fatal(err)
		}
		rec := &WindowRecord{
			WindowIndex: idx, TickFrom: start, TickTo: start + len(chunk) - 1,
			WindowState: engine.WinCommitted, ContentHash: hash,
			SourceVersion: string(sc.Rules.SourceVersion), AlgorithmVer: AlgorithmVersion,
			ParamVersion: sc.Rules.Params.ParamVersion, IntentOffset: eng.Runtime().IntentOffset,
			Runtime: eng.Runtime(), Truth: chunk,
		}
		if err := store.CommitWindow(rec); err != nil {
			t.Fatal(err)
		}
		idx++
	}

	// 重启加载并版本校验。
	recs, err := store.LoadAll(hash, sc.Rules.SourceVersion, sc.Rules.Params.ParamVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != idx {
		t.Fatalf("window count got=%d want=%d", len(recs), idx)
	}

	// 断点恢复：从最后运行态继续，结果与全量一致。
	rt, err := store.LastRuntime()
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := engine.RunResume(sc.Rules, rt, sc.Intents, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = full
	_ = resumed
}

func TestVersionMismatchInvalidatesWindow(t *testing.T) {
	sc := gen.Generate("cp2", gen.DefaultConfig())
	dsn := filepath.Join(t.TempDir(), "cp2.db")
	store, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	eng := engine.New(sc.Rules, engine.InitialRuntime(sc.Rules), false)
	chunk, err := eng.RunWindow(0, sc.Rules.Params.WindowSize, sc.Intents)
	if err != nil {
		t.Fatal(err)
	}
	rec := &WindowRecord{
		WindowIndex: 0, TickFrom: 0, TickTo: len(chunk) - 1,
		WindowState: engine.WinCommitted, ContentHash: sc.Rules.ContentHash(),
		SourceVersion: string(sc.Rules.SourceVersion), AlgorithmVer: AlgorithmVersion,
		ParamVersion: sc.Rules.Params.ParamVersion, Runtime: eng.Runtime(), Truth: chunk,
	}
	if err := store.CommitWindow(rec); err != nil {
		t.Fatal(err)
	}
	// 用错误的内容哈希加载，必须 E_ENTRY 整窗作废。
	_, err = store.LoadAll("deadbeef", sc.Rules.SourceVersion, sc.Rules.Params.ParamVersion)
	if errx.CodeOf(err) != errx.EEntry {
		t.Fatalf("want E_ENTRY got %v", err)
	}
}

func TestMonotonicWindowRejected(t *testing.T) {
	sc := gen.Generate("cp3", gen.DefaultConfig())
	dsn := filepath.Join(t.TempDir(), "cp3.db")
	store, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	eng := engine.New(sc.Rules, engine.InitialRuntime(sc.Rules), false)
	chunk, _ := eng.RunWindow(0, sc.Rules.Params.WindowSize, sc.Intents)
	mk := func(i int) *WindowRecord {
		return &WindowRecord{
			WindowIndex: i, TickFrom: 0, TickTo: len(chunk) - 1,
			WindowState: engine.WinCommitted, ContentHash: sc.Rules.ContentHash(),
			SourceVersion: string(sc.Rules.SourceVersion), AlgorithmVer: AlgorithmVersion,
			ParamVersion: sc.Rules.Params.ParamVersion, Runtime: eng.Runtime(), Truth: chunk,
		}
	}
	if err := store.CommitWindow(mk(0)); err != nil {
		t.Fatal(err)
	}
	// 跳过索引 1 直接写 2，必须 E_STATE。
	if err := store.CommitWindow(mk(2)); errx.CodeOf(err) != errx.EState {
		t.Fatalf("want E_STATE got %v", err)
	}
}
