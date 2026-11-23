package streaming_test

import (
	"testing"

	"occlbench/internal/culling"
	"occlbench/internal/generator"
	"occlbench/internal/streaming"
)

// TestFailedWindowIsolation 验证单窗口失败被置 failed 且不污染其他窗口，
// 并可通过合法迁移重试回到 committed。
func TestFailedWindowIsolationAndRetry(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	cfg := testConfig()
	eng, err := streaming.NewEngine(w, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cullResult, err := culling.NewEngine().Run(w, cfg.CullParams)
	if err != nil {
		t.Fatal(err)
	}
	windows := streaming.BuildWindows(w, cfg.WindowSize, cullResult.Samples)
	eng.SetWindows(windows)

	// 模拟窗口 1 在加载阶段失败：手动经合法迁移置 failed。
	target := windows[1]
	s, err := streaming.Transition(target.State, streaming.StateCulling)
	if err != nil {
		t.Fatal(err)
	}
	target.State = s
	s, err = streaming.Transition(target.State, streaming.StateLoading)
	if err != nil {
		t.Fatal(err)
	}
	target.State = s
	s, err = streaming.Transition(target.State, streaming.StateFailed)
	if err != nil {
		t.Fatal(err)
	}
	target.State = s
	target.FailureCause = "simulated io fault"

	// 其他窗口保持待处理，未被污染。
	if windows[0].State != streaming.StatePending {
		t.Fatalf("window 0 should remain pending, got %s", windows[0].State)
	}
	if windows[2].State != streaming.StatePending {
		t.Fatalf("window 2 should remain pending, got %s", windows[2].State)
	}

	// 重试失败窗口，应经 failed->pending->culling->loading->committed 成功提交。
	if err := eng.RetryWindow(1, cullResult.Samples); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if target.State != streaming.StateCommitted || !target.Frozen {
		t.Fatalf("after retry window should be committed, got %s", target.State)
	}

	// 已提交窗口不允许再次迁移（终态保护）。
	if _, err := streaming.Transition(target.State, streaming.StateLoading); err == nil {
		t.Fatal("committed window must reject further transitions")
	}
}
