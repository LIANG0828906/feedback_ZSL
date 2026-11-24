package audiocal

import "testing"

func TestFitMeetsAnalyticUpperBound(t *testing.T) {
	for _, s := range GenerateSamples(20261124, 3) {
		cfg := DefaultFitConfig()
		model, err := Fit(s, cfg)
		if err != nil {
			t.Fatalf("样本 %s 拟合失败: %v", s.ID, err)
		}
		if len(model.Channels) != len(s.Channels) {
			t.Fatalf("通道数不匹配")
		}
		for ci, ch := range model.Channels {
			if len(ch.Beta) != cfg.BasisCount {
				t.Fatalf("滤波器参数维度错误")
			}
			if len(ch.FIRTaps) != cfg.FIRTaps {
				t.Fatalf("FIR 抽头数错误")
			}
			for pi, r := range ch.Residuals {
				if r > s.Channels[ci].Truth.ResidualUpperDB[pi] {
					t.Fatalf("样本 %s 通道 %d 频点 %d 残差 %.4f 超上界 %.4f",
						s.ID, ci, pi, r, s.Channels[ci].Truth.ResidualUpperDB[pi])
				}
			}
		}
	}
}

func TestRegularizationMonotonicAndStableOrder(t *testing.T) {
	s := GenerateSamples(20261124, 3)[0]
	lambdas := []float64{1e-6, 1e-4, 1e-3, 1e-2}
	prevSSE := -1.0
	prevOrder := -1
	for _, lam := range lambdas {
		cfg := DefaultFitConfig()
		cfg.Regularization = lam
		m, err := Fit(s, cfg)
		if err != nil {
			t.Fatal(err)
		}
		sse := m.TotalSSE()
		if sse+1e-9 < prevSSE {
			t.Fatalf("λ=%v 收紧时 SSE 下降: %.6f < %.6f", lam, sse, prevSSE)
		}
		order := len(m.Channels[0].Beta)
		if prevOrder != -1 && order != prevOrder {
			t.Fatalf("正则收紧改变了滤波器阶数: %d -> %d", prevOrder, order)
		}
		prevSSE, prevOrder = sse, order
	}
}

func TestFitViolationReturnsEFiT(t *testing.T) {
	s := GenerateSamples(1, 1)[0]
	// 构造无法满足的上界：把所有上界压到极小。
	for ci := range s.Channels {
		for pi := range s.Channels[ci].Truth.ResidualUpperDB {
			s.Channels[ci].Truth.ResidualUpperDB[pi] = 1e-9
		}
	}
	_, err := Fit(s, DefaultFitConfig())
	be, ok := AsBizError(err)
	if !ok || be.Code != CodeFit {
		t.Fatalf("期望 E_FIT，得到 %v", err)
	}
	if be.Detail["channel_index"] == nil || be.Detail["point_index"] == nil {
		t.Fatalf("E_FIT 未给出超差通道与频点索引: %+v", be.Detail)
	}
}

func TestConfigurableGridFit(t *testing.T) {
	s := GenerateSamples(1, 1)[0]
	cfg := DefaultFitConfig()
	cfg.GridFrequencies = []float64{80, 160, 320, 640, 1280, 2560, 5120, 10240}
	m, err := Fit(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Grid) != 8 || len(m.Channels[0].Residuals) != 8 {
		t.Fatal("可配置网格未生效")
	}
}
