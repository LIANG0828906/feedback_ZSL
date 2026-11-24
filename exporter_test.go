package audiocal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateMachineTransitions(t *testing.T) {
	m := buildTestModel(t)
	exp, err := NewExporter(m, NewCheckpointStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	// 非法迁移：pending 不能直接 committed。
	err = exp.Transition(0, StateCommitted)
	assertCode(t, err, CodeState, "segment=0")
	if exp.States()[0].State != StatePending {
		t.Fatal("非法迁移后原状态被改变")
	}
	// 合法迁移链路。
	if err := exp.Transition(0, StateExporting); err != nil {
		t.Fatal(err)
	}
	if err := exp.Transition(0, StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := exp.Transition(0, StateExporting); err != nil {
		t.Fatal(err)
	}
	if err := exp.Transition(0, StateCommitted); err != nil {
		t.Fatal(err)
	}
	// 已提交为终态。
	err = exp.Transition(0, StateExporting)
	assertCode(t, err, CodeState, "segment=0")

	committed, total := exp.Progress()
	if committed != 1 || total != len(m.Channels) {
		t.Fatalf("进度错误: %d/%d", committed, total)
	}
}

func TestCommittedSegmentFrozen(t *testing.T) {
	s := GenerateSamples(1, 1)[0]
	m, err := Fit(s, DefaultFitConfig())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	exp, err := NewExporter(m, NewCheckpointStore(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exp.ExportSegment(0); err != nil {
		t.Fatal(err)
	}
	before := exp.Firmware().Segments[0].ContentHash

	// 调整正则权重只作用于未提交段。
	newCfg := DefaultFitConfig()
	newCfg.Regularization = 1e-2
	if err := exp.RebuildWithConfig(newCfg, s); err != nil {
		t.Fatal(err)
	}
	after := exp.Firmware().Segments[0].ContentHash
	if before != after {
		t.Fatal("已提交段系数被重算，冻结边界被破坏")
	}
	if exp.States()[0].State != StateCommitted {
		t.Fatal("已提交段状态被重置")
	}
}

func TestCheckpointResumeAndInvalidation(t *testing.T) {
	s := GenerateSamples(1, 1)[0]
	cfg := DefaultFitConfig()
	m, err := Fit(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "cp")
	store := NewCheckpointStore(dir)
	exp, err := NewExporter(m, store)
	if err != nil {
		t.Fatal(err)
	}
	_, total := exp.Progress()
	for i := 0; i < total; i++ {
		if _, err := exp.ExportSegment(i); err != nil {
			t.Fatal(err)
		}
	}

	// 重启：加载检查点并恢复。
	recs, err := store.LoadAll(TargetFingerprint(m.Target), cfg.ParamVersion, AlgorithmID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != total {
		t.Fatalf("恢复段数 %d != %d", len(recs), total)
	}
	exp2, err := NewExporter(m, NewCheckpointStore(dir))
	if err != nil {
		t.Fatal(err)
	}
	exp2.Restore(recs)
	committed, _ := exp2.Progress()
	if committed != total {
		t.Fatalf("恢复后已提交段数 %d != %d", committed, total)
	}

	// 版本不匹配 -> 整体作废。
	_, err = store.LoadAll(TargetFingerprint(m.Target), cfg.ParamVersion+99, AlgorithmID)
	assertCode(t, err, CodeStore, "checkpoint")
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatal("快照作废后目录未清理，存在部分混用风险")
	}
}

func TestCheckpointContentHashTamper(t *testing.T) {
	s := GenerateSamples(1, 1)[0]
	m, _ := Fit(s, DefaultFitConfig())
	dir := t.TempDir()
	store := NewCheckpointStore(dir)
	exp, _ := NewExporter(m, store)
	if _, err := exp.ExportSegment(0); err != nil {
		t.Fatal(err)
	}
	// 篡改段文件内容。
	files, _ := os.ReadDir(dir)
	var segFile string
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".json" {
			segFile = filepath.Join(dir, f.Name())
		}
	}
	raw, _ := os.ReadFile(segFile)
	tampered := append([]byte(nil), raw...)
	// 翻转 JSON 中部一个数字字符，破坏内容哈希但保持文件可读。
	mid := len(tampered) / 2
	for i := mid; i < len(tampered); i++ {
		if tampered[i] >= '0' && tampered[i] <= '8' {
			tampered[i] = tampered[i] + 1
			break
		}
	}
	if err := os.WriteFile(segFile, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := store.LoadAll(TargetFingerprint(m.Target), m.Config.ParamVersion, AlgorithmID)
	assertCode(t, err, CodeStore, "checkpoint")
}
