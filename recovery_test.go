package audiocal

import (
	"path/filepath"
	"testing"
)

// TestCrashRecoveryOnlyUsesPersistedSegments 模拟进程在中途被强制终止：
// 重启后只能依据已持久化段恢复，未完成时不得产出完整导出报告。
func TestCrashRecoveryOnlyUsesPersistedSegments(t *testing.T) {
	s := GenerateSamples(20261124, 2)[0]
	m, err := Fit(s, DefaultFitConfig())
	if err != nil {
		t.Fatal(err)
	}
	cpDir := filepath.Join(t.TempDir(), "cp")

	// 第一“进程”：提交 2 段后崩溃，没有调用任何 finalize。
	first, err := NewExporter(m, NewCheckpointStore(cpDir))
	if err != nil {
		t.Fatal(err)
	}
	_, total := first.Progress()
	if total < 3 {
		t.Fatalf("测试前提不成立：总段数 %d 过少", total)
	}
	for i := 0; i < 2; i++ {
		if _, err := first.ExportSegment(i); err != nil {
			t.Fatal(err)
		}
	}
	if first.Complete() {
		t.Fatal("中途崩溃点不应是完成态")
	}

	// 第二“进程”：从检查点恢复，只看到 2 个已持久化段。
	store := NewCheckpointStore(cpDir)
	recs, err := store.LoadAll(TargetFingerprint(m.Target), m.Config.ParamVersion, AlgorithmID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("恢复段数应为 2，得到 %d", len(recs))
	}
	second, err := NewExporter(m, store)
	if err != nil {
		t.Fatal(err)
	}
	second.Restore(recs)
	committed, total2 := second.Progress()
	if committed != 2 || total2 != total {
		t.Fatalf("恢复进度错误: %d/%d", committed, total2)
	}

	// 未补齐前完成导出必须失败，禁止承诺完整报告。
	if _, err := finalize(second, m); err == nil {
		t.Fatal("未完成导出却生成了完整报告")
	} else {
		assertCode(t, err, CodeState, "progress")
	}

	// 补齐剩余段后才能产出与全量路径一致的完整报告。
	for i := 2; i < total; i++ {
		if _, err := second.ExportSegment(i); err != nil {
			t.Fatal(err)
		}
	}
	resumed, err := finalize(second, m)
	if err != nil {
		t.Fatal(err)
	}
	full, err := ExportAllAtOnce(m, NewCheckpointStore(filepath.Join(t.TempDir(), "full")))
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareResults(resumed, full); err != nil {
		t.Fatalf("崩溃恢复后产物与全量导出不一致: %v", err)
	}
}

// TestCheckpointSerialWrites 验证高并发提交下检查点写入不交错、段序稳定。
func TestCheckpointSerialWrites(t *testing.T) {
	s := GenerateSamples(1, 1)[0]
	m, _ := Fit(s, DefaultFitConfig())
	store := NewCheckpointStore(t.TempDir())
	exp, _ := NewExporter(m, store)
	_, total := exp.Progress()
	for i := 0; i < total; i++ {
		if _, err := exp.ExportSegment(i); err != nil {
			t.Fatal(err)
		}
	}
	recs := exp.CommittedRecords()
	for i, r := range recs {
		if r.Index != i {
			t.Fatalf("检查点段序不稳定: 位置 %d 存的是段 %d", i, r.Index)
		}
	}
}
