package locklab

import (
	"encoding/json"
	"fmt"
	"sort"
)

func (e *engine) performRead(tx *TxState, op Operation, step int, result *StepResult) {
	versions := e.state.Versions[op.ItemID]
	if len(versions) == 0 {
		result.Outcome = OutcomeSkipped
		return
	}
	observed := versions[len(versions)-1]
	dirty := false
	snapshotSeq := e.state.CommitSeq

	if tx.Isolation == RepeatableRead || tx.Isolation == Serializable {
		snapshotSeq = tx.SnapshotSeq
	}

	own, hasOwn := e.latestPendingWrite(tx.ID, op.ItemID)
	if hasOwn {
		observed = VersionRecord{
			ItemID: op.ItemID, Number: e.tentativeVersionNumber(op.ItemID),
			Value: own.Value, WriterTx: tx.ID, CommitSeq: 0,
			WriteStep: own.ExecutedStep, CreatedAtMs: own.AtMs,
		}
	} else if tx.Isolation == ReadUncommitted {
		if pending, ok := e.latestPendingWriteAny(op.ItemID); ok {
			observed = VersionRecord{
				ItemID: op.ItemID, Number: e.tentativeVersionNumber(op.ItemID),
				Value: pending.write.Value, WriterTx: pending.txID, CommitSeq: 0,
				WriteStep: pending.write.ExecutedStep, CreatedAtMs: pending.write.AtMs,
			}
			dirty = true
		} else {
			observed = latestCommittedVersion(versions, e.state.CommitSeq)
		}
	} else {
		observed = latestCommittedVersion(versions, snapshotSeq)
	}

	observation := &ReadObservation{
		TxID: tx.ID, ItemID: op.ItemID, GlobalStep: result.GlobalStep, ExecutedStep: step,
		AtMs: e.state.ClockMs, Isolation: tx.Isolation, SnapshotSeq: snapshotSeq,
		VersionNumber: observed.Number, Value: observed.Value,
		WriterTx: observed.WriterTx, CommitSeq: observed.CommitSeq, Dirty: dirty,
	}
	result.Read = observation
	result.Outcome = OutcomeExecuted
	readFact := ReadFact{
		TxID: tx.ID, ItemID: op.ItemID, ReaderStep: step, ReaderAtMs: e.state.ClockMs,
		Value:         observed.Value,
		VersionNumber: observed.Number, WriterTx: observed.WriterTx,
		CommitSeq: observed.CommitSeq, SnapshotSeq: snapshotSeq,
		Isolation: tx.Isolation, Dirty: dirty,
	}
	e.state.ReadFacts = append(e.state.ReadFacts, readFact)
}

func (e *engine) performWrite(tx *TxState, op Operation, step int, result *StepResult) {
	record := WriteRecord{
		ItemID: op.ItemID, Value: op.Value, WriterTx: tx.ID,
		GlobalStep: result.GlobalStep, ExecutedStep: step,
		AtMs: e.state.ClockMs, WriteSeq: e.state.NextWriteSeq,
	}
	e.state.NextWriteSeq++
	tx.PendingWrites = append(tx.PendingWrites, record)
	result.Write = &WriteObservation{
		TxID: tx.ID, ItemID: op.ItemID, GlobalStep: result.GlobalStep, ExecutedStep: step,
		AtMs: e.state.ClockMs, Value: op.Value, WriteSeq: record.WriteSeq,
	}
	result.Outcome = OutcomeExecuted
}

func latestCommittedVersion(versions []VersionRecord, maxCommitSeq uint64) VersionRecord {
	for i := len(versions) - 1; i >= 0; i-- {
		if versions[i].Initial || versions[i].CommitSeq <= maxCommitSeq {
			return versions[i]
		}
	}
	return versions[0]
}

func (e *engine) latestPendingWrite(txID, itemID string) (WriteRecord, bool) {
	tx := e.state.Transactions[txID]
	if tx == nil {
		return WriteRecord{}, false
	}
	for i := len(tx.PendingWrites) - 1; i >= 0; i-- {
		if tx.PendingWrites[i].ItemID == itemID {
			return tx.PendingWrites[i], true
		}
	}
	return WriteRecord{}, false
}

type pendingWrite struct {
	txID  string
	write WriteRecord
}

func (e *engine) latestPendingWriteAny(itemID string) (pendingWrite, bool) {
	var best pendingWrite
	found := false
	txIDs := make([]string, 0, len(e.state.Transactions))
	for id := range e.state.Transactions {
		txIDs = append(txIDs, id)
	}
	sort.Strings(txIDs)
	for _, id := range txIDs {
		tx := e.state.Transactions[id]
		if tx == nil || tx.Done {
			continue
		}
		for _, write := range tx.PendingWrites {
			if write.ItemID != itemID {
				continue
			}
			if !found || write.WriteSeq > best.write.WriteSeq {
				best = pendingWrite{txID: id, write: write}
				found = true
			}
		}
	}
	return best, found
}

func (e *engine) tentativeVersionNumber(itemID string) int {
	versions := e.state.Versions[itemID]
	if len(versions) == 0 {
		return 1
	}
	number := versions[len(versions)-1].Number
	txIDs := make([]string, 0, len(e.state.Transactions))
	for id := range e.state.Transactions {
		txIDs = append(txIDs, id)
	}
	sort.Strings(txIDs)
	for _, id := range txIDs {
		tx := e.state.Transactions[id]
		if tx == nil || tx.Done {
			continue
		}
		for _, write := range tx.PendingWrites {
			if write.ItemID == itemID {
				number++
			}
		}
	}
	return number
}

func (e *engine) computeAnomalies() []Anomaly {
	anomalies := make([]Anomaly, 0)
	seen := make(map[string]struct{})
	add := func(anomaly Anomaly) {
		sort.Strings(anomaly.TxIDs)
		sort.Ints(anomaly.Steps)
		keyData, _ := json.Marshal(anomaly)
		key := string(keyData)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		anomalies = append(anomalies, anomaly)
	}

	for _, read := range e.state.ReadFacts {
		if read.Dirty {
			add(Anomaly{
				Kind: "dirty_read", TxIDs: []string{read.TxID, read.WriterTx},
				ItemID: read.ItemID, Steps: []int{read.ReaderStep},
				Detail: fmt.Sprintf("%s read uncommitted version of %s from %s",
					read.TxID, read.ItemID, read.WriterTx),
			})
		}
	}

	type readKey struct{ txID, itemID string }
	history := make(map[readKey][]ReadFact)
	for _, read := range e.state.ReadFacts {
		key := readKey{txID: read.TxID, itemID: read.ItemID}
		history[key] = append(history[key], read)
	}
	for key, reads := range history {
		if len(reads) < 2 {
			continue
		}
		first := reads[0]
		different := false
		steps := []int{first.ReaderStep}
		for _, read := range reads[1:] {
			if read.VersionNumber != first.VersionNumber || read.Value != first.Value {
				different = true
			}
			if read.VersionNumber != first.VersionNumber {
				steps = append(steps, read.ReaderStep)
			}
		}
		if different {
			add(Anomaly{
				Kind: "non_repeatable_read", TxIDs: []string{key.txID},
				ItemID: key.itemID, Steps: steps,
				Detail: fmt.Sprintf("%s observed changing versions of %s", key.txID, key.itemID),
			})
		}
	}

	for _, cycle := range e.serializationCycles() {
		add(Anomaly{
			Kind: "nonserializable", TxIDs: cycle,
			Detail: "commit/read/write dependency cycle: " + joinSorted(cycle),
		})
	}
	sort.Slice(anomalies, func(i, j int) bool {
		if anomalies[i].Kind != anomalies[j].Kind {
			return anomalies[i].Kind < anomalies[j].Kind
		}
		if anomalies[i].ItemID != anomalies[j].ItemID {
			return anomalies[i].ItemID < anomalies[j].ItemID
		}
		return joinSorted(anomalies[i].TxIDs) < joinSorted(anomalies[j].TxIDs)
	})
	return anomalies
}

func (e *engine) serializationCycles() [][]string {
	committed := make(map[string]uint64)
	for _, commit := range e.state.Commits {
		committed[commit.TxID] = commit.CommitSeq
	}
	if len(committed) < 2 {
		return nil
	}
	edges := make(map[string]map[string]struct{})
	addEdge := func(from, to string) {
		if from == "" || to == "" || from == to {
			return
		}
		if _, ok := committed[from]; !ok {
			return
		}
		if _, ok := committed[to]; !ok {
			return
		}
		if edges[from] == nil {
			edges[from] = make(map[string]struct{})
		}
		edges[from][to] = struct{}{}
	}

	for _, read := range e.state.ReadFacts {
		addEdge(read.WriterTx, read.TxID)
	}

	writesByItem := make(map[string][]WriteRecord)
	for _, write := range e.state.WriteFacts {
		writesByItem[write.ItemID] = append(writesByItem[write.ItemID], write)
	}
	for _, writes := range writesByItem {
		sort.SliceStable(writes, func(i, j int) bool {
			left, right := committed[writes[i].WriterTx], committed[writes[j].WriterTx]
			if left != right {
				return left < right
			}
			return writes[i].WriteSeq < writes[j].WriteSeq
		})
		for i := 1; i < len(writes); i++ {
			addEdge(writes[i-1].WriterTx, writes[i].WriterTx)
		}
	}

	for _, read := range e.state.ReadFacts {
		for _, writes := range writesByItem {
			for _, write := range writes {
				if write.ItemID != read.ItemID || write.WriterTx == read.TxID {
					continue
				}
				if read.CommitSeq < committed[write.WriterTx] &&
					read.ReaderStep < write.ExecutedStep {
					addEdge(read.TxID, write.WriterTx)
				}
			}
		}
	}

	nodes := make([]string, 0, len(committed))
	for id := range committed {
		nodes = append(nodes, id)
	}
	sort.Strings(nodes)
	index := 0
	stack := make([]string, 0)
	onStack := make(map[string]bool)
	indices := make(map[string]int)
	lowlink := make(map[string]int)
	for _, node := range nodes {
		indices[node] = -1
	}
	var components [][]string
	var strongConnect func(string)
	strongConnect = func(node string) {
		indices[node] = index
		lowlink[node] = index
		index++
		stack = append(stack, node)
		onStack[node] = true
		nextNodes := make([]string, 0, len(edges[node]))
		for next := range edges[node] {
			nextNodes = append(nextNodes, next)
		}
		sort.Strings(nextNodes)
		for _, next := range nextNodes {
			if indices[next] == -1 {
				strongConnect(next)
				if lowlink[next] < lowlink[node] {
					lowlink[node] = lowlink[next]
				}
			} else if onStack[next] && indices[next] < lowlink[node] {
				lowlink[node] = indices[next]
			}
		}
		if lowlink[node] == indices[node] {
			var component []string
			for {
				last := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[last] = false
				component = append(component, last)
				if last == node {
					break
				}
			}
			if len(component) > 1 {
				sort.Strings(component)
				components = append(components, component)
			}
		}
	}
	for _, node := range nodes {
		if indices[node] == -1 {
			strongConnect(node)
		}
	}
	sort.Slice(components, func(i, j int) bool {
		return joinSorted(components[i]) < joinSorted(components[j])
	})
	return components
}
