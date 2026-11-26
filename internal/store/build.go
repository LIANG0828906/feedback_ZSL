package store

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"meshpolicy/internal/engine"
	"meshpolicy/internal/model"
)

func CanonicalVersion(p *model.Project) string {
	clone := model.CloneProject(p)
	clone.SchemaSource = ""
	sort.SliceStable(clone.Policies, func(i, j int) bool { return clone.Policies[i].ID < clone.Policies[j].ID })
	sort.SliceStable(clone.Workloads, func(i, j int) bool { return clone.Workloads[i].ID < clone.Workloads[j].ID })
	sort.SliceStable(clone.Timeline, func(i, j int) bool {
		si, oi := clone.Timeline[i].SortKey()
		sj, oj := clone.Timeline[j].SortKey()
		if si != sj {
			return si < sj
		}
		return oi < oj
	})
	b, err := model.JSONMarshal(clone)
	if err != nil {
		return "pv-error"
	}
	sum := sha256.Sum256(b)
	return "pv-" + hex.EncodeToString(sum[:])[:16]
}

func BuildCheckpoint(p *model.Project, report *engine.RunReport, atSeq int64) Checkpoint {
	version := CanonicalVersion(p)
	if atSeq <= 0 {
		for _, d := range report.Decisions {
			if d.Seq > atSeq {
				atSeq = d.Seq
			}
		}
	}

	byWorkload := map[string]map[string]engine.Decision{}
	appliedSeq := map[string]int64{}
	for _, w := range p.Workloads {
		byWorkload[w.ID] = map[string]engine.Decision{}
	}
	for _, d := range report.Decisions {
		if d.Seq > atSeq || d.Stale || d.Gate.State == "denied_no_baseline" {
			continue
		}
		target := d.TargetWorkloadID
		if target == "" {
			continue
		}
		if byWorkload[target] == nil {
			byWorkload[target] = map[string]engine.Decision{}
		}
		byWorkload[target][d.FlowKey] = d
		if d.Seq > appliedSeq[target] {
			appliedSeq[target] = d.Seq
		}
	}

	workloadStates := []WorkloadState{}
	for _, w := range p.Workloads {
		decisions := byWorkload[w.ID]
		if decisions == nil {
			decisions = map[string]engine.Decision{}
		}
		raw, _ := model.JSONMarshal(decisions)
		workloadStates = append(workloadStates, WorkloadState{
			WorkloadID: w.ID, Namespace: w.Namespace, AppliedSeq: appliedSeq[w.ID],
			DecisionsJSON: string(raw),
		})
	}

	queue := []QueueItem{}
	if report.Propagation != nil {
		for _, layer := range report.Propagation.Layers {
			if layer.Generation == 0 || layer.ArrivalSeq <= atSeq {
				continue
			}
			payload, _ := model.JSONMarshal(layer)
			queue = append(queue, QueueItem{
				MarkerID: layer.MarkerID, ChangeID: layer.ChangeID, SourcePolicyID: layer.SourcePolicyID,
				Generation: layer.Generation, Status: "pending_propagation", PayloadJSON: string(payload),
			})
		}
	}

	return Checkpoint{
		ProjectVersion: version, CommittedSeq: atSeq, Workloads: workloadStates, Queue: queue,
	}
}
