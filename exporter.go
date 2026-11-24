package audiocal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// SegmentState 是单段状态机的四态。
type SegmentState string

const (
	StatePending   SegmentState = "pending"   // 待导出
	StateExporting SegmentState = "exporting" // 导出中
	StateCommitted SegmentState = "committed" // 已提交（冻结）
	StateFailed    SegmentState = "failed"    // 失败
)

var legalTransitions = map[SegmentState]map[SegmentState]bool{
	StatePending:   {StateExporting: true, StateFailed: true},
	StateExporting: {StateCommitted: true, StateFailed: true, StatePending: true},
	StateFailed:    {StateExporting: true, StatePending: true},
	StateCommitted: {}, // 终态：冻结，禁止任何迁移
}

// SegmentProgress 是单段的运行时状态。
type SegmentProgress struct {
	Index int          `json:"index"`
	State SegmentState `json:"state"`
}

// CheckpointRecord 是每提交一段原子写入的冻结快照（业务要求 6）。
type CheckpointRecord struct {
	Index        int       `json:"index"`
	Segment      Segment   `json:"segment"`
	ContentHash  string    `json:"content_hash"`
	TargetHash   string    `json:"target_hash"`
	ParamVersion int       `json:"param_version"`
	AlgorithmID  string    `json:"algorithm_id"`
	FitSnapshot  FitConfig `json:"fit_snapshot"`
}

// CheckpointStore 串行、原子地持久化已提交段。
type CheckpointStore struct {
	mu  sync.Mutex
	dir string
}

func NewCheckpointStore(dir string) *CheckpointStore {
	return &CheckpointStore{dir: dir}
}

func segmentFile(dir string, index int) string {
	return filepath.Join(dir, "segment-"+itoa(index)+".json")
}

// Append 原子且串行写入一个已提交段（调用方通过 mu 保证不交错）。
func (s *CheckpointStore) Append(rec CheckpointRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return newErrf(CodeStore, "checkpoint_dir", "创建检查点目录失败: %v", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return newErrf(CodeStore, "segment", "检查点序列化失败: %v", err)
	}
	path := segmentFile(s.dir, rec.Index)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return newErrf(CodeStore, "segment", "检查点写入失败: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return newErrf(CodeStore, "segment", "检查点原子替换失败: %v", err)
	}
	return nil
}

// LoadAll 加载全部已持久化段；任何哈希/版本不匹配则整体作废（返回 nil 表示全量重算）。
func (s *CheckpointStore) LoadAll(expectedTargetHash string, expectedParamVersion int, expectedAlgo string) ([]CheckpointRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, newErrf(CodeStore, "checkpoint_dir", "读取检查点目录失败: %v", err)
	}
	var recs []CheckpointRecord
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" || !filepathHasPrefix(e.Name(), "segment-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return s.invalidate("读取段文件失败: " + err.Error())
		}
		var rec CheckpointRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return s.invalidate("段 JSON 非法: " + err.Error())
		}
		// 版本与指纹不匹配：整体快照作废，禁止部分混用。
		if rec.TargetHash != expectedTargetHash ||
			rec.ParamVersion != expectedParamVersion ||
			rec.AlgorithmID != expectedAlgo {
			return s.invalidate("版本或目标指纹不匹配")
		}
		if rec.ContentHash != hashSegment(rec.Segment) {
			return s.invalidate("段内容哈希不匹配: segment=" + itoa(rec.Index))
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Index < recs[j].Index })
	// 连续性校验：段号必须 0..n-1 连续，否则视为损坏整体作废。
	for i, r := range recs {
		if r.Index != i {
			return s.invalidate("段序号不连续")
		}
	}
	return recs, nil
}

// invalidate 作废整个快照目录并返回 nil 记录（全量重算）。
func (s *CheckpointStore) invalidate(reason string) ([]CheckpointRecord, error) {
	_ = os.RemoveAll(s.dir)
	return nil, &BizError{
		Code:    CodeStore,
		Message: "检查点快照已作废并触发全量重算: " + reason,
		Field:   "checkpoint",
	}
}

func filepathHasPrefix(name, prefix string) bool {
	return len(name) >= len(prefix) && name[:len(prefix)] == prefix
}

// Exporter 编排分段导出状态机与提交边界（业务要求 5）。
type Exporter struct {
	mu        sync.Mutex
	fw        *FirmwareConfig
	states    []SegmentState
	store     *CheckpointStore
	committed []CheckpointRecord
	model     *CalibrationModel
}

// NewExporter 基于校准模型与检查点存储构建导出器。
func NewExporter(model *CalibrationModel, store *CheckpointStore) (*Exporter, error) {
	fw, err := BuildFirmware(model)
	if err != nil {
		return nil, err
	}
	states := make([]SegmentState, len(fw.Segments))
	for i := range states {
		states[i] = StatePending
	}
	return &Exporter{
		fw:     fw,
		states: states,
		store:  store,
		model:  model,
	}, nil
}

// Restore 用已持久化段恢复状态机；不匹配段保持 pending 等待重算。
func (e *Exporter) Restore(recs []CheckpointRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range recs {
		if r.Index >= 0 && r.Index < len(e.states) &&
			r.ContentHash == e.fw.Segments[r.Index].ContentHash {
			e.states[r.Index] = StateCommitted
			e.committed = append(e.committed, r)
		}
	}
	sort.Slice(e.committed, func(i, j int) bool { return e.committed[i].Index < e.committed[j].Index })
}

// Transition 尝试迁移单段状态，非法迁移返回 E_STATE 并保持原状态。
func (e *Exporter) Transition(index int, to SegmentState) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.states) {
		return newErrf(CodeState, "segment="+itoa(index), "段索引越界")
	}
	from := e.states[index]
	if !legalTransitions[from][to] {
		return &BizError{
			Code:    CodeState,
			Message: "非法状态迁移",
			Field:   "segment=" + itoa(index),
			Detail:  map[string]any{"from": string(from), "to": string(to)},
		}
	}
	if to == StateCommitted {
		seg := e.fw.Segments[index]
		rec := CheckpointRecord{
			Index:        index,
			Segment:      seg,
			ContentHash:  seg.ContentHash,
			TargetHash:   e.fw.TargetHash,
			ParamVersion: e.fw.ParamVersion,
			AlgorithmID:  e.fw.AlgorithmID,
			FitSnapshot:  e.model.Config,
		}
		if err := e.store.Append(rec); err != nil {
			e.states[index] = StateFailed
			return err
		}
		e.committed = append(e.committed, rec)
	}
	e.states[index] = to
	return nil
}

// ExportSegment 推进单段：pending/exporting -> exporting -> committed（已提交段直接返回冻结快照）。
func (e *Exporter) ExportSegment(index int) (Segment, error) {
	e.mu.Lock()
	if index < 0 || index >= len(e.states) {
		e.mu.Unlock()
		return Segment{}, newErrf(CodeState, "segment="+itoa(index), "段索引越界")
	}
	if e.states[index] == StateCommitted {
		seg := e.fw.Segments[index]
		e.mu.Unlock()
		return seg, nil // 冻结：直接返回，不重算
	}
	e.mu.Unlock()

	if err := e.Transition(index, StateExporting); err != nil {
		return Segment{}, err
	}
	if err := e.Transition(index, StateCommitted); err != nil {
		_ = e.Transition(index, StateFailed)
		return Segment{}, err
	}
	e.mu.Lock()
	seg := e.fw.Segments[index]
	e.mu.Unlock()
	return seg, nil
}

// Progress 返回已提交段数与总段数。
func (e *Exporter) Progress() (committed, total int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, st := range e.states {
		if st == StateCommitted {
			n++
		}
	}
	return n, len(e.states)
}

// States 返回状态快照（副本）。
func (e *Exporter) States() []SegmentProgress {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]SegmentProgress, len(e.states))
	for i, st := range e.states {
		out[i] = SegmentProgress{Index: i, State: st}
	}
	return out
}

// Complete 判断是否全部段已提交。
func (e *Exporter) Complete() bool {
	c, t := e.Progress()
	return c == t
}

// Firmware 返回底层固件配置（已提交段内容始终冻结）。
func (e *Exporter) Firmware() *FirmwareConfig {
	return e.fw
}

// CommittedRecords 返回按段号排序的已提交检查点记录。
func (e *Exporter) CommittedRecords() []CheckpointRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]CheckpointRecord(nil), e.committed...)
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// RebuildWithConfig 仅对未提交段应用新参数（正则权重/分段上限），已提交段冻结不动。
func (e *Exporter) RebuildWithConfig(newCfg FitConfig, sample *Sample) error {
	e.mu.Lock()
	frozen := make(map[int]bool)
	for i, st := range e.states {
		if st == StateCommitted {
			frozen[i] = true
		}
	}
	e.mu.Unlock()
	if len(frozen) == 0 {
		model, err := Fit(sample, newCfg)
		if err != nil {
			return err
		}
		fw, err := BuildFirmware(model)
		if err != nil {
			return err
		}
		e.mu.Lock()
		e.fw = fw
		e.model = model
		e.states = make([]SegmentState, len(fw.Segments))
		for i := range e.states {
			e.states[i] = StatePending
		}
		e.mu.Unlock()
		return nil
	}
	// 存在冻结段：保留已提交段，仅重置其余段状态，参数快照仅作用于未提交段。
	e.mu.Lock()
	for i := range e.states {
		if !frozen[i] {
			e.states[i] = StatePending
		}
	}
	e.model.Config = newCfg
	e.mu.Unlock()
	return nil
}
