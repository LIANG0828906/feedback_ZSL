package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"haptickernel/internal/packetize"
	"haptickernel/internal/synth"
	"haptickernel/internal/validate"
)

// WindowRecord 是一个提交窗口的持久化记录（按包提交顺序稳定排列）。
type WindowRecord struct {
	WindowIndex      int                      `json:"window_index"`
	ManifestVersion  string                   `json:"manifest_version"`
	AlgoVersion      string                   `json:"algo_version"`
	QuantUnit        int                      `json:"quant_unit"`
	SamplesPerPacket int                      `json:"samples_per_packet"`
	Packets          []*packetize.Packet      `json:"packets"`
	Summaries        []packetize.FrameSummary `json:"frame_summaries"`
	ContentHash      string                   `json:"content_hash"`
}

// CheckpointStore 是文件形式检查点目录，所有写入串行且原子。
// 目录名只用相对短名，Windows 下同样可创建。
type CheckpointStore struct {
	dir string
	mu  sync.Mutex
}

func NewCheckpointStore(dir string) (*CheckpointStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, &validate.BizError{Code: validate.EStorage, Message: "创建检查点目录失败", Detail: err.Error()}
	}
	return &CheckpointStore{dir: dir}, nil
}

func (s *CheckpointStore) windowPath(index int) string {
	return filepath.Join(s.dir, fmt.Sprintf("window-%06d.json", index))
}

func (s *CheckpointStore) tempPath(index int) string {
	return filepath.Join(s.dir, fmt.Sprintf(".window-%06d.tmp", index))
}

// fingerprint 对窗口业务内容做稳定哈希（排序键、不依赖 map 遍历）。
func fingerprint(w *WindowRecord) string {
	h := sha256.New()
	write := func(b []byte) { h.Write(b) }
	write([]byte(w.ManifestVersion))
	write([]byte(w.AlgoVersion))
	write([]byte(fmt.Sprintf(":%d:%d:", w.QuantUnit, w.SamplesPerPacket)))
	for _, pkt := range w.Packets {
		write(pkt.Encode())
	}
	sums := append([]packetize.FrameSummary(nil), w.Summaries...)
	sort.SliceStable(sums, func(i, j int) bool {
		if sums[i].Channel != sums[j].Channel {
			return sums[i].Channel < sums[j].Channel
		}
		return sums[i].StartOffset < sums[j].StartOffset
	})
	for _, sm := range sums {
		write([]byte(sm.Hash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CommitWindow 原子且串行写入一个窗口；调用方负责保证窗口按序提交。
func (s *CheckpointStore) CommitWindow(w *WindowRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	w.ContentHash = fingerprint(w)
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return &validate.BizError{Code: validate.EStorage, Message: "窗口序列化失败", Detail: err.Error()}
	}
	tmp := s.tempPath(w.WindowIndex)
	final := s.windowPath(w.WindowIndex)
	// 写临时文件并 fsync，再 rename 原子替换，保证不会出现交错的半写文件。
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return &validate.BizError{Code: validate.EStorage, Message: "检查点写入失败", Detail: err.Error()}
	}
	if err := os.Rename(tmp, final); err != nil {
		return &validate.BizError{Code: validate.EStorage, Message: "检查点原子替换失败", Detail: err.Error()}
	}
	return nil
}

// LoadedSnapshot 是重启加载结果。
type LoadedSnapshot struct {
	Windows []*WindowRecord
	Valid   bool
	Reason  string
}

// Load 读取全部窗口；任一哈希或版本不匹配则整体作废（Valid=false），由上层全量重放。
func (s *CheckpointStore) Load(m *synth.Manifest) (*LoadedSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, &validate.BizError{Code: validate.EStorage, Message: "读取检查点目录失败", Detail: err.Error()}
	}
	var names []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	snap := &LoadedSnapshot{Valid: true}
	if len(names) == 0 {
		snap.Valid = false
		snap.Reason = "检查点为空，需要全量重放"
		return snap, nil
	}
	expectedIndex := 0
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			return nil, &validate.BizError{Code: validate.EStorage, Message: "读取窗口失败", Detail: err.Error()}
		}
		var w WindowRecord
		if err := json.Unmarshal(raw, &w); err != nil {
			snap.Valid = false
			snap.Reason = "窗口 JSON 损坏: " + name
			return snap, nil
		}
		if w.WindowIndex != expectedIndex {
			snap.Valid = false
			snap.Reason = fmt.Sprintf("窗口序号不连续: 期望 %d 实际 %d", expectedIndex, w.WindowIndex)
			return snap, nil
		}
		if w.ManifestVersion != m.ManifestVersion {
			snap.Valid = false
			snap.Reason = "能力参数版本不匹配"
			return snap, nil
		}
		if w.AlgoVersion != synth.AlgoVersion {
			snap.Valid = false
			snap.Reason = "算法版本不匹配"
			return snap, nil
		}
		// 包内容哈希逐个复核。
		for _, pkt := range w.Packets {
			if pkt.Hash() != pkt.ContentHash {
				snap.Valid = false
				snap.Reason = fmt.Sprintf("包内容哈希不匹配: seq=%d channel=%d", pkt.Seq, pkt.Channel)
				return snap, nil
			}
		}
		if fingerprint(&w) != w.ContentHash {
			snap.Valid = false
			snap.Reason = "窗口整体内容哈希不匹配: " + name
			return snap, nil
		}
		snap.Windows = append(snap.Windows, &w)
		expectedIndex++
	}
	return snap, nil
}

// Reset 作废并清空检查点目录（全量重放前调用）。
func (s *CheckpointStore) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(s.dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
