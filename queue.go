package orchestrator

import "fmt"

// Queue is the cross-dialogue presentation queue. Interruptions never
// reorder it; an interrupted dialogue keeps its position and resumes
// in place instead of moving to the tail.
type Queue struct {
	order []string       // dialogue IDs in presentation order
	pos   map[string]int // dialogue ID -> queue position
}

// BuildQueue validates the declared queue order against the project
// dialogues and returns the queue. Duplicates, unknown IDs or missing
// dialogues yield E_QUEUE with the conflict position.
func BuildQueue(p *Project) (*Queue, *Error) {
	declared := p.Policy.QueueOrder
	if len(declared) != len(p.Dialogues) {
		return nil, newError(EQueue, "", "queue_order",
			fmt.Sprintf("queue order declares %d entries for %d dialogues",
				len(declared), len(p.Dialogues)))
	}
	q := &Queue{order: make([]string, len(declared)), pos: make(map[string]int, len(declared))}
	for i, id := range declared {
		if id == "" {
			e := newError(EQueue, "", "queue_order", "empty dialogue id in queue order")
			e.Position = i
			return nil, e
		}
		if _, dup := q.pos[id]; dup {
			e := newError(EQueue, id, "queue_order", "duplicate dialogue in queue order")
			e.Position = i
			return nil, e
		}
		if p.DialogueByID(id) == nil {
			e := newError(EQueue, id, "queue_order", "queue order references unknown dialogue")
			e.Position = i
			return nil, e
		}
		q.order[i] = id
		q.pos[id] = i
	}
	return q, nil
}

// Order returns the presentation order (copy).
func (q *Queue) Order() []string {
	out := make([]string, len(q.order))
	copy(out, q.order)
	return out
}

// Position returns the queue position of a dialogue.
func (q *Queue) Position(id string) (int, bool) {
	p, ok := q.pos[id]
	return p, ok
}

// At returns the dialogue ID at a queue position.
func (q *Queue) At(pos int) string { return q.order[pos] }

// Len returns the queue length.
func (q *Queue) Len() int { return len(q.order) }

// CheckResumePosition verifies that a dialogue requesting resume is
// actually in the resume-pending set and still occupies its original
// queue position.
func (q *Queue) CheckResumePosition(id string, resumePending map[string]bool) *Error {
	pos, ok := q.pos[id]
	if !ok {
		return newError(EQueue, id, "queue_order", "resume requested for dialogue not in queue")
	}
	if !resumePending[id] {
		e := newError(EQueue, id, "queue_order", "resume position missing: dialogue is not resume-pending")
		e.Position = pos
		return e
	}
	if q.order[pos] != id {
		e := newError(EQueue, id, "queue_order", "queue position conflict")
		e.Position = pos
		return e
	}
	return nil
}
