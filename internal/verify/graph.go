package verify

func DependencyGraph() map[string][]string {
	return map[string][]string{
		string(EvWatermark): {"source:frame_content", "param:grid", "param:watermark_threshold", "param:watermark_region"},
		string(EvTimeline):  {string(EvWatermark) + "[frame]", string(EvWatermark) + "[frame-1]"},
		string(EvResidual):  {"source:frames[frame-window-1:frame]", "param:residual_window"},
		string(EvNoise):     {"source:frame_content", "param:block_size"},
		string(EvEdge):      {"source:frame_content", "truth:primitive_bboxes"},
		string(EvDecision): {
			string(EvWatermark), string(EvTimeline), string(EvResidual),
			string(EvNoise), string(EvEdge), "param:decision_thresholds",
		},
	}
}
