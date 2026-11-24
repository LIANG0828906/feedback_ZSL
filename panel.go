package audiocal

import (
	"fmt"
	"strings"
)

// Panel 聚合面板所需数据并渲染为纯文本（无第三方依赖，终端/日志均可展示）。
type Panel struct {
	Model     *CalibrationModel
	Exporter  *Exporter
	Echo      []*EchoReport
	FWSummary string
	FWLen     int
}

// NewPanel 基于模型与导出器构建面板。
func NewPanel(model *CalibrationModel, exp *Exporter) *Panel {
	p := &Panel{Model: model, Exporter: exp}
	if exp != nil {
		n, h := exp.Firmware().ByteSummary()
		p.FWLen, p.FWSummary = n, h
	}
	return p
}

// SetEcho 绑定回显校验明细。
func (p *Panel) SetEcho(reps []*EchoReport) { p.Echo = reps }

// Render 输出完整面板文本。
func (p *Panel) Render() string {
	var b strings.Builder
	b.WriteString("==== 声学校准可视化面板 ====\n")
	b.WriteString(fmt.Sprintf("样本: %s  采样率: %dHz  网格频点: %d  算法: %s\n",
		p.Model.SampleID, p.Model.SampleRate, len(p.Model.Grid), p.Model.AlgorithmID))

	if p.Exporter != nil {
		committed, total := p.Exporter.Progress()
		b.WriteString(fmt.Sprintf("\n[分段状态] 已提交 %d/%d\n", committed, total))
		for _, sp := range p.Exporter.States() {
			b.WriteString(fmt.Sprintf("  段 %02d  %s\n", sp.Index, stateGlyph(sp.State)))
		}
		b.WriteString(fmt.Sprintf("固件字节: %d 字节  SHA256: %s\n", p.FWLen, p.FWSummary))
		if diag := p.Exporter.Firmware().Diagnostics; diag != nil {
			b.WriteString(fmt.Sprintf("[合并诊断] 自然段 %d -> 上限 %d, 合并 %d 处\n",
				diag.RequestedSegments, diag.MaxSegments, len(diag.MergedPairs)))
		}
	}

	b.WriteString("\n[拟合残差曲线 |residual| dB]\n")
	for _, ch := range p.Model.Channels {
		b.WriteString(fmt.Sprintf("  通道 %d SSE=%.5f  曲线: ", ch.ChannelID, sseOf(ch.Residuals)))
		b.WriteString(sparkline(ch.Residuals))
		b.WriteString("\n")
	}

	if len(p.Echo) > 0 {
		b.WriteString("\n[回显校验明细]\n")
		for _, r := range p.Echo {
			status := "PASS"
			if !r.Pass {
				status = "FAIL"
			}
			deltas := make([]float64, len(r.Points))
			for i, pt := range r.Points {
				deltas[i] = pt.DeltaDB
			}
			b.WriteString(fmt.Sprintf("  通道 %d %s 最大偏差 %.5fdB 容差 %.4fdB 曲线: ",
				r.ChannelID, status, r.MaxDelta, r.Tolerance))
			b.WriteString(sparkline(deltas))
			b.WriteString("\n")
		}
	}
	return b.String()
}

func stateGlyph(s SegmentState) string {
	switch s {
	case StatePending:
		return "待导出 ."
	case StateExporting:
		return "导出中 ~"
	case StateCommitted:
		return "已提交 #"
	case StateFailed:
		return "失败 x"
	default:
		return string(s)
	}
}

func sseOf(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x * x
	}
	return s
}

// sparkline 把数值序列渲染为块字符迷你曲线。
func sparkline(xs []float64) string {
	if len(xs) == 0 {
		return ""
	}
	const blocks = " _.-=+*#@"
	min, max := xs[0], xs[0]
	for _, x := range xs {
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
	}
	var b strings.Builder
	span := max - min
	for _, x := range xs {
		var idx int
		if span > 0 {
			idx = int((x - min) / span * float64(len(blocks)-1))
		}
		b.WriteByte(blocks[idx])
	}
	return b.String()
}
