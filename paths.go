package audiocal

import "sort"

// ExportResult 汇总任一导出路径的确定性产物。
type ExportResult struct {
	Firmware       *FirmwareConfig
	Bytes          []byte
	CommitSequence []int // 段提交顺序（段号序列）
	Records        []CheckpointRecord
	Model          *CalibrationModel
}

// commitSequence 从检查点记录提取按提交顺序的段号。
func commitSequence(recs []CheckpointRecord) []int {
	ordered := append([]CheckpointRecord(nil), recs...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	seq := make([]int, len(ordered))
	for i, r := range ordered {
		seq[i] = r.Index
	}
	return seq
}

// ExportInteractive 交互式逐段导出：逐段 pending->exporting->committed。
func ExportInteractive(model *CalibrationModel, store *CheckpointStore) (*ExportResult, error) {
	exp, err := NewExporter(model, store)
	if err != nil {
		return nil, err
	}
	_, total := exp.Progress()
	for i := 0; i < total; i++ {
		if _, err := exp.ExportSegment(i); err != nil {
			return nil, err
		}
	}
	return finalize(exp, model)
}

// ExportResumable 断点恢复导出：先加载检查点恢复已提交段，再逐段补齐未完成段。
func ExportResumable(model *CalibrationModel, store *CheckpointStore) (*ExportResult, error) {
	recs, err := store.LoadAll(TargetFingerprint(model.Target), model.Config.ParamVersion, model.AlgorithmID)
	if err != nil {
		if be, ok := AsBizError(err); ok && be.Code == CodeStore {
			// 快照作废：全量重算（业务要求 6）。
			recs = nil
		} else {
			return nil, err
		}
	}
	exp, err := NewExporter(model, store)
	if err != nil {
		return nil, err
	}
	exp.Restore(recs)
	_, total := exp.Progress()
	for i := 0; i < total; i++ {
		if _, err := exp.ExportSegment(i); err != nil {
			return nil, err
		}
	}
	return finalize(exp, model)
}

// ExportAllAtOnce 一次性全量导出：单轮提交全部段（仍走同一状态机与同一编码器）。
func ExportAllAtOnce(model *CalibrationModel, store *CheckpointStore) (*ExportResult, error) {
	exp, err := NewExporter(model, store)
	if err != nil {
		return nil, err
	}
	_, total := exp.Progress()
	for i := 0; i < total; i++ {
		if _, err := exp.ExportSegment(i); err != nil {
			return nil, err
		}
	}
	return finalize(exp, model)
}

func finalize(exp *Exporter, model *CalibrationModel) (*ExportResult, error) {
	if !exp.Complete() {
		return nil, newErr(CodeState, "导出未完成，不能生成完整导出报告", "progress")
	}
	fw := exp.Firmware()
	recs := exp.CommittedRecords()
	return &ExportResult{
		Firmware:       fw,
		Bytes:          fw.Encode(),
		CommitSequence: commitSequence(recs),
		Records:        recs,
		Model:          model,
	}, nil
}

// CompareResults 自动比对两条导出结果，不一致返回 E_DIFF 并定位字节与段号。
func CompareResults(a, b *ExportResult) error {
	if err := compareBytes("firmware", a.Bytes, b.Bytes, a.Firmware); err != nil {
		return err
	}
	if !equalInts(a.CommitSequence, b.CommitSequence) {
		return &BizError{
			Code:    CodeDiff,
			Message: "段提交序列不一致",
			Field:   "commit_sequence",
			Detail:  map[string]any{"left": a.CommitSequence, "right": b.CommitSequence},
		}
	}
	if len(a.Records) != len(b.Records) || len(a.Firmware.Segments) != len(b.Firmware.Segments) {
		return newErr(CodeDiff, "段数量不一致", "segments")
	}
	for i := range a.Firmware.Segments {
		if a.Firmware.Segments[i].ContentHash != b.Firmware.Segments[i].ContentHash {
			return &BizError{
				Code:    CodeDiff,
				Message: "段内容哈希不一致",
				Field:   "segment=" + itoa(i),
			}
		}
	}
	return nil
}

func compareBytes(label string, x, y []byte, fw *FirmwareConfig) error {
	n := len(x)
	if len(y) < n {
		n = len(y)
	}
	for i := 0; i < n; i++ {
		if x[i] != y[i] {
			segIdx := -1
			if fw != nil {
				segIdx = LocateByteSegment(fw, i)
			}
			return &BizError{
				Code:    CodeDiff,
				Message: "固件字节在首个不一致偏移处不同",
				Field:   label + "@offset=" + itoa(i),
				Detail: map[string]any{
					"offset":        i,
					"segment_index": segIdx,
					"left_byte":     x[i],
					"right_byte":    y[i],
				},
			}
		}
	}
	if len(x) != len(y) {
		return &BizError{
			Code:    CodeDiff,
			Message: "固件长度不一致",
			Field:   label + "@length",
			Detail:  map[string]any{"left_len": len(x), "right_len": len(y)},
		}
	}
	return nil
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// LocateByteSegment 在固件配置中精确定位字节偏移所属段编号。
func LocateByteSegment(fw *FirmwareConfig, offset int) int {
	b := fw.Encode()
	if offset < HeaderSize || offset >= len(b) {
		return -1
	}
	cursor := HeaderSize
	cursor += len(fw.ChannelMap) * 12
	cursor += len(fw.Grid) * 8
	for si, s := range fw.Segments {
		segLen := 4*4 + 8 + 8 + 1 + len(s.Coeffs)*8 + len(s.EnvelopeDB)*8 + 32
		if offset >= cursor && offset < cursor+segLen {
			return si
		}
		cursor += segLen
	}
	return -1
}
