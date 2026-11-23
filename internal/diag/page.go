package diag

import (
	"fmt"
	"strings"

	"haptickernel/internal/synth"
)

const style = `
<style>
body{font-family:system-ui,Arial,sans-serif;margin:0;background:#0f1220;color:#e6e9f5}
header{padding:14px 22px;background:#171b30;border-bottom:1px solid #2a3052}
h1{font-size:18px;margin:0} h2{font-size:15px;color:#9db4ff;margin:18px 0 8px}
main{padding:16px 22px;display:grid;grid-template-columns:1fr 1fr;gap:18px}
section{background:#171b30;border:1px solid #2a3052;border-radius:10px;padding:14px}
table{border-collapse:collapse;width:100%;font-size:12px}
th,td{border:1px solid #2a3052;padding:4px 6px;text-align:left}
th{color:#9db4ff;background:#1d2240}
.ok{color:#54e08a}.bad{color:#ff7a7a}.warn{color:#ffd166}
.tag{display:inline-block;padding:1px 7px;border-radius:999px;font-size:11px}
.freeze{background:#26304d;color:#9db4ff}.full{grid-column:1 / -1}
svg{background:#0f1220;border-radius:8px}
input{background:#0f1220;border:1px solid #2a3052;color:#e6e9f5;padding:6px;border-radius:6px}
button{background:#3a57ff;color:#fff;border:0;padding:7px 14px;border-radius:6px;cursor:pointer}
code{color:#ffd166}
</style>`

func renderPage(s *AppState) string {
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset='utf-8'><title>触觉量化内核诊断</title>" + style + "</head><body>")
	b.WriteString("<header><h1>触觉事件通道合并与包络量化内核 · 本地诊断</h1>")
	b.WriteString("<div style='font-size:12px;color:#9aa3c7;margin-top:4px'>仅监听 127.0.0.1 · 确定性样本 · 三路径一致性验证</div></header><main>")

	renderCompare(&b, s)
	renderEventTimeline(&b, s)
	renderCurves(&b, s)
	renderPackets(&b, s)
	renderInvalidation(&b, s)
	renderAlign(&b, s)
	renderRetry(&b, s)

	b.WriteString("</main></body></html>")
	return b.String()
}

func renderCompare(b *strings.Builder, s *AppState) {
	b.WriteString("<section class='full'><h2>三路径比对（实时 / 断点恢复 / 持久化全量重放）</h2>")
	if s.CompareReport == nil {
		b.WriteString("<p class='warn'>尚未运行</p></section>")
		return
	}
	r := s.CompareReport
	status := "<span class='tag ok'>PASS</span>"
	if !r.Passed {
		status = "<span class='tag bad'>FAIL E_DIFF</span>"
	}
	b.WriteString("<p>结果 " + status + "</p><table><tr><th>路径</th><th>采样点数</th><th>包数</th></tr>")
	for _, p := range r.PathOrder {
		fmt.Fprintf(b, "<tr><td>%s</td><td>%d</td><td>%d</td></tr>", p, r.SampleTotals[p], r.PacketCounts[p])
	}
	b.WriteString("</table>")
	if r.FirstDiff != nil {
		fmt.Fprintf(b, "<p class='bad'>首个不一致: %s ↔ %s @ %s (a=%d b=%d)</p>",
			r.FirstDiff.PathA, r.FirstDiff.PathB, r.FirstDiff.LogicalTime,
			r.FirstDiff.ValueA, r.FirstDiff.ValueB)
	}
	if s.SerialVsParallel != nil {
		par := "<span class='tag ok'>串行=并行</span>"
		if !s.SerialVsParallel.Identical {
			par = "<span class='tag bad'>串行≠并行</span>"
		}
		fmt.Fprintf(b, "<p>并发校验 %s workers=%d 背压事件=%d</p>", par,
			s.SerialVsParallel.Workers, len(s.SerialVsParallel.Backpressure))
	}
	b.WriteString("</section>")
}

func renderEventTimeline(b *strings.Builder, s *AppState) {
	b.WriteString("<section><h2>事件流时间轴与逐事件决策</h2><table><tr><th>帧</th><th>事件</th><th>类型</th><th>通道</th><th>优先级</th><th>禁断</th><th>决策</th><th>原因</th></tr>")
	dec := map[string]synth.EventDecision{}
	for _, c := range s.Realtime.Curves {
		for _, d := range c.Events {
			dec[d.EventID] = d
		}
	}
	for _, ev := range s.Events {
		d := dec[ev.ID]
		cls := "ok"
		if d.Decision == synth.DecisionDropped {
			cls = "bad"
		} else if d.Decision == synth.DecisionReplaced {
			cls = "warn"
		}
		fmt.Fprintf(b, "<tr><td>%d/%d</td><td>%s</td><td>%s</td><td>%d</td><td>%d</td><td>%v</td><td class='%s'>%s</td><td>%s</td></tr>",
			ev.Frame, ev.FrameIndex, ev.ID, ev.Type, ev.Channel, ev.Priority, ev.NoInterrupt,
			cls, d.Decision, d.Reason)
	}
	b.WriteString("</table></section>")
}

func renderCurves(b *strings.Builder, s *AppState) {
	b.WriteString("<section><h2>各通道包络曲线（定点 16bit）</h2>")
	for _, curve := range s.Realtime.Curves {
		fmt.Fprintf(b, "<h2 style='font-size:13px'>通道 %d · %d 采样点</h2>", curve.Channel, len(curve.Frames))
		b.WriteString(sparkline(curve))
	}
	b.WriteString("</section>")
}

func sparkline(curve *synth.ChannelCurve) string {
	const w, h = 360, 70
	var sb strings.Builder
	fmt.Fprintf(&sb, "<svg width='%d' height='%d' viewBox='0 0 %d %d'>", w, h, w, h)
	n := len(curve.Frames)
	if n == 0 {
		return sb.String() + "</svg>"
	}
	mid := h / 2
	for i, f := range curve.Frames {
		x := i * w / n
		barH := f.Value * (h / 2) / 32767
		y := mid - barH
		if barH < 0 {
			y = mid
		}
		fmt.Fprintf(&sb, "<rect x='%d' y='%d' width='2' height='%d' fill='#5b8cff'/>",
			x, y, abs(barH))
	}
	sb.WriteString("</svg>")
	return sb.String()
}

func renderPackets(b *strings.Builder, s *AppState) {
	b.WriteString("<section class='full'><h2>定点包序列与冻结状态</h2><table><tr><th>Seq</th><th>通道</th><th>通道内序号</th><th>起始偏移</th><th>采样点</th><th>时序(us)</th><th>依赖事件</th><th>状态</th></tr>")
	for _, pkt := range s.Realtime.Packets {
		fmt.Fprintf(b, "<tr><td>%d</td><td>%d</td><td>%d</td><td>%d</td><td>%d</td><td>%d</td><td>%s</td><td><span class='tag freeze'>已提交·冻结</span></td></tr>",
			pkt.Seq, pkt.Channel, pkt.ChannelIndex, pkt.StartOffset, pkt.SampleCount,
			pkt.TimingUS, strings.Join(pkt.DependEvents, ","))
	}
	b.WriteString("</table><p style='font-size:12px;color:#9aa3c7'>已提交包冻结，后续编辑只作用于未提交包。</p></section>")
}

func renderInvalidation(b *strings.Builder, s *AppState) {
	b.WriteString("<section><h2>失效传播清单（编辑事件优先级演示）</h2><table><tr><th>Seq</th><th>通道</th><th>原因</th><th>复用</th></tr>")
	for _, e := range s.Invalidations {
		cls := "bad"
		state := "失效重算"
		if e.Reused {
			cls = "ok"
			state = "复用缓存"
		}
		fmt.Fprintf(b, "<tr><td>%d</td><td>%d</td><td>%s</td><td class='%s'>%s</td></tr>",
			e.Seq, e.Channel, e.Reason, cls, state)
	}
	b.WriteString("</table></section>")
}

func renderAlign(b *strings.Builder, s *AppState) {
	b.WriteString("<section><h2>回显对齐与异常分类</h2>")
	if s.AlignReport == nil {
		b.WriteString("<p class='warn'>尚未运行设备回环</p></section>")
		return
	}
	r := s.AlignReport
	status := "<span class='tag ok'>全部对齐</span>"
	if !r.OK {
		status = "<span class='tag bad'>存在异常</span>"
	}
	fmt.Fprintf(b, "<p>%s 对齐包数=%d 异常数=%d</p>", status, r.AlignedCount, len(r.Anomalies))
	b.WriteString("<table><tr><th>错误码</th><th>侧</th><th>Seq</th><th>通道</th><th>逻辑时刻(us)</th><th>原因</th></tr>")
	for _, a := range r.Anomalies {
		cls := "warn"
		if a.Side == "device" {
			cls = "bad"
		}
		fmt.Fprintf(b, "<tr class='%s'><td><code>%s</code></td><td>%s</td><td>%d</td><td>%d</td><td>%d</td><td>%s</td></tr>",
			cls, a.Code, a.Side, a.Seq, a.Channel, a.LogicalTime, a.Reason)
	}
	b.WriteString("</table></section>")
}

func renderRetry(b *strings.Builder, s *AppState) {
	b.WriteString("<section class='full'><h2>异常注入与重试（回环设备 " + s.DeviceURL + "）</h2>")
	b.WriteString("<form method='post' action='/api/retry'>")
	b.WriteString("缺包 seq 列表 <input name='miss' placeholder='例如 3,7'/> ")
	b.WriteString("重发 seq 列表 <input name='dup' placeholder='例如 5'/> ")
	b.WriteString("超时 seq 列表 <input name='timeout' placeholder='例如 9'/> ")
	b.WriteString("<button type='submit'>重新提交并对齐</button></form>")
	b.WriteString("<p style='font-size:12px;color:#9aa3c7'>设备侧返回 E_MISS / E_DUP / E_TIMEOUT；内核侧违规统一 E_LOCAL，两类严格区分。</p></section>")
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
