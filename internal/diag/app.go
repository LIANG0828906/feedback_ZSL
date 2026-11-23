package diag

import (
	"path/filepath"
	"sync"

	"haptickernel/internal/device"
	"haptickernel/internal/packetize"
	"haptickernel/internal/paths"
	"haptickernel/internal/sample"
	"haptickernel/internal/store"
	"haptickernel/internal/synth"
	"haptickernel/internal/validate"
)

// AppState 是诊断界面展示与重试所需的全局状态。
type AppState struct {
	Manifest         *synth.Manifest               `json:"manifest"`
	Events           []synth.Event                 `json:"events"`
	Realtime         *paths.RunResult              `json:"realtime"`
	Resume           *paths.RunResult              `json:"resume"`
	Replay           *paths.RunResult              `json:"replay"`
	CompareReport    *paths.CompareReport          `json:"compare_report"`
	CompareErr       *validate.BizError            `json:"compare_err,omitempty"`
	SerialVsParallel *ParallelReport               `json:"serial_vs_parallel"`
	Invalidations    []packetize.InvalidationEntry `json:"invalidations"`
	AlignReport      *device.AlignReport           `json:"align_report"`
	Backpressure     []synth.BackpressureEvent     `json:"backpressure"`
	DeviceURL        string                        `json:"device_url"`
	Fault            device.FaultSpec              `json:"fault"`
}

// StateHolder 用独立结构持锁，避免拷贝带锁的状态数据。
type StateHolder struct {
	mu    sync.RWMutex
	state AppState
}

func NewHolder() *StateHolder {
	return &StateHolder{}
}

// ParallelReport 比较串行与并行两条调度结果。
type ParallelReport struct {
	Identical    bool                      `json:"identical"`
	Workers      int                       `json:"workers"`
	Backpressure []synth.BackpressureEvent `json:"backpressure"`
	DiffDetail   string                    `json:"diff_detail,omitempty"`
}

func NewState() *StateHolder {
	return NewHolder()
}

// Snapshot 返回只读状态副本。
func (h *StateHolder) Snapshot() AppState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.state
}

// Bootstrap 生成确定性样本、加载校验，并执行完整三路径流水线。
func (h *StateHolder) Bootstrap(dataDir, cpDir string, workers int) error {
	if _, _, err := sample.Generate(dataDir); err != nil {
		return err
	}
	m, err := validate.LoadManifest(filepath.Join(dataDir, "manifest.json"))
	if err != nil {
		return err
	}
	events, err := validate.LoadEvents(filepath.Join(dataDir, "events.json"), m)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.state.Manifest = m
	h.state.Events = events
	h.mu.Unlock()

	windowPackets := 8

	// 路径 1：实时量化（并行工作池）。
	rtStore, err := store.NewCheckpointStore(filepath.Join(cpDir, "realtime"))
	if err != nil {
		return err
	}
	if err := rtStore.Reset(); err != nil {
		return err
	}
	rt, err := paths.RunRealtime(events, m, paths.Options{
		Store: rtStore, MaxWorkers: workers, MaxBufferFrames: 256, WindowPackets: windowPackets,
	})
	if err != nil {
		return err
	}

	// 路径 2：断点恢复。先把实时快照复制为恢复快照，再从恢复快照加载；
	// 若快照失效则整体作废并在恢复目录内全量重算、重新提交。
	if err := copyCheckpointDir(filepath.Join(cpDir, "realtime"), filepath.Join(cpDir, "resume")); err != nil {
		return err
	}
	resumeStore, err := store.NewCheckpointStore(filepath.Join(cpDir, "resume"))
	if err != nil {
		return err
	}
	resume, err := paths.RunResume(events, m, paths.Options{
		Store: resumeStore, WindowPackets: windowPackets,
	})
	if err != nil {
		return err
	}

	// 路径 3：仅依据持久化决策的全量重放（严格只读持久化窗口，不重新仲裁）。
	if err := copyCheckpointDir(filepath.Join(cpDir, "realtime"), filepath.Join(cpDir, "replay")); err != nil {
		return err
	}
	replayStore, err := store.NewCheckpointStore(filepath.Join(cpDir, "replay"))
	if err != nil {
		return err
	}
	replay, err := paths.RunReplay(events, m, paths.Options{
		Store: replayStore, WindowPackets: windowPackets,
	})
	if err != nil {
		return err
	}

	report, cmpErr := paths.Compare(rt, resume, replay)
	bizErr, _ := cmpErr.(*validate.BizError)

	// 串行 vs 并行一致性。
	parReport := h.compareSerialParallel(m, events, workers)

	// 失效传播演示：编辑第一个事件的优先级。
	edit := packetize.EditSpec{
		EditedEventIDs:  []string{events[0].ID},
		TouchedChannels: []int{events[0].Channel},
	}
	invalidations := packetize.PropagateInvalidation(rt.Packets, edit)

	h.mu.Lock()
	h.state.Realtime = rt
	h.state.Resume = resume
	h.state.Replay = replay
	h.state.CompareReport = report
	h.state.CompareErr = bizErr
	h.state.SerialVsParallel = parReport
	h.state.Invalidations = invalidations
	h.state.Backpressure = parReport.Backpressure
	h.mu.Unlock()
	return nil
}

func (h *StateHolder) compareSerialParallel(m *synth.Manifest, events []synth.Event, workers int) *ParallelReport {
	horizon := gridHorizon(events, m)
	serial := synth.SynthesizeAll(events, m, horizon)
	par := synth.SynthesizeConcurrent(events, m, horizon, synth.SchedulerConfig{
		MaxWorkers: workers, MaxBufferFrames: 256,
	})
	report := &ParallelReport{Identical: true, Workers: workers, Backpressure: par.Backpressure}
	for c := range serial {
		for k := range serial[c].Frames {
			if serial[c].Frames[k].Value != par.Curves[c].Frames[k].Value {
				report.Identical = false
				report.DiffDetail = "first diff channel " +
					itoa(serial[c].Channel) + " grid " + itoa(serial[c].Frames[k].GridOffset)
				return report
			}
		}
	}
	return report
}

// RunDeviceLoop 启动回环设备、提交全部包并对齐回显（含异常注入）。
func (h *StateHolder) RunDeviceLoop(fault device.FaultSpec) error {
	srv := device.NewServer()
	addr, err := srv.Start()
	if err != nil {
		return err
	}
	defer srv.Close()

	client := &device.Client{BaseURL: addr, Client: httpClient()}
	var echoes []device.Echo
	var timeouts []int
	for _, pkt := range h.state.Realtime.Packets {
		resp, err := client.Submit(pkt, fault)
		if err != nil {
			return err
		}
		if resp.Timeout {
			timeouts = append(timeouts, pkt.Seq)
			continue
		}
		echoes = append(echoes, resp.Echoes...)
	}

	m := h.state.Manifest
	localAnom := device.LocalCheck(h.state.Realtime.Packets, m.SamplesPerPacket,
		m.Channels[0].TimingWindow.MinOffsetUS, m.Channels[0].TimingWindow.MaxOffsetUS)
	report := device.Align(h.state.Realtime.Packets, echoes, timeouts)
	if localAnom != nil {
		report.Anomalies = append([]device.Anomaly{*localAnom}, report.Anomalies...)
	}
	h.mu.Lock()
	h.state.AlignReport = report
	h.state.DeviceURL = addr
	h.state.Fault = fault
	h.mu.Unlock()
	return nil
}

func gridHorizon(events []synth.Event, m *synth.Manifest) int {
	maxGrid := 0
	for _, ev := range events {
		tpl := m.Template(ev.Channel, ev.Template)
		end := ev.StartGrid(m.FrameGrid) + tpl.Duration
		if end > maxGrid {
			maxGrid = end
		}
	}
	if rem := maxGrid % m.SamplesPerPacket; rem != 0 {
		maxGrid += m.SamplesPerPacket - rem
	}
	return maxGrid
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
