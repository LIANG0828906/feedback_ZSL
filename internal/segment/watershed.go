package segment

import (
	"container/heap"
)

type pqItem struct {
	priority float64
	index    int
	label    int32
	seq      int
}

type priorityQueue []*pqItem

func (pq priorityQueue) Len() int { return len(pq) }
func (pq priorityQueue) Less(i, j int) bool {
	if pq[i].priority != pq[j].priority {
		return pq[i].priority < pq[j].priority
	}
	if pq[i].index != pq[j].index {
		return pq[i].index < pq[j].index
	}
	return pq[i].seq < pq[j].seq
}
func (pq priorityQueue) Swap(i, j int) { pq[i], pq[j] = pq[j], pq[i] }
func (pq *priorityQueue) Push(x any)   { *pq = append(*pq, x.(*pqItem)) }
func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*pq = old[:n-1]
	return item
}

func priorityFlood(gradient []float64, labels []int32, w, h int, floodThreshold float64) (int, []int32) {
	out := make([]int32, len(labels))
	copy(out, labels)
	pq := make(priorityQueue, 0, w*h/4)
	seq := 0
	for i, label := range out {
		if label != 0 {
			heap.Push(&pq, &pqItem{priority: gradient[i], index: i, label: label, seq: seq})
			seq++
		}
	}

	samples := 0
	for pq.Len() > 0 {
		item := heap.Pop(&pq).(*pqItem)
		samples++
		x, y := item.index%w, item.index/w
		for _, n := range neighbor4Indices(x, y, w, h) {
			if out[n] != 0 {
				continue
			}
			level := gradient[n]
			if level > floodThreshold {
				continue
			}
			out[n] = item.label
			priority := item.priority
			if level > priority {
				priority = level
			}
			heap.Push(&pq, &pqItem{priority: priority, index: n, label: item.label, seq: seq})
			seq++
		}
	}
	return samples, out
}

func neighbor4Indices(x, y, w, h int) []int {
	var out []int
	if x > 0 {
		out = append(out, y*w+x-1)
	}
	if x+1 < w {
		out = append(out, y*w+x+1)
	}
	if y > 0 {
		out = append(out, (y-1)*w+x)
	}
	if y+1 < h {
		out = append(out, (y+1)*w+x)
	}
	return out
}
