package locklab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// RuntimeStateFingerprint covers every field that can affect future decisions,
// while excluding replayable audit/result fields. It is used to splice cached
// suffixes during incremental recomputation.
func RuntimeStateFingerprint(state *RuntimeState) string {
	if state == nil {
		return ""
	}
	payload, _ := json.Marshal(struct {
		ClockMs         int64                      `json:"clock_ms"`
		NextStep        int                        `json:"next_step"`
		CommitSeq       uint64                     `json:"commit_seq"`
		NextWaitSeq     int                        `json:"next_wait_seq"`
		NextWriteSeq    uint64                     `json:"next_write_seq"`
		Transactions    map[string]*TxState        `json:"transactions"`
		Locks           map[string]HeldLock        `json:"locks"`
		WaitQueues      map[string][]WaitRequest   `json:"wait_queues"`
		Versions        map[string][]VersionRecord `json:"versions"`
		ReadFacts       []ReadFact                 `json:"read_facts"`
		WriteFacts      []WriteRecord              `json:"write_facts"`
		Commits         []CommitEvent              `json:"commits"`
		Rollbacks       []RollbackEvent            `json:"rollbacks"`
		DeadlockVictims []string                   `json:"deadlock_victims"`
	}{
		ClockMs: state.ClockMs, NextStep: state.NextStep, CommitSeq: state.CommitSeq,
		NextWaitSeq: state.NextWaitSeq, NextWriteSeq: state.NextWriteSeq,
		Transactions: state.Transactions, Locks: state.Locks, WaitQueues: state.WaitQueues,
		Versions: state.Versions, ReadFacts: state.ReadFacts, WriteFacts: state.WriteFacts,
		Commits: state.Commits, Rollbacks: state.Rollbacks,
		DeadlockVictims: state.DeadlockVictims,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ValidateWaitGraphReferences is used by checkpoint recovery and fault
// injection. It rejects edges whose endpoints are absent from the snapshot.
func ValidateWaitGraphReferences(state *RuntimeState, edges []WaitGraphEdge) error {
	if state == nil {
		return NewError(EWaitGraphDangling, "$.wait_graph", "runtime state is nil")
	}
	for i, edge := range edges {
		if _, ok := state.Transactions[edge.From]; !ok {
			return NewError(EWaitGraphDangling, "$.wait_graph["+itoa(i)+"].from",
				"wait graph source %q is not present in transaction snapshot", edge.From)
		}
		if _, ok := state.Transactions[edge.To]; !ok {
			return NewError(EWaitGraphDangling, "$.wait_graph["+itoa(i)+"].to",
				"wait graph target %q is not present in transaction snapshot", edge.To)
		}
	}
	return nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := make([]byte, 0, 12)
	for v > 0 {
		digits = append(digits, byte('0'+v%10))
		v /= 10
	}
	for left, right := 0, len(digits)-1; left < right; left, right = left+1, right-1 {
		digits[left], digits[right] = digits[right], digits[left]
	}
	return string(digits)
}
