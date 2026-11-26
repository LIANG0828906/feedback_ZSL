package schema

import (
	"encoding/json"
	"math/rand"
	"strconv"

	"meshpolicy/internal/model"
)

const (
	V1 = "mesh.policy/v1"
	V2 = "mesh.policy/v2"
	V3 = "mesh.policy/v3"
)

func EncodeV1(p *model.Project, rng *rand.Rand) ([]byte, error) {
	doc := v1Doc{
		Schema: V1, ProjectID: p.ID, Seed: p.Seed, TickMS: p.TickMS,
		DefaultDeny: &p.DefaultDeny, PolicyLimits: p.PolicyLimits,
		Workloads: make([]v1Workload, 0, len(p.Workloads)),
		Policies:  make([]v1Policy, 0, len(p.Policies)),
		Events:    make([]v1Event, 0, len(p.Timeline)),
	}
	for _, w := range p.Workloads {
		doc.Workloads = append(doc.Workloads, v1Workload{
			WorkloadID: w.ID, Namespace: w.Namespace, ServiceAccount: w.ServiceAccount,
			Labels: w.Labels, DependsOn: w.DependsOn, Exited: w.Exited,
		})
	}
	for _, pol := range p.Policies {
		actionCode := v1ActionCode(pol.Action)
		dto := v1Policy{
			PolicyID: pol.ID, Namespace: pol.Namespace,
			SourceSelector: &v1Selector{
				Namespace:      pol.Source.Namespace,
				MatchLabels:    pol.Source.MatchLabels,
				ServiceAccount: pol.Source.ServiceAccounts,
				WorkloadRefs:   pol.Source.WorkloadRefs,
			},
			Action: string(pol.Action), ActionCode: &actionCode,
			Priority: pol.Priority, DeclaredOrder: pol.Order,
			Window: v1Window{StartTick: pol.StartSeq, EndTick: pol.EndSeq},
			State:  string(pol.State), Revision: pol.Revision,
			Replaces: pol.Replaces, ReplacedBy: pol.ReplacedBy,
		}
		if pol.PortFrom == pol.PortTo {
			port := pol.PortFrom
			dto.TargetPort = &port
		} else {
			dto.PortRange = []int{pol.PortFrom, pol.PortTo}
		}
		doc.Policies = append(doc.Policies, dto)
	}
	for _, ev := range p.Timeline {
		dto := v1Event{AtSeq: ev.Seq, Order: ev.Order}
		if ev.Attempt != nil {
			dto.Connection = &v1Attempt{
				AttemptID: ev.Attempt.ID, AtSeq: ev.Attempt.Seq, Order: ev.Attempt.Order,
				SourceWorkloadID: ev.Attempt.SourceWorkloadID, TargetWorkloadID: ev.Attempt.TargetWorkloadID,
				TargetPort: ev.Attempt.Port, Protocol: ev.Attempt.Protocol,
			}
		}
		if ev.Change != nil {
			dto.Change = encodeV1Change(ev.Change)
		}
		doc.Events = append(doc.Events, dto)
	}
	shuffleIf(rng, doc.Policies)
	shuffleIf(rng, doc.Events)
	return json.MarshalIndent(doc, "", "  ")
}

func EncodeV2(p *model.Project, rng *rand.Rand) ([]byte, error) {
	doc := v2Doc{
		APIVersion: V2, ProjectID: p.ID, Seed: p.Seed, TickMS: p.TickMS,
		// V2 intentionally omits defaultDeny; loader applies the version profile (default deny).
		PolicyLimits: p.PolicyLimits,
		Workloads:    make([]v2Workload, 0, len(p.Workloads)),
		Policies:     make([]v2Policy, 0, len(p.Policies)),
		Timeline:     make([]v2Event, 0, len(p.Timeline)),
	}
	for _, w := range p.Workloads {
		doc.Workloads = append(doc.Workloads, v2Workload{
			WorkloadID: w.ID, Namespace: w.Namespace, ServiceAccount: w.ServiceAccount,
			Labels: w.Labels, DependsOn: w.DependsOn, Exited: w.Exited,
		})
	}
	for _, pol := range p.Policies {
		code := v2ActionCode(pol.Action)
		dto := v2Policy{
			PolicyID: pol.ID, Namespace: pol.Namespace,
			SourceSelector: &v2Selector{Kind: "namespace_tree", NamespaceTree: map[string]v2SelectorNode{
				selectorNamespace(pol.Source, pol.Namespace): {
					MatchLabels: pol.Source.MatchLabels, ServiceAccounts: pol.Source.ServiceAccounts,
					WorkloadRefs: pol.Source.WorkloadRefs,
				},
			}},
			PortRange:  &v2PortRange{From: pol.PortFrom, To: pol.PortTo},
			ActionName: v2ActionName(pol.Action), ActionCode: &code,
			Priority: pol.Priority, DeclaredOrder: pol.Order,
			EffectiveWindow: &v2Window{StartMS: pol.StartSeq * p.TickMS, EndMS: pol.EndSeq * p.TickMS},
			State:           string(pol.State), Revision: pol.Revision,
			Replaces: pol.Replaces, ReplacedBy: pol.ReplacedBy,
		}
		doc.Policies = append(doc.Policies, dto)
	}
	for _, ev := range p.Timeline {
		dto := v2Event{AtMS: ev.Seq * p.TickMS, Order: ev.Order}
		if ev.Attempt != nil {
			dto.Connection = &v2Attempt{
				AttemptID: ev.Attempt.ID, AtMS: ev.Attempt.Seq * p.TickMS, Order: ev.Attempt.Order,
				SourceWorkloadID: ev.Attempt.SourceWorkloadID, TargetWorkloadID: ev.Attempt.TargetWorkloadID,
				TargetPort: ev.Attempt.Port, Protocol: ev.Attempt.Protocol,
			}
		}
		if ev.Change != nil {
			dto.Change = encodeV2Change(ev.Change, p.TickMS)
		}
		doc.Timeline = append(doc.Timeline, dto)
	}
	shuffleIf(rng, doc.Policies)
	shuffleIf(rng, doc.Timeline)
	return json.MarshalIndent(doc, "", "  ")
}

func EncodeV3(p *model.Project, rng *rand.Rand) ([]byte, error) {
	doc := v3Doc{
		APIVersion: V3, ProjectName: p.ID, FixedSeed: p.Seed, TickDurationMS: p.TickMS,
		DefaultDeny: &p.DefaultDeny, Quotas: p.PolicyLimits,
		WorkloadSet:   make([]v3Workload, 0, len(p.Workloads)),
		SelectorTable: []v3SelectorEntry{},
		PolicyEntries: make([]v3Policy, 0, len(p.Policies)),
		Moments:       make([]v3Moment, 0, len(p.Timeline)),
	}
	selectorIDs := map[string]string{}
	for i, pol := range p.Policies {
		sid := "sel-" + pol.ID
		selectorIDs[pol.ID] = sid
		doc.SelectorTable = append(doc.SelectorTable, v3SelectorEntry{
			SelectorID: sid, Namespace: pol.Source.Namespace, MatchLabels: pol.Source.MatchLabels,
			ServiceAccounts: pol.Source.ServiceAccounts, WorkloadRefs: pol.Source.WorkloadRefs,
		})
		_ = i
	}
	for _, w := range p.Workloads {
		doc.WorkloadSet = append(doc.WorkloadSet, v3Workload{
			Name: w.ID, Namespace: w.Namespace, ServiceAccountName: w.ServiceAccount,
			LabelSet: w.Labels, DependencyRefs: w.DependsOn, Removed: w.Exited,
		})
	}
	for _, pol := range p.Policies {
		dto := v3Policy{
			EntryID: pol.ID, NamespaceName: pol.Namespace, SelectorRef: selectorIDs[pol.ID],
			PortExpression: encodePortExpression(pol.PortFrom, pol.PortTo),
			Effect:         string(pol.Action), PriorityValue: pol.Priority, DeclarationIndex: pol.Order,
			TickWindow: []int64{pol.StartSeq, pol.EndSeq}, LifecycleState: string(pol.State),
			Revision: pol.Revision, Replaces: pol.Replaces, ReplacedBy: pol.ReplacedBy,
		}
		doc.PolicyEntries = append(doc.PolicyEntries, dto)
	}
	for _, ev := range p.Timeline {
		m := v3Moment{AtTick: ev.Seq, SequenceOrder: ev.Order}
		if ev.Attempt != nil {
			m.Connection = &v3Connection{
				ConnectionID: ev.Attempt.ID, AtTick: ev.Attempt.Seq, SequenceOrder: ev.Attempt.Order,
				SourceRef: ev.Attempt.SourceWorkloadID, TargetRef: ev.Attempt.TargetWorkloadID,
				ServicePort: ev.Attempt.Port, ProtocolName: ev.Attempt.Protocol,
			}
		}
		if ev.Change != nil {
			m.Edit = encodeV3Change(ev.Change)
		}
		doc.Moments = append(doc.Moments, m)
	}
	shuffleIf(rng, doc.PolicyEntries)
	shuffleIf(rng, doc.Moments)
	return json.MarshalIndent(doc, "", "  ")
}

func shuffleIf[T any](rng *rand.Rand, items []T) {
	if rng == nil {
		return
	}
	rng.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
}

func selectorNamespace(sel model.Selector, fallback string) string {
	if sel.Namespace != "" {
		return sel.Namespace
	}
	return fallback
}

func v1ActionCode(a model.Action) int {
	if a == model.ActionDeny {
		return 1
	}
	return 0
}

func v2ActionCode(a model.Action) int {
	if a == model.ActionDeny {
		return 0
	}
	return 1
}

func v2ActionName(a model.Action) string {
	if a == model.ActionDeny {
		return "DENY"
	}
	return "ALLOW"
}

func encodePortExpression(from, to int) string {
	if from == to {
		return strconv.Itoa(from)
	}
	return strconv.Itoa(from) + "-" + strconv.Itoa(to)
}
