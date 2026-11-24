package audiocal

import (
	"bytes"
	"testing"
)

func buildTestModel(t *testing.T) *CalibrationModel {
	t.Helper()
	s := GenerateSamples(20261124, 2)[0]
	m, err := Fit(s, DefaultFitConfig())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDeterministicEncoding(t *testing.T) {
	m := buildTestModel(t)
	fw, err := BuildFirmware(m)
	if err != nil {
		t.Fatal(err)
	}
	b1 := fw.Encode()
	b2 := fw.Encode()
	if !bytes.Equal(b1, b2) {
		t.Fatal("同一模型重复编码字节不一致")
	}
	if string(b1[:8]) != FirmwareMagic {
		t.Fatal("固件魔数错误")
	}
	// 独立重建的模型也必须逐字节一致。
	fw2, _ := BuildFirmware(buildTestModel(t))
	if !bytes.Equal(b1, fw2.Encode()) {
		t.Fatal("独立重建模型编码不一致（存在顺序/线程依赖）")
	}
}

func TestSegmentWindowConstraints(t *testing.T) {
	m := buildTestModel(t)
	fw, err := BuildFirmware(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, seg := range fw.Segments {
		if seg.EventEndUS-seg.EventStartUS > fw.WindowUS || seg.EventStartUS < 0 {
			t.Fatalf("段 %d 事件时序越窗", seg.Index)
		}
	}
	// 通道映射必须按逻辑通道排序。
	for i := 1; i < len(fw.ChannelMap); i++ {
		if fw.ChannelMap[i-1].LogicalChannel > fw.ChannelMap[i].LogicalChannel {
			t.Fatal("通道映射未排序")
		}
	}
}

func TestSegmentMergeAndDiagnostics(t *testing.T) {
	m := buildTestModel(t)
	// 单通道拆成多段后，把段槽压到通道下限，触发同通道窗内合并。
	m.Config.MaxCoeffsPerSeg = 16 // 64 抽头 -> 每通道 4 个自然段，共 16 段
	m.Config.MaxSegments = 4      // 每通道合并回 1 段
	fw, err := BuildFirmware(m)
	if err != nil {
		t.Fatal(err)
	}
	if fw.Diagnostics == nil || fw.Diagnostics.RequestedSegments <= fw.Diagnostics.MaxSegments {
		t.Fatal("缺少分段合并诊断")
	}
	if len(fw.Segments) != 4 {
		t.Fatalf("合并后应保留每通道 1 段，得到 %d", len(fw.Segments))
	}
	mergedAny := false
	for _, seg := range fw.Segments {
		if seg.Merged {
			mergedAny = true
		}
	}
	if !mergedAny {
		t.Fatal("未标记合并段")
	}
}

func TestIllegalWindowReturnsEWin(t *testing.T) {
	m := buildTestModel(t)
	m.Config.WindowUS = 0
	_, err := BuildFirmware(m)
	assertCode(t, err, CodeWin, "window")

	m2 := buildTestModel(t)
	m2.Config.MaxSegments = 0
	_, err = BuildFirmware(m2)
	assertCode(t, err, CodeWin, "max_segments")
}

func TestMergeOverflowWindowReportsDetails(t *testing.T) {
	m := buildTestModel(t)
	// 极窄窗口 + 极低分段上限：合并后的事件槽必然越过窗口总预算。
	m.Config.MaxSegments = 1
	m.Config.WindowUS = 2
	_, err := BuildFirmware(m)
	be, ok := AsBizError(err)
	if !ok || be.Code != CodeWin {
		t.Fatalf("期望 E_WIN，得到 %v", err)
	}
	if be.Detail["actual_segments"] == nil || be.Detail["window_us"] == nil {
		t.Fatalf("E_WIN 未指出实测段数与窗宽: %+v", be.Detail)
	}
}

func TestLocateByteSegment(t *testing.T) {
	m := buildTestModel(t)
	fw, _ := BuildFirmware(m)
	if idx := LocateByteSegment(fw, 0); idx != -1 {
		t.Fatalf("头部偏移应返回 -1，得到 %d", idx)
	}
	if idx := LocateByteSegment(fw, HeaderSize+len(fw.ChannelMap)*12+len(fw.Grid)*8); idx != 0 {
		t.Fatalf("首个段体偏移应归属段 0，得到 %d", idx)
	}
}
