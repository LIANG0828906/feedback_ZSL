package forward

import (
	"encoding/json"
	"fmt"
	"sort"
)

type EditRequest struct {
	Schema           string `json:"schema,omitempty"`
	EditID           string `json:"edit_id,omitempty"`
	Kind             string `json:"kind"`
	TunnelID         string `json:"tunnel_id,omitempty"`
	RuleID           string `json:"rule_id,omitempty"`
	Value            any    `json:"value"`
	EffectiveFromSeq int    `json:"effective_from_seq,omitempty"`
}

type ChangeSet struct {
	EditID           string   `json:"edit_id"`
	Kind             string   `json:"kind"`
	Path             string   `json:"path"`
	Description      string   `json:"description"`
	ChangedNodes     []string `json:"changed_nodes"`
	EffectiveFromSeq int      `json:"effective_from_seq"`
	Closure          []string `json:"closure"`
	InvalidatedKeys  []string `json:"invalidated_keys"`
	RebuiltSeqs      []int    `json:"rebuilt_seqs"`
	ReusedSeqs       []int    `json:"reused_seqs"`
	RebuiltIntervals []string `json:"rebuilt_intervals"`
}

type EntryTrace struct {
	Key                 string   `json:"key"`
	Kind                string   `json:"kind"`
	Seq                 int      `json:"seq"`
	Status              string   `json:"status"`
	Dependencies        []string `json:"dependencies,omitempty"`
	InvalidationSources []string `json:"invalidation_sources,omitempty"`
	ResultNode          string   `json:"result_node"`
}

type ResultDiff struct {
	Key     string `json:"key"`
	Seq     int    `json:"seq"`
	Before  any    `json:"before,omitempty"`
	After   any    `json:"after,omitempty"`
	Changed bool   `json:"changed"`
}

type IncrementalResult struct {
	ChangeSet
	Traces      []EntryTrace `json:"traces"`
	Diffs       []ResultDiff `json:"diffs"`
	Report      *Report      `json:"report"`
	FullReport  *Report      `json:"full_report"`
	Consistent  bool         `json:"consistent"`
	RebuiltKeys []string     `json:"rebuilt_keys"`
	ReusedKeys  []string     `json:"reused_keys"`
}

type CacheEntry struct {
	Key    string
	Kind   string
	Seq    int
	Deps   []string
	Result any
}

type IncrementalService struct {
	original *Project
	current  *Project
	events   []Event
	entries  map[string]CacheEntry
	order    []string
	editSeq  int
}

func NewIncrementalService(p *Project, events []Event) *IncrementalService {
	return &IncrementalService{
		original: p.Clone(),
		current:  p.Clone(),
		events:   events,
		entries:  map[string]CacheEntry{},
		editSeq:  0,
	}
}

func (s *IncrementalService) CurrentProject() *Project { return s.current.Clone() }

func (s *IncrementalService) Build() (*Report, error) {
	report, err := RunReport(s.current, s.events)
	if err != nil {
		return nil, err
	}
	s.entries = map[string]CacheEntry{}
	s.order = nil
	for _, ev := range report.Events {
		for _, tr := range ev.Transitions {
			key := "state:" + tr.EventID
			s.put(CacheEntry{
				Key: key, Kind: "state", Seq: tr.Seq, Result: tr,
				Deps: []string{
					"tunnel:" + tr.TunnelID + ".peer_id",
					"tunnel:" + tr.TunnelID + ".keepalive_ms",
					"tunnel:" + tr.TunnelID + ".watermark",
				},
			})
		}
		for _, pe := range ev.Packets {
			vKey := "verdict:" + pe.Packet.ID
			s.put(CacheEntry{Key: vKey, Kind: "verdict", Seq: pe.EventSeq, Result: pe.Verdict, Deps: pe.Verdict.Dependencies})
			fDeps := []string{
				"result:" + vKey,
				"tunnel:" + pe.Plan.TunnelID + ".mtu",
				"tunnel:" + pe.Plan.TunnelID + ".protocol",
				"tunnel:" + pe.Plan.TunnelID + ".cipher",
				"tunnel:" + pe.Plan.TunnelID + ".path_mtu",
				"tunnel:" + pe.Plan.TunnelID + ".peer_id",
				"project:max_fragments",
				"project:integer_bits",
			}
			s.put(CacheEntry{Key: "fragment:" + pe.Packet.ID, Kind: "fragment", Seq: pe.EventSeq, Result: pe.Plan, Deps: fDeps})
		}
	}
	return report, nil
}

func (s *IncrementalService) put(e CacheEntry) {
	if _, exists := s.entries[e.Key]; !exists {
		s.order = append(s.order, e.Key)
	}
	s.entries[e.Key] = e
}

func ApplyEdit(p *Project, req EditRequest) (*ChangeSet, *BizError) {
	if req.EditID == "" {
		req.EditID = "edit-" + req.Kind
	}
	cs := &ChangeSet{EditID: req.EditID, Kind: req.Kind, EffectiveFromSeq: req.EffectiveFromSeq}
	if cs.EffectiveFromSeq <= 0 {
		cs.EffectiveFromSeq = 1
	}
	switch req.Kind {
	case "tunnel_mtu":
		t, _, ok := p.Tunnel(req.TunnelID)
		if !ok {
			return nil, NewBizError(CodeTunnelNotFound, "edit.tunnel_id", "tunnel %q not found", req.TunnelID)
		}
		mtu, biz := editInt(req.Value, "value")
		if biz != nil {
			return nil, biz
		}
		old := t.MTUBytes
		t.MTUBytes = mtu
		cs.Path = fmt.Sprintf("tunnels[%s].mtu_bytes", req.TunnelID)
		cs.Description = fmt.Sprintf("%s MTU changed %d -> %d", t.ID, old, mtu)
		cs.ChangedNodes = []string{"tunnel:" + t.ID + ".mtu"}
		if err := ValidateProject(p); err != nil {
			// Restore source-like working copy semantics by returning typed error; caller cloned first.
			if b, ok := AsBizError(err); ok {
				return nil, b
			}
			return nil, WrapBiz(CodeEditInvalid, cs.Path, err, "invalid MTU edit")
		}
	case "tunnel_cipher":
		t, _, ok := p.Tunnel(req.TunnelID)
		if !ok {
			return nil, NewBizError(CodeTunnelNotFound, "edit.tunnel_id", "tunnel %q not found", req.TunnelID)
		}
		cipher, ok := req.Value.(string)
		if !ok || cipher == "" {
			return nil, NewBizError(CodeFieldType, "edit.value", "cipher value must be a non-empty string")
		}
		t.CipherSuite = cipher
		cs.Path = fmt.Sprintf("tunnels[%s].cipher_suite", t.ID)
		cs.Description = "cipher changed to " + cipher
		cs.ChangedNodes = []string{"tunnel:" + t.ID + ".cipher"}
		if err := ValidateProject(p); err != nil {
			if b, ok := AsBizError(err); ok {
				return nil, b
			}
			return nil, WrapBiz(CodeEditInvalid, cs.Path, err, "invalid cipher edit")
		}
	case "tunnel_peer":
		t, _, ok := p.Tunnel(req.TunnelID)
		if !ok {
			return nil, NewBizError(CodeTunnelNotFound, "edit.tunnel_id", "tunnel %q not found", req.TunnelID)
		}
		peerID, ok := req.Value.(string)
		if !ok || peerID == "" {
			return nil, NewBizError(CodeFieldType, "edit.value", "peer id must be a string")
		}
		if _, exists := p.Peer(peerID); !exists {
			return nil, NewBizError(CodePeerNotFound, "edit.value", "peer %q not found", peerID).WithContext("peer_id", peerID)
		}
		old := t.PeerID
		t.PeerID = peerID
		cs.Path = fmt.Sprintf("tunnels[%s].peer_id", t.ID)
		cs.Description = fmt.Sprintf("peer attribution %s -> %s", old, peerID)
		cs.ChangedNodes = []string{"tunnel:" + t.ID + ".peer_id"}
	case "rule_action_tunnel":
		r, _, ok := p.Rule(req.RuleID)
		if !ok {
			return nil, NewBizError(CodeRuleNotFound, "edit.rule_id", "rule %q not found", req.RuleID)
		}
		tid, ok := req.Value.(string)
		if !ok || tid == "" {
			return nil, NewBizError(CodeFieldType, "edit.value", "tunnel id must be a string")
		}
		if _, _, exists := p.Tunnel(tid); !exists {
			return nil, NewBizError(CodeTunnelNotFound, "edit.value", "tunnel %q not found", tid)
		}
		r.Action = Action{Type: ActionTunnel, TunnelID: tid}
		cs.Path = fmt.Sprintf("rules[%s].action.tunnel_id", r.ID)
		cs.Description = "rule action tunnel changed to " + tid
		cs.ChangedNodes = []string{"rule:" + r.ID}
	case "rule_cidr":
		r, _, ok := p.Rule(req.RuleID)
		if !ok {
			return nil, NewBizError(CodeRuleNotFound, "edit.rule_id", "rule %q not found", req.RuleID)
		}
		cidr, ok := req.Value.(string)
		if !ok {
			return nil, NewBizError(CodeFieldType, "edit.value", "CIDR must be a string")
		}
		if _, err := ParseCIDR(cidr); err != nil {
			return nil, NewBizError(CodeFieldType, "edit.value", "invalid CIDR %q", cidr)
		}
		if len(r.CIDRs) == 0 {
			r.CIDRs = []string{cidr}
		} else {
			r.CIDRs[0] = cidr
		}
		cs.Path = fmt.Sprintf("rules[%s].cidrs[0]", r.ID)
		cs.Description = "rule CIDR changed to " + cidr
		cs.ChangedNodes = []string{"rule:" + r.ID}
	case "rule_priority":
		r, _, ok := p.Rule(req.RuleID)
		if !ok {
			return nil, NewBizError(CodeRuleNotFound, "edit.rule_id", "rule %q not found", req.RuleID)
		}
		priority, biz := editInt(req.Value, "value")
		if biz != nil {
			return nil, biz
		}
		r.Priority = int(priority)
		cs.Path = fmt.Sprintf("rules[%s].priority", r.ID)
		cs.Description = fmt.Sprintf("rule priority changed to %d", priority)
		cs.ChangedNodes = []string{"rule:" + r.ID, "rules-index"}
	default:
		return nil, NewBizError(CodeEditInvalid, "edit.kind", "unsupported edit kind %q", req.Kind)
	}
	p.ContentHash = p.CanonicalHash()
	return cs, nil
}

func editInt(v any, path string) (int64, *BizError) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		return int64(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, NewBizError(CodeFieldType, path, "expected integer: %v", err)
		}
		return i, nil
	default:
		return 0, NewBizError(CodeFieldType, path, "expected integer, got %T", v)
	}
}

func (s *IncrementalService) ApplyAndReplay(req EditRequest) (*IncrementalResult, error) {
	if len(s.entries) == 0 {
		if _, err := s.Build(); err != nil {
			return nil, err
		}
	}
	edited := s.current.Clone()
	change, biz := ApplyEdit(edited, req)
	if biz != nil {
		return nil, biz
	}
	s.current = edited
	s.editSeq++
	if change.EditID == "edit-"+req.Kind && s.editSeq > 1 {
		change.EditID = fmt.Sprintf("%s-%d", change.EditID, s.editSeq)
	}

	invalidated, closure := s.invalidateClosure(change.ChangedNodes, change.EffectiveFromSeq)
	change.Closure = closure
	change.InvalidatedKeys = keysOf(invalidated)

	oldResults := map[string]any{}
	for key := range invalidated {
		if entry, ok := s.entries[key]; ok {
			oldResults[key] = entry.Result
			delete(s.entries, key)
		}
	}

	report, traces, diffs := s.replay(change.EffectiveFromSeq, invalidated, oldResults)
	rebuiltSet := map[int]bool{}
	reusedSet := map[int]bool{}
	for _, trace := range traces {
		if trace.Status == "rebuilt" {
			rebuiltSet[trace.Seq] = true
		} else {
			reusedSet[trace.Seq] = true
		}
	}
	change.RebuiltSeqs = sortedSeqSet(rebuiltSet)
	change.ReusedSeqs = sortedSeqSet(reusedSet)
	change.RebuiltIntervals = seqIntervals(change.RebuiltSeqs)

	full, err := RunTimelineFull(s.original, s.current, s.events, change.EffectiveFromSeq)
	if err != nil {
		return nil, err
	}
	result := &IncrementalResult{
		ChangeSet:   *change,
		Traces:      traces,
		Diffs:       diffs,
		Report:      report,
		FullReport:  full,
		Consistent:  SignaturesEqual(ReportSignature(report), ReportSignature(full)),
		RebuiltKeys: change.InvalidatedKeys,
		ReusedKeys:  reusedKeys(traces),
	}
	return result, nil
}

func (s *IncrementalService) invalidateClosure(changedNodes []string, fromSeq int) (map[string][]string, []string) {
	reverse := map[string][]string{}
	for _, key := range s.order {
		entry, ok := s.entries[key]
		if !ok {
			continue
		}
		for _, dep := range entry.Deps {
			reverse[dep] = append(reverse[dep], key)
		}
	}
	invalidated := map[string][]string{}
	closureSet := map[string]bool{}
	queue := append([]string(nil), changedNodes...)
	visitedNodes := map[string]bool{}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if visitedNodes[node] {
			continue
		}
		visitedNodes[node] = true
		closureSet[node] = true
		for _, key := range reverse[node] {
			entry, ok := s.entries[key]
			if !ok || entry.Seq < fromSeq {
				continue
			}
			if _, exists := invalidated[key]; !exists {
				invalidated[key] = []string{node}
			} else {
				invalidated[key] = append(invalidated[key], node)
			}
			resultNode := "result:" + key
			if !visitedNodes[resultNode] {
				queue = append(queue, resultNode)
			}
		}
	}
	closure := make([]string, 0, len(closureSet))
	for node := range closureSet {
		closure = append(closure, node)
	}
	sort.Strings(closure)
	return invalidated, closure
}

func (s *IncrementalService) replay(fromSeq int, invalidated map[string][]string, oldResults map[string]any) (*Report, []EntryTrace, []ResultDiff) {
	engine, _ := NewEngine(s.current)
	traces := []EntryTrace{}
	diffs := []ResultDiff{}
	events := make([]EventReport, 0, len(s.events))

	for _, ev := range s.events {
		er := EventReport{
			Seq: ev.Seq, EventID: ev.ID, Kind: ev.Kind, AtMs: ev.AtMs, NormalizedTimeMs: ev.AtMs,
			TunnelID: ev.TunnelID, ConfigRevision: ev.ConfigRevision,
			Transitions: []Transition{}, Packets: []PacketEvaluation{},
		}
		switch ev.Kind {
		case "connect", "handshake_complete", "disconnect", "keepalive_timeout":
			key := "state:" + ev.ID
			if entry, ok := s.entries[key]; ok {
				tr := entry.Result.(Transition)
				if st := engine.states[tr.TunnelID]; st != nil {
					st.State = tr.ToState
					st.EffectiveAtMs = tr.EffectiveAtMs
					st.SourceEventID = tr.SourceEventID
					st.LastSeq = tr.Seq
				}
				er.Transitions = append(er.Transitions, tr)
				traces = append(traces, s.hitTrace(entry))
			} else {
				tr := engine.transition(ev)
				er.Transitions = append(er.Transitions, tr)
				deps := []string{"tunnel:" + ev.TunnelID + ".peer_id", "tunnel:" + ev.TunnelID + ".keepalive_ms", "tunnel:" + ev.TunnelID + ".watermark"}
				entry := CacheEntry{Key: key, Kind: "state", Seq: ev.Seq, Result: tr, Deps: deps}
				s.put(entry)
				traces = append(traces, rebuiltTrace(entry, invalidated[key]))
				diffs = appendDiff(diffs, key, ev.Seq, oldResults[key], tr)
			}
		case "packet":
			if ev.Packet == nil {
				break
			}
			pkt := *ev.Packet
			vKey := "verdict:" + pkt.ID
			var verdict Verdict
			if entry, ok := s.entries[vKey]; ok {
				verdict = entry.Result.(Verdict)
				traces = append(traces, s.hitTrace(entry))
			} else {
				verdict = EvaluateVerdict(s.current, pkt)
				entry := CacheEntry{Key: vKey, Kind: "verdict", Seq: ev.Seq, Result: verdict, Deps: verdict.Dependencies}
				s.put(entry)
				traces = append(traces, rebuiltTrace(entry, invalidated[vKey]))
				diffs = appendDiff(diffs, vKey, ev.Seq, oldResults[vKey], verdict)
			}

			fKey := "fragment:" + pkt.ID
			var plan FragmentPlan
			if entry, ok := s.entries[fKey]; ok {
				plan = entry.Result.(FragmentPlan)
				traces = append(traces, s.hitTrace(entry))
			} else {
				pe := engine.evaluatePacket(ev.Seq, ev.ID, pkt)
				pe.Verdict = verdict
				// Re-plan with the replayed verdict in case action changed.
				if verdict.Action.Type == ActionTunnel {
					if t, _, ok := s.current.Tunnel(verdict.Action.TunnelID); ok {
						pe.Plan = PlanFragmentation(FragmentRequest{
							PacketID: pkt.ID, TunnelID: t.ID, PayloadBytes: pkt.PayloadBytes,
							PeerID:   t.PeerID,
							MTUBytes: t.MTUBytes, PathMTUBytes: t.PathMTUBytes,
							OuterProtocol: t.OuterProtocol, CipherSuite: t.CipherSuite,
							IntegerBits: s.current.IntegerBits, MaxFragments: s.current.MaxFragments,
						})
					}
				}
				plan = pe.Plan
				er.Packets = append(er.Packets, pe)
				deps := []string{"result:" + vKey}
				if plan.TunnelID != "" {
					deps = append(deps,
						"tunnel:"+plan.TunnelID+".mtu",
						"tunnel:"+plan.TunnelID+".protocol",
						"tunnel:"+plan.TunnelID+".cipher",
						"tunnel:"+plan.TunnelID+".path_mtu",
						"tunnel:"+plan.TunnelID+".peer_id")
				}
				deps = append(deps, "project:max_fragments", "project:integer_bits")
				entry := CacheEntry{Key: fKey, Kind: "fragment", Seq: ev.Seq, Result: plan, Deps: deps}
				s.put(entry)
				traces = append(traces, rebuiltTrace(entry, invalidated[fKey]))
				diffs = appendDiff(diffs, fKey, ev.Seq, oldResults[fKey], plan)
				break
			}

			pe := PacketEvaluation{EventSeq: ev.Seq, EventID: ev.ID, Packet: pkt, Verdict: verdict, Plan: plan, Dependencies: packetDependencies(verdict, plan)}
			if plan.TunnelID != "" {
				if st := engine.states[plan.TunnelID]; st != nil {
					pe.TunnelState = st.State
					pe.Forwardable = st.State == StateEstablished
				}
			}
			er.Packets = append(er.Packets, pe)
		case "config_apply":
			// Carried in event report.
		default:
			er.Error = NewBizError(CodeEventUnknown, "events.kind", "unknown event kind %q", ev.Kind)
		}
		events = append(events, er)
	}

	report := &Report{
		ProjectID: s.current.ID, SchemaVersion: s.current.SourceSchema, ContentHash: s.current.ContentHash,
		EventCount: len(events), Events: events, FinalStates: engine.SnapshotStates(),
	}
	FillEventStateSnapshots(report, s.current)
	report.RejectSet = collectRejects(events)
	sort.SliceStable(traces, func(i, j int) bool {
		if traces[i].Seq != traces[j].Seq {
			return traces[i].Seq < traces[j].Seq
		}
		return traces[i].Key < traces[j].Key
	})
	return report, traces, diffs
}

func (s *IncrementalService) hitTrace(e CacheEntry) EntryTrace {
	return EntryTrace{Key: e.Key, Kind: e.Kind, Seq: e.Seq, Status: "hit", Dependencies: e.Deps, ResultNode: "result:" + e.Key}
}

func rebuiltTrace(e CacheEntry, sources []string) EntryTrace {
	if len(sources) == 0 {
		sources = []string{"recomputed_dependency_closure"}
	}
	return EntryTrace{Key: e.Key, Kind: e.Kind, Seq: e.Seq, Status: "rebuilt", Dependencies: e.Deps, InvalidationSources: sources, ResultNode: "result:" + e.Key}
}

func appendDiff(diffs []ResultDiff, key string, seq int, before, after any) []ResultDiff {
	d := ResultDiff{Key: key, Seq: seq, Before: before, After: after}
	bb, _ := json.Marshal(before)
	ab, _ := json.Marshal(after)
	d.Changed = string(bb) != string(ab)
	return append(diffs, d)
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSeqSet(set map[int]bool) []int {
	out := []int{}
	for seq := range set {
		out = append(out, seq)
	}
	sort.Ints(out)
	return out
}

func reusedKeys(traces []EntryTrace) []string {
	out := []string{}
	for _, t := range traces {
		if t.Status == "hit" {
			out = append(out, t.Key)
		}
	}
	sort.Strings(out)
	return out
}

func seqIntervals(seqs []int) []string {
	if len(seqs) == 0 {
		return nil
	}
	out := []string{}
	start, prev := seqs[0], seqs[0]
	for _, seq := range seqs[1:] {
		if seq == prev+1 {
			prev = seq
			continue
		}
		out = append(out, intervalLabel(start, prev))
		start, prev = seq, seq
	}
	out = append(out, intervalLabel(start, prev))
	return out
}

func intervalLabel(a, b int) string {
	if a == b {
		return fmt.Sprintf("%d", a)
	}
	return fmt.Sprintf("%d-%d", a, b)
}

// RunTimelineFull performs a from-scratch replay where the edited snapshot becomes effective at seq.
func RunTimelineFull(original, edited *Project, events []Event, effectiveFromSeq int) (*Report, error) {
	if effectiveFromSeq <= 0 {
		effectiveFromSeq = 1
	}
	baseEngine, err := NewEngine(original)
	if err != nil {
		return nil, err
	}
	editEngine, err := NewEngine(edited)
	if err != nil {
		return nil, err
	}
	report := &Report{ProjectID: edited.ID, SchemaVersion: edited.SourceSchema, ContentHash: edited.ContentHash}
	active := baseEngine
	switched := false
	for _, ev := range events {
		if !switched && ev.Seq >= effectiveFromSeq {
			editEngine.states = baseEngine.states
			active = editEngine
			switched = true
		}
		er := active.Handle(ev)
		report.Events = append(report.Events, er)
	}
	if !switched {
		editEngine.states = baseEngine.states
	}
	report.EventCount = len(report.Events)
	FillEventStateSnapshots(report, edited)
	report.FinalStates = active.SnapshotStates()
	report.RejectSet = collectRejects(report.Events)
	return report, nil
}
