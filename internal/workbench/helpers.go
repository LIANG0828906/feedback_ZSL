package workbench

import (
	"occlbench/internal/culling"
	"occlbench/internal/scene"
	"occlbench/internal/streaming"
)

// buildWindowsFor 复用流式引擎的窗口切分，保证三条路径同源。
func buildWindowsFor(w *scene.World, cfg streaming.Config, samples []culling.SampleResult) []*streaming.Window {
	return streaming.BuildWindows(w, cfg.WindowSize, samples)
}

// stepWindow 用流式引擎推进单个窗口，保持与全量重放完全相同的状态机与调度。
func stepWindow(
	eng *streaming.Engine,
	win *streaming.Window,
	w *scene.World,
	cfg streaming.Config,
	resident *[]int,
	samples []culling.SampleResult,
) error {
	return eng.ProcessWindow(win, samples, resident)
}
