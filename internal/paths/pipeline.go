package paths

import (
	"fmt"

	"haptickernel/internal/packetize"
	"haptickernel/internal/store"
	"haptickernel/internal/synth"
	"haptickernel/internal/validate"
)

// PathName 标识三条执行路径。
type PathName string

const (
	PathRealtime PathName = "realtime" // 实时量化
	PathResume   PathName = "resume"   // 断点恢复量化
	PathReplay   PathName = "replay"   // 仅依据持久化决策的全量重放
)

// RunResult 是一条路径的完整产物。
type RunResult struct {
	Path        PathName                 `json:"path"`
	Curves      []*synth.ChannelCurve    `json:"curves"`
	Packets     []*packetize.Packet      `json:"packets"`
	Summaries   []packetize.FrameSummary `json:"frame_summaries"`
	Windows     []*store.WindowRecord    `json:"windows"`
	SampleTotal int                      `json:"sample_total"`
}

// Options 控制一次运行使用哪条路径与并发度。
type Options struct {
	Store           *store.CheckpointStore
	MaxWorkers      int
	MaxBufferFrames int
	WindowPackets   int // 每个提交窗口包含多少个包
}

func horizonOf(events []synth.Event, m *synth.Manifest) int {
	maxGrid := 0
	for _, ev := range events {
		tpl := m.Template(ev.Channel, ev.Template)
		end := ev.StartGrid(m.FrameGrid) + tpl.Duration
		if end > maxGrid {
			maxGrid = end
		}
	}
	// 向上取整到每包采样点的整数倍，保证切分首尾相接、无残样。
	if rem := maxGrid % m.SamplesPerPacket; rem != 0 {
		maxGrid += m.SamplesPerPacket - rem
	}
	return maxGrid
}

func buildWindows(packets []*packetize.Packet, summaries []packetize.FrameSummary,
	m *synth.Manifest, windowPackets int) []*store.WindowRecord {
	var windows []*store.WindowRecord
	idx := 0
	for off := 0; off < len(packets); off += windowPackets {
		end := off + windowPackets
		if end > len(packets) {
			end = len(packets)
		}
		w := &store.WindowRecord{
			WindowIndex:      idx,
			ManifestVersion:  m.ManifestVersion,
			AlgoVersion:      synth.AlgoVersion,
			QuantUnit:        m.QuantUnit,
			SamplesPerPacket: m.SamplesPerPacket,
			Packets:          packets[off:end],
		}
		// 摘要只收本窗口包覆盖的范围。
		want := map[string]bool{}
		for _, pkt := range w.Packets {
			want[fmt.Sprintf("%d:%d", pkt.Channel, pkt.StartOffset)] = true
		}
		for _, sm := range summaries {
			if want[fmt.Sprintf("%d:%d", sm.Channel, sm.StartOffset)] {
				w.Summaries = append(w.Summaries, sm)
			}
		}
		windows = append(windows, w)
		idx++
	}
	return windows
}

func commitWindows(s *store.CheckpointStore, windows []*store.WindowRecord) error {
	for _, w := range windows {
		if err := s.CommitWindow(w); err != nil {
			return err
		}
	}
	return nil
}

// RunRealtime 实时量化路径：合成 -> 切包 -> 串行检查点提交。
func RunRealtime(events []synth.Event, m *synth.Manifest, opt Options) (*RunResult, error) {
	horizon := horizonOf(events, m)
	var curves []*synth.ChannelCurve
	if opt.MaxWorkers > 1 {
		res := synth.SynthesizeConcurrent(events, m, horizon, synth.SchedulerConfig{
			MaxWorkers: opt.MaxWorkers, MaxBufferFrames: opt.MaxBufferFrames,
		})
		curves = res.Curves
	} else {
		curves = synth.SynthesizeAll(events, m, horizon)
	}
	sliced, err := packetize.Slice(curves, m)
	if err != nil {
		return nil, err
	}
	windows := buildWindows(sliced.Packets, sliced.Summaries, m, opt.WindowPackets)
	if opt.Store != nil {
		if err := commitWindows(opt.Store, windows); err != nil {
			return nil, err
		}
	}
	return assemble(PathRealtime, curves, sliced, windows), nil
}

// RunResume 断点恢复路径：加载检查点，快照有效则复用；无效则全量重算并重新提交。
func RunResume(events []synth.Event, m *synth.Manifest, opt Options) (*RunResult, error) {
	snap, err := opt.Store.Load(m)
	if err != nil {
		return nil, err
	}
	if !snap.Valid {
		// 整体作废快照 -> 全量重放。
		if err := opt.Store.Reset(); err != nil {
			return nil, err
		}
		return RunRealtime(events, m, opt)
	}
	// 快照有效：从持久化窗口重建包序列，并重新合成曲线用于逐采样比对。
	horizon := horizonOf(events, m)
	curves := synth.SynthesizeAll(events, m, horizon)
	var packets []*packetize.Packet
	var summaries []packetize.FrameSummary
	for _, w := range snap.Windows {
		packets = append(packets, w.Packets...)
		summaries = append(summaries, w.Summaries...)
	}
	return assemble(PathResume, curves, &packetize.SliceResult{
		Packets: packets, Summaries: summaries,
	}, snap.Windows), nil
}

// RunReplay 全量重放路径：只依据持久化决策读取窗口，不重新做合成仲裁。
// 为输出逐采样曲线，仍按相同确定性算法重建帧，但包序列严格来自检查点。
func RunReplay(events []synth.Event, m *synth.Manifest, opt Options) (*RunResult, error) {
	snap, err := opt.Store.Load(m)
	if err != nil {
		return nil, err
	}
	if !snap.Valid {
		return nil, validate.NewBiz(validate.EStorage, "重放前快照无效", "checkpoint", -1, -1)
	}
	horizon := horizonOf(events, m)
	curves := synth.SynthesizeAll(events, m, horizon)
	var packets []*packetize.Packet
	var summaries []packetize.FrameSummary
	for _, w := range snap.Windows {
		packets = append(packets, w.Packets...)
		summaries = append(summaries, w.Summaries...)
	}
	return assemble(PathReplay, curves, &packetize.SliceResult{
		Packets: packets, Summaries: summaries,
	}, snap.Windows), nil
}

func assemble(path PathName, curves []*synth.ChannelCurve, sliced *packetize.SliceResult,
	windows []*store.WindowRecord) *RunResult {
	total := 0
	for _, c := range curves {
		total += len(c.Frames)
	}
	return &RunResult{
		Path: path, Curves: curves, Packets: sliced.Packets,
		Summaries: sliced.Summaries, Windows: windows, SampleTotal: total,
	}
}
