package forward

import (
	"sort"
)

type TunnelStatus struct {
	TunnelID         string   `json:"tunnel_id"`
	PeerID           string   `json:"peer_id"`
	State            string   `json:"state"`
	EffectiveAtMs    int64    `json:"effective_at_ms"`
	SourceEventID    string   `json:"source_event_id,omitempty"`
	LastSeq          int      `json:"last_seq"`
	WatermarkEnabled bool     `json:"watermark_enabled"`
	WatermarkBytes   *int64   `json:"watermark_bytes,omitempty"`
	Degradations     []string `json:"degradations,omitempty"`
}

type Transition struct {
	Seq              int       `json:"seq"`
	EventID          string    `json:"event_id"`
	AtMs             int64     `json:"at_ms"`
	EffectiveAtMs    int64     `json:"effective_at_ms"`
	TunnelID         string    `json:"tunnel_id"`
	PeerID           string    `json:"peer_id"`
	FromState        string    `json:"from_state"`
	ToState          string    `json:"to_state"`
	Changed          bool      `json:"changed"`
	ReasonCode       string    `json:"reason_code"`
	SourceEventID    string    `json:"source_event_id"`
	WatermarkEnabled bool      `json:"watermark_enabled"`
	Degradations     []string  `json:"degradations,omitempty"`
	Error            *BizError `json:"error,omitempty"`
}

type PacketEvaluation struct {
	EventSeq     int          `json:"event_seq"`
	EventID      string       `json:"event_id"`
	Packet       PacketSpec   `json:"packet"`
	Verdict      Verdict      `json:"verdict"`
	Plan         FragmentPlan `json:"fragment"`
	TunnelState  string       `json:"tunnel_state,omitempty"`
	Forwardable  bool         `json:"forwardable"`
	Dependencies []string     `json:"dependencies"`
}

type EventReport struct {
	Seq              int                `json:"seq"`
	EventID          string             `json:"event_id"`
	Kind             string             `json:"kind"`
	AtMs             int64              `json:"at_ms"`
	NormalizedTimeMs int64              `json:"normalized_time_ms"`
	TunnelID         string             `json:"tunnel_id,omitempty"`
	ConfigRevision   string             `json:"config_revision,omitempty"`
	Transitions      []Transition       `json:"transitions,omitempty"`
	Packets          []PacketEvaluation `json:"packets,omitempty"`
	States           []TunnelStatus     `json:"states"`
	Error            *BizError          `json:"error,omitempty"`
}

type RejectRecord struct {
	PacketID string `json:"packet_id"`
	EventID  string `json:"event_id"`
	Code     string `json:"code"`
	Reason   string `json:"reason"`
	TunnelID string `json:"tunnel_id,omitempty"`
}

type Report struct {
	ProjectID     string         `json:"project_id"`
	SchemaVersion string         `json:"schema_version"`
	ContentHash   string         `json:"content_hash"`
	EventCount    int            `json:"event_count"`
	Events        []EventReport  `json:"events"`
	FinalStates   []TunnelStatus `json:"final_states"`
	RejectSet     []RejectRecord `json:"reject_set"`
	Budget        *Budget        `json:"budget,omitempty"`
	WatermarkNote string         `json:"watermark_note,omitempty"`
}

type Engine struct {
	project *Project
	states  map[string]*TunnelStatus
}

func NewEngine(p *Project) (*Engine, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	e := &Engine{project: p, states: map[string]*TunnelStatus{}}
	for _, t := range p.Tunnels {
		status := &TunnelStatus{
			TunnelID:         t.ID,
			PeerID:           t.PeerID,
			State:            StateIdle,
			WatermarkEnabled: t.WatermarkEnabled && !p.WatermarkDegraded,
			WatermarkBytes:   t.KeepaliveWatermarkBytes,
		}
		if p.WatermarkDegraded {
			status.WatermarkEnabled = false
			status.WatermarkBytes = nil
			status.Degradations = []string{"W_WATERMARK_SCHEMA_FIELD_ABSENT:degrade_to_disabled"}
		} else if !t.WatermarkEnabled {
			status.Degradations = []string{"W_WATERMARK_FIELD_ABSENT:disabled_for_tunnel"}
		}
		e.states[t.ID] = status
	}
	return e, nil
}

func (e *Engine) RestoreStates(states []TunnelStatus) {
	for _, st := range states {
		if current, ok := e.states[st.TunnelID]; ok {
			*current = st
		}
	}
}

func RunReport(p *Project, events []Event) (*Report, error) {
	engine, err := NewEngine(p)
	if err != nil {
		return nil, err
	}
	report := &Report{
		ProjectID:     p.ID,
		SchemaVersion: p.SourceSchema,
		ContentHash:   p.ContentHash,
		Events:        make([]EventReport, 0, len(events)),
	}
	if p.WatermarkDegraded {
		report.WatermarkNote = "schema v1 has no keepalive watermark field; declared degradation disables watermark processing"
	}
	for _, ev := range events {
		er := engine.Handle(ev)
		report.Events = append(report.Events, er)
		if er.Error != nil && er.Error.Code == CodeEventUnknown {
			return report, er.Error
		}
	}
	report.EventCount = len(report.Events)
	FillEventStateSnapshots(report, p)
	report.FinalStates = engine.SnapshotStates()
	report.RejectSet = collectRejects(report.Events)
	return report, nil
}

func (e *Engine) SnapshotStates() []TunnelStatus {
	out := make([]TunnelStatus, 0, len(e.project.Tunnels))
	for _, t := range e.project.Tunnels {
		if st, ok := e.states[t.ID]; ok {
			cp := *st
			if st.Degradations != nil {
				cp.Degradations = append([]string(nil), st.Degradations...)
			}
			out = append(out, cp)
		}
	}
	return out
}

// FillEventStateSnapshots reconstructs deterministic all-tunnel snapshots after each event.
// It is used after parallel result merge, so it never introduces scheduler-dependent reads.
func FillEventStateSnapshots(report *Report, p *Project) {
	if report == nil {
		return
	}
	engine, err := NewEngine(p)
	if err != nil {
		return
	}
	for i := range report.Events {
		for _, tr := range report.Events[i].Transitions {
			st, ok := engine.states[tr.TunnelID]
			if !ok || !tr.Changed {
				continue
			}
			st.State = tr.ToState
			st.EffectiveAtMs = tr.EffectiveAtMs
			st.SourceEventID = tr.SourceEventID
			st.LastSeq = tr.Seq
		}
		report.Events[i].States = engine.SnapshotStates()
	}
}

func (e *Engine) Handle(ev Event) EventReport {
	seq := ev.Seq
	if seq == 0 {
		seq = len(e.project.Tunnels) // not meaningful; callers should assign seq
	}
	result := EventReport{
		Seq: seq, EventID: ev.ID, Kind: ev.Kind, AtMs: ev.AtMs, NormalizedTimeMs: ev.AtMs,
		TunnelID: ev.TunnelID, ConfigRevision: ev.ConfigRevision,
		Transitions: []Transition{}, Packets: []PacketEvaluation{},
	}

	switch ev.Kind {
	case "connect", "handshake_complete", "disconnect", "keepalive_timeout":
		tr := e.transition(ev)
		result.Transitions = append(result.Transitions, tr)
		if tr.Error != nil {
			result.Error = tr.Error
		}
	case "config_apply":
		// Configuration revision is carried in the event. Structural edits are handled by incremental service.
	case "packet":
		if ev.Packet != nil {
			pe := e.evaluatePacket(seq, ev.ID, *ev.Packet)
			result.Packets = append(result.Packets, pe)
		}
	default:
		result.Error = NewBizError(CodeEventUnknown, "events.kind", "unknown event kind %q", ev.Kind).
			WithContext("event_id", ev.ID)
	}
	return result
}

func (e *Engine) transition(ev Event) Transition {
	t, _, ok := e.project.Tunnel(ev.TunnelID)
	tr := Transition{
		Seq: ev.Seq, EventID: ev.ID, AtMs: ev.AtMs, EffectiveAtMs: ev.AtMs,
		TunnelID: ev.TunnelID, SourceEventID: ev.ID, ReasonCode: "no_change",
	}
	if !ok {
		tr.Error = NewBizError(CodeTunnelNotFound, "events.tunnel_id", "event %s references missing tunnel %q", ev.ID, ev.TunnelID)
		tr.ReasonCode = CodeTunnelNotFound
		return tr
	}
	st := e.states[ev.TunnelID]
	tr.PeerID = t.PeerID
	tr.FromState = st.State
	tr.WatermarkEnabled = st.WatermarkEnabled
	tr.Degradations = append([]string(nil), st.Degradations...)

	next, reason, bizErr := NextTunnelState(st.State, ev.Kind)
	if bizErr != nil {
		tr.ToState = st.State
		tr.ReasonCode = bizErr.Code
		tr.Error = bizErr.WithContext("event_id", ev.ID)
		return tr
	}
	tr.ToState = next
	tr.ReasonCode = reason
	if next != st.State {
		st.State = next
		st.EffectiveAtMs = ev.AtMs
		st.SourceEventID = ev.ID
		st.LastSeq = ev.Seq
		tr.Changed = true
	}
	return tr
}

func NextTunnelState(current, kind string) (next string, reason string, err *BizError) {
	switch kind {
	case "connect":
		switch current {
		case StateIdle, StateDisconnected:
			return StateHandshaking, "connect_start_handshake", nil
		case StateHandshaking:
			return StateHandshaking, "duplicate_connect_ignored", nil
		case StateEstablished:
			return StateEstablished, "duplicate_connect_ignored", nil
		}
	case "handshake_complete":
		switch current {
		case StateHandshaking:
			return StateEstablished, "handshake_completed", nil
		case StateEstablished:
			return StateEstablished, "duplicate_handshake_ignored", nil
		case StateIdle:
			return current, "handshake_without_connect", NewBizError(CodeStateSkip, "events.kind",
				"cannot move %s directly to established; connect/handshaking intermediate is required", current)
		case StateDisconnected:
			return current, "handshake_after_disconnect", NewBizError(CodeStateSkip, "events.kind",
				"cannot complete handshake from disconnected; a new connect must enter handshaking first")
		}
	case "disconnect":
		switch current {
		case StateHandshaking, StateEstablished:
			return StateDisconnected, "explicit_disconnect", nil
		default:
			return current, "disconnect_ignored_not_active", nil
		}
	case "keepalive_timeout":
		switch current {
		case StateEstablished, StateHandshaking:
			return StateDisconnected, "keepalive_timeout_expired", nil
		default:
			return current, "keepalive_timeout_ignored_not_active", nil
		}
	}
	return current, "no_change", nil
}

func (e *Engine) evaluatePacket(seq int, eventID string, pkt PacketSpec) PacketEvaluation {
	pe := evaluatePacketPure(e.project, seq, eventID, pkt)
	if pe.Plan.TunnelID != "" {
		if st := e.states[pe.Plan.TunnelID]; st != nil {
			pe.TunnelState = st.State
			pe.Forwardable = st.State == StateEstablished
		}
	}
	return pe
}

func evaluatePacketPure(p *Project, seq int, eventID string, pkt PacketSpec) PacketEvaluation {
	verdict := EvaluateVerdict(p, pkt)
	pe := PacketEvaluation{
		EventSeq: seq, EventID: eventID, Packet: pkt, Verdict: verdict,
		Dependencies: append([]string(nil), verdict.Dependencies...),
	}
	if verdict.Action.Type != ActionTunnel {
		pe.Forwardable = false
		pe.TunnelState = ""
		pe.Plan = FragmentPlan{
			PacketID:     pkt.ID,
			RejectCode:   "E_ACTION_NOT_TUNNELED",
			RejectReason: "verdict action " + verdict.Action.Type + " does not select a tunnel fragment plan",
		}
		pe.Dependencies = append(pe.Dependencies, "action:"+verdict.Action.Type)
		return pe
	}
	t, _, ok := p.Tunnel(verdict.Action.TunnelID)
	if !ok {
		biz := NewBizError(CodeTunnelNotFound, "verdict.action.tunnel_id", "selected tunnel %q missing", verdict.Action.TunnelID)
		pe.Plan = FragmentPlan{PacketID: pkt.ID, RejectCode: biz.Code, RejectReason: biz.Message}
		return pe
	}
	pe.Plan = PlanFragmentation(FragmentRequest{
		PacketID:      pkt.ID,
		TunnelID:      t.ID,
		PeerID:        t.PeerID,
		PayloadBytes:  pkt.PayloadBytes,
		MTUBytes:      t.MTUBytes,
		PathMTUBytes:  t.PathMTUBytes,
		OuterProtocol: t.OuterProtocol,
		CipherSuite:   t.CipherSuite,
		IntegerBits:   p.IntegerBits,
		MaxFragments:  p.MaxFragments,
	})
	pe.Dependencies = append(pe.Dependencies,
		"verdict:"+pkt.ID,
		"tunnel:"+t.ID+".mtu",
		"tunnel:"+t.ID+".protocol",
		"tunnel:"+t.ID+".cipher",
		"tunnel:"+t.ID+".path_mtu",
		"tunnel:"+t.ID+".peer_id",
		"project:max_fragments",
		"project:integer_bits",
	)
	return pe
}

func packetDependencies(verdict Verdict, plan FragmentPlan) []string {
	deps := append([]string(nil), verdict.Dependencies...)
	deps = append(deps, "verdict:"+verdict.PacketID)
	if plan.TunnelID != "" {
		deps = append(deps,
			"tunnel:"+plan.TunnelID+".mtu",
			"tunnel:"+plan.TunnelID+".protocol",
			"tunnel:"+plan.TunnelID+".cipher",
			"tunnel:"+plan.TunnelID+".path_mtu",
			"tunnel:"+plan.TunnelID+".peer_id",
		)
	}
	deps = append(deps, "project:max_fragments", "project:integer_bits")
	return deps
}

func collectRejects(events []EventReport) []RejectRecord {
	out := []RejectRecord{}
	for _, ev := range events {
		for _, pe := range ev.Packets {
			if pe.Plan.RejectCode != "" && pe.Plan.RejectCode != "E_ACTION_NOT_TUNNELED" {
				out = append(out, RejectRecord{
					PacketID: pe.Packet.ID, EventID: ev.EventID,
					Code: pe.Plan.RejectCode, Reason: pe.Plan.RejectReason, TunnelID: pe.Plan.TunnelID,
				})
			}
		}
		if ev.Error != nil {
			out = append(out, RejectRecord{EventID: ev.EventID, Code: ev.Error.Code, Reason: ev.Error.Message})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PacketID != out[j].PacketID {
			return out[i].PacketID < out[j].PacketID
		}
		return out[i].EventID < out[j].EventID
	})
	return out
}

func CollectRejectsPublic(events []EventReport) []RejectRecord {
	return collectRejects(events)
}

// CanonicalReportJSON is used by concurrency and incremental equivalence checks.
func CanonicalReportJSON(r *Report) ([]byte, error) {
	return canonicalJSON(r)
}
