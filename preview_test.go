package iconmask

import "testing"

func TestPreviewAndExactPathsUseSameSnapshot(t *testing.T) {
	project := testProject(t)
	snapshot := NewParameterSnapshot(DefaultRepairConfig())
	previewCfg := DefaultPreviewConfig()
	for _, image := range project.Images {
		result, err := RunDualPath(image, snapshot, previewCfg)
		if err != nil {
			t.Fatalf("%s: %v", image.ID, err)
		}
		if result.ParameterVersion != snapshot.Version || result.Preview.ParameterVersion != snapshot.Version {
			t.Fatalf("%s: parameter version was not shared", image.ID)
		}
		if result.Preview.ActualDownsample != previewCfg.Downsample {
			t.Fatalf("%s: unexpected actual downsample %d", image.ID, result.Preview.ActualDownsample)
		}
		if result.Preview.RoundsUsed > previewCfg.Rounds {
			t.Fatalf("%s: preview exceeded reduced round budget", image.ID)
		}
		if !result.Preview.WithinLatency {
			t.Fatalf("%s: preview exceeded latency budget: %s", image.ID, result.Preview.Elapsed)
		}
		if !result.Comparison.Passed {
			t.Fatalf("%s: comparison did not pass: %+v", image.ID, result.Comparison)
		}
		if result.Preview.Stats.Count != result.Exact.Stats.Count {
			t.Fatalf("%s: region count mismatch", image.ID)
		}
	}
}

func TestPreviewDivergenceError(t *testing.T) {
	project := testProject(t)
	image := findImage(t, project, "icon-96")
	snapshot := NewParameterSnapshot(DefaultRepairConfig())
	cfg := DefaultPreviewConfig()
	cfg.AreaRelativeTolerance = -1
	result, err := RunDualPath(image, snapshot, cfg)
	domainErr := assertCode(t, err, CodeDivergence)
	if result == nil || result.Comparison.Passed {
		t.Fatal("expected retained divergent comparison")
	}
	if domainErr.FirstDiffXY == nil {
		t.Fatal("E_DIVERGE should report the first differing pixel")
	}
	if domainErr.Details == nil || domainErr.Details["maxAreaRelativeError"] == nil {
		t.Fatalf("expected measured divergence values: %+v", domainErr)
	}
}

func TestPreviewRejectsStaleSnapshot(t *testing.T) {
	project := testProject(t)
	image := findImage(t, project, "icon-96")
	snapshot := NewParameterSnapshot(DefaultRepairConfig())
	snapshot.Config.Radius.Max++
	_, err := RunDualPath(image, snapshot, DefaultPreviewConfig())
	domainErr := assertCode(t, err, CodeState)
	if domainErr.Field != "parameterVersion" {
		t.Fatalf("unexpected stale snapshot error: %+v", domainErr)
	}
}
