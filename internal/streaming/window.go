package streaming

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"occlbench/internal/culling"
)

// Window 一个流式加载窗口覆盖轨迹上的一段采样时刻。
type Window struct {
	Index       int            `json:"index"`
	SampleStart int            `json:"sampleStart"`
	SampleEnd   int            `json:"sampleEnd"` // 不含
	State       State          `json:"state"`
	Visible     []int          `json:"visible"`
	Occluded    []int          `json:"occluded"`
	GridSummary string         `json:"gridSummary"`
	LoadList    []int          `json:"loadList"`
	UnloadList  []int          `json:"unloadList"`
	Delayed     []DelayedChunk `json:"delayed"`
	Resident    []int          `json:"resident"`
	// 冻结字段：提交后不再随相机/参数变化。
	Frozen       bool   `json:"frozen"`
	ContentHash  string `json:"contentHash"`
	FailureCause string `json:"failureCause,omitempty"`
	// 轨迹消费偏移（到窗口末尾为止已消费的采样数）。
	ConsumedOffset int `json:"consumedOffset"`
}

// Freeze 提交时冻结可见集合、加载清单与栅格摘要，并计算内容哈希。
func (w *Window) Freeze() {
	w.Visible = sortedCopy(w.Visible)
	w.Occluded = sortedCopy(w.Occluded)
	w.LoadList = sortedCopy(w.LoadList)
	w.UnloadList = sortedCopy(w.UnloadList)
	w.Resident = sortedCopy(w.Resident)
	if w.Delayed == nil {
		w.Delayed = []DelayedChunk{}
	}
	w.Frozen = true
	w.State = StateCommitted
	w.ContentHash = w.computeHash()
}

func sortedCopy(in []int) []int {
	out := append([]int{}, in...)
	sort.Ints(out)
	return out
}

// computeHash 对冻结的确定性内容求哈希，作为持久化与版本比对的输入。
func (w *Window) computeHash() string {
	var b strings.Builder
	b.WriteString("v1|")
	b.WriteString(joinInts(w.Visible))
	b.WriteString("|")
	b.WriteString(joinInts(w.Occluded))
	b.WriteString("|")
	b.WriteString(joinInts(w.LoadList))
	b.WriteString("|")
	b.WriteString(joinInts(w.UnloadList))
	b.WriteString("|")
	b.WriteString(joinInts(w.Resident))
	b.WriteString("|")
	b.WriteString(w.GridSummary)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:16])
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

// windowFromSample 汇总一个窗口覆盖的多个采样时刻的剔除结果为窗口级集合。
func aggregateWindow(index, start, end int, samples []culling.SampleResult) *Window {
	visSet := map[int]bool{}
	occSet := map[int]bool{}
	gridParts := make([]string, 0, len(samples))
	for k := start; k < end; k++ {
		sr := samples[k]
		for _, id := range sr.Visible {
			visSet[id] = true
		}
		for _, id := range sr.Occluded {
			occSet[id] = true
		}
		gridParts = append(gridParts, sr.GridSummary)
	}
	win := &Window{
		Index:       index,
		SampleStart: start,
		SampleEnd:   end,
		State:       StatePending,
		GridSummary: strings.Join(gridParts, ";"),
	}
	for id := range visSet {
		win.Visible = append(win.Visible, id)
	}
	for id := range occSet {
		win.Occluded = append(win.Occluded, id)
	}
	sort.Ints(win.Visible)
	sort.Ints(win.Occluded)
	return win
}
