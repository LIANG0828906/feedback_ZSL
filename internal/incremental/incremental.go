package incremental

import (
	"encoding/json"
	"sort"

	"tlscheck/internal/bizerr"
	"tlscheck/internal/engine"
	"tlscheck/internal/model"
)

type Change struct {
	Source      string
	AtMs        int64
	EndpointIDs []string
}

type Manager struct {
	project   model.Project
	report    *engine.Report
	snapshots map[string]map[int64]*engine.EndpointState
}

func New(project model.Project) (*Manager, error) {
	m := &Manager{
		project:   model.CloneProject(project),
		snapshots: map[string]map[int64]*engine.EndpointState{},
	}
	report, err := engine.Run(m.project, engine.Options{ObserveState: m.observe})
	if err != nil {
		return nil, err
	}
	m.report = report
	return m, nil
}

func (m *Manager) Project() model.Project { return model.CloneProject(m.project) }
func (m *Manager) Report() *engine.Report { return m.report }

func (m *Manager) observe(endpointID string, t int64, st *engine.EndpointState) {
	cp, err := CloneState(st)
	if err != nil {
		return
	}
	if m.snapshots[endpointID] == nil {
		m.snapshots[endpointID] = map[int64]*engine.EndpointState{}
	}
	m.snapshots[endpointID][t] = cp
}

func (m *Manager) EditCertificateValidity(chainID string, level int, notBeforeMs, notAfterMs, atMs int64) (Change, error) {
	ch, ok := m.project.Chain(chainID)
	if !ok {
		return Change{}, bizerr.Newf(bizerr.ENotFound, "chain %q not found", chainID)
	}
	if level < 0 || level >= len(ch.Certificates) {
		return Change{}, bizerr.Newf(bizerr.EInvalidArgument, "level %d out of range for chain %q", level, chainID)
	}
	for i := range m.project.Chains {
		if m.project.Chains[i].ID == chainID {
			m.project.Chains[i].Certificates[level].NotBeforeMs = notBeforeMs
			m.project.Chains[i].Certificates[level].NotAfterMs = notAfterMs
		}
	}
	return Change{
		Source: "SRC_CHAIN_VALIDITY:" + chainID + "#" + itoa(level), AtMs: atMs,
		EndpointIDs: m.endpointsReferencing(chainID),
	}, nil
}

func (m *Manager) EditIssuer(chainID string, level int, issuer string, atMs int64) (Change, error) {
	ch, ok := m.project.Chain(chainID)
	if !ok {
		return Change{}, bizerr.Newf(bizerr.ENotFound, "chain %q not found", chainID)
	}
	if level < 0 || level >= len(ch.Certificates) {
		return Change{}, bizerr.Newf(bizerr.EInvalidArgument, "level %d out of range", level)
	}
	for i := range m.project.Chains {
		if m.project.Chains[i].ID == chainID {
			m.project.Chains[i].Certificates[level].Issuer = issuer
		}
	}
	return Change{
		Source: "SRC_ISSUER_RELATION:" + chainID + "#" + itoa(level), AtMs: atMs,
		EndpointIDs: m.endpointsReferencing(chainID),
	}, nil
}

func (m *Manager) EditProbePeriod(endpointID string, periodMs, atMs int64) (Change, error) {
	if periodMs <= 0 {
		return Change{}, bizerr.New(bizerr.EInvalidArgument, "period must be positive")
	}
	_, ok := m.project.Endpoint(endpointID)
	if !ok {
		return Change{}, bizerr.Newf(bizerr.ENotFound, "endpoint %q not found", endpointID)
	}
	for i := range m.project.Endpoints {
		if m.project.Endpoints[i].ID == endpointID {
			m.project.Endpoints[i].PeriodMs = periodMs
		}
	}
	return Change{Source: "SRC_PROBE_PERIOD:" + endpointID, AtMs: atMs, EndpointIDs: []string{endpointID}}, nil
}

func (m *Manager) AppendRequest(atMs int64, req model.Request) (Change, error) {
	if req.ID == "" || req.EndpointID == "" {
		return Change{}, bizerr.New(bizerr.EInvalidArgument, "request id and endpoint are required")
	}
	if _, ok := m.project.Endpoint(req.EndpointID); !ok {
		return Change{}, bizerr.Newf(bizerr.ENotFound, "endpoint %q not found", req.EndpointID)
	}
	maxSeq := 0
	for _, ev := range m.project.Events {
		if ev.Sequence > maxSeq {
			maxSeq = ev.Sequence
		}
	}
	m.project.Events = append(m.project.Events, model.Event{
		Sequence: maxSeq + 1, Kind: model.KindHandshake, AtMs: atMs, Request: &req,
	})
	return Change{Source: "SRC_REQUEST_APPEND:" + req.ID, AtMs: atMs, EndpointIDs: []string{req.EndpointID}}, nil
}

func (m *Manager) DeleteRequest(requestID string, atMs int64) (Change, error) {
	found := -1
	var endpointID string
	var arrival int64
	for i, ev := range m.project.Events {
		if ev.Request != nil && ev.Request.ID == requestID {
			found = i
			endpointID = ev.Request.EndpointID
			arrival = ev.AtMs
			break
		}
	}
	if found < 0 {
		return Change{}, bizerr.Newf(bizerr.ENotFound, "request %q not found", requestID)
	}
	m.project.Events = append(m.project.Events[:found], m.project.Events[found+1:]...)
	if atMs == 0 {
		atMs = arrival
	}
	return Change{Source: "SRC_REQUEST_DELETE:" + requestID, AtMs: atMs, EndpointIDs: []string{endpointID}}, nil
}

func (m *Manager) Refresh(changes ...Change) (*engine.Report, error) {
	if len(changes) == 0 {
		return m.report, nil
	}
	byEndpoint := map[string]*Change{}
	for i := range changes {
		ch := changes[i]
		for _, epID := range ch.EndpointIDs {
			existing := byEndpoint[epID]
			if existing == nil {
				cp := ch
				cp.EndpointIDs = nil
				byEndpoint[epID] = &cp
				continue
			}
			if ch.AtMs < existing.AtMs {
				existing.AtMs = ch.AtMs
			}
			existing.Source += "," + ch.Source
		}
	}

	oldResultsByEP := map[string][]*engine.RequestResult{}
	for _, rr := range m.report.Results {
		oldResultsByEP[rr.EndpointID] = append(oldResultsByEP[rr.EndpointID], rr)
	}
	allEndpoints := make([]string, 0, len(m.project.Endpoints))
	for _, ep := range m.project.Endpoints {
		allEndpoints = append(allEndpoints, ep.ID)
	}
	sort.Strings(allEndpoints)

	runs := make([]engine.EndpointRun, 0, len(allEndpoints))
	for _, epID := range allEndpoints {
		change, affected := byEndpoint[epID]
		if !affected {
			old := oldResultsByEP[epID]
			for _, rr := range old {
				rr.Provenance.Reused = true
				rr.Provenance.Recomputed = false
				rr.Provenance.CacheKey = "cache:" + m.report.SnapshotHash[:12] + ":" + rr.RequestID
				if !contains(rr.Provenance.Sources, "CACHE_REUSE") {
					rr.Provenance.Sources = append(rr.Provenance.Sources, "CACHE_REUSE")
				}
			}
			runs = append(runs, engine.EndpointRun{
				ID: epID, Results: old, State: m.report.EndpointStates[epID],
				Moments: oldEndpointMoments(m.report, epID, 1<<62),
			})
			continue
		}
		boundary := m.boundaryState(epID, change.AtMs)
		if boundary != nil {
			boundary.PeriodStartMs = 0
		}
		currentRequests := map[string]bool{}
		for _, ev := range m.project.Events {
			if ev.Request != nil && ev.Request.EndpointID == epID {
				currentRequests[ev.Request.ID] = true
			}
		}
		var kept []*engine.RequestResult
		for _, rr := range oldResultsByEP[epID] {
			if currentRequests[rr.RequestID] {
				kept = append(kept, rr)
			}
		}
		initialResults := map[string][]*engine.RequestResult{
			epID: cloneResults(kept),
		}
		opts := engine.Options{
			InitialStates:  map[string]*engine.EndpointState{},
			InitialResults: initialResults,
			ObserveState:   m.observe,
		}
		if boundary != nil {
			opts.InitialStates[epID] = boundary
		}
		results, state, moments, err := engine.RunEndpoint(m.project, epID, boundary, opts)
		if err != nil {
			return nil, err
		}
		moments = append(oldEndpointMoments(m.report, epID, change.AtMs), moments...)
		for _, rr := range results {
			classifyProvenance(rr, change)
		}
		runs = append(runs, engine.EndpointRun{ID: epID, Results: results, State: state, Moments: moments})
	}
	m.report = engine.MergeRuns(m.project, runs)
	return m.report, nil
}

func oldEndpointMoments(r *engine.Report, endpointID string, beforeMs int64) []engine.MomentReport {
	var out []engine.MomentReport
	for _, m := range r.Moments {
		if m.TimeMs >= beforeMs {
			continue
		}
		if mep, ok := m.Endpoints[endpointID]; ok {
			out = append(out, engine.MomentReport{
				TimeMs: m.TimeMs, Endpoints: map[string]engine.MomentEndpoint{endpointID: mep},
			})
		}
	}
	return out
}

func (m *Manager) boundaryState(epID string, atMs int64) *engine.EndpointState {
	snaps := m.snapshots[epID]
	if len(snaps) == 0 {
		return nil
	}
	best := int64(-1)
	for t := range snaps {
		if t < atMs && t > best {
			best = t
		}
	}
	if best < 0 {
		return nil
	}
	cp, _ := CloneState(snaps[best])
	return cp
}

func classifyProvenance(rr *engine.RequestResult, change *Change) {
	rr.Provenance.CacheKey = ""
	prefixTerminal := rr.ArrivalAtMs < change.AtMs &&
		((rr.Status == engine.StatusCompleted && rr.CompletedAtMs > 0 && rr.CompletedAtMs < change.AtMs) ||
			(rr.Status == engine.StatusRejected && rr.CompletedAtMs > 0 && rr.CompletedAtMs < change.AtMs))
	if prefixTerminal {
		rr.Provenance.Reused = true
		rr.Provenance.Recomputed = false
		rr.Provenance.Sources = []string{"CACHE_REUSE:terminal_before_invalidation"}
		rr.Provenance.CacheKey = "cache:" + rr.RequestID
		return
	}
	rr.Provenance.Reused = false
	rr.Provenance.Recomputed = true
	rr.Provenance.InvalidationSource = splitSources(change.Source)
	rr.Provenance.Sources = append(splitSources(change.Source), "DEPENDENCY_INTERVAL_RECOMPUTE")
}

func (m *Manager) endpointsReferencing(chainID string) []string {
	var out []string
	for _, ep := range m.project.Endpoints {
		for _, cid := range ep.ChainIDs {
			if cid == chainID {
				out = append(out, ep.ID)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func FullRecompute(project model.Project) (*engine.Report, error) {
	return engine.Run(project)
}

func JudgmentsEqual(a, b *engine.Report) (bool, error) {
	ja, err := JudgmentJSON(a)
	if err != nil {
		return false, err
	}
	jb, err := JudgmentJSON(b)
	if err != nil {
		return false, err
	}
	return string(ja) == string(jb), nil
}

func OutcomesEqual(a, b *engine.Report) (bool, error) {
	ja, err := OutcomeJSON(a)
	if err != nil {
		return false, err
	}
	jb, err := OutcomeJSON(b)
	if err != nil {
		return false, err
	}
	return string(ja) == string(jb), nil
}

func JudgmentJSON(r *engine.Report) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var cp engine.Report
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, err
	}
	cp.Diagnostics = engine.Diagnostics{}
	for _, rr := range cp.Results {
		rr.Provenance = engine.Provenance{}
		for ci := range rr.ChainResults {
			for li := range rr.ChainResults[ci].Levels {
				rr.ChainResults[ci].Levels[li].Reused = false
			}
		}
	}
	return json.Marshal(struct {
		Results      []*engine.RequestResult          `json:"results"`
		QueueRecords []*engine.QueueRecord            `json:"queue_records"`
		Moments      []engine.MomentReport            `json:"moments"`
		States       map[string]*engine.EndpointState `json:"states"`
	}{cp.Results, cp.QueueRecords, cp.Moments, cp.EndpointStates})
}

func OutcomeJSON(r *engine.Report) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var cp engine.Report
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, err
	}
	cp.Diagnostics = engine.Diagnostics{}
	for _, rr := range cp.Results {
		rr.Provenance = engine.Provenance{}
		for ci := range rr.ChainResults {
			for li := range rr.ChainResults[ci].Levels {
				rr.ChainResults[ci].Levels[li].Reused = false
			}
		}
	}
	return json.Marshal(struct {
		Results      []*engine.RequestResult          `json:"results"`
		QueueRecords []*engine.QueueRecord            `json:"queue_records"`
		States       map[string]*engine.EndpointState `json:"states"`
	}{cp.Results, cp.QueueRecords, cp.EndpointStates})
}

func CloneState(st *engine.EndpointState) (*engine.EndpointState, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var out engine.EndpointState
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func cloneResults(in []*engine.RequestResult) []*engine.RequestResult {
	b, _ := json.Marshal(in)
	var out []*engine.RequestResult
	_ = json.Unmarshal(b, &out)
	return out
}

func splitSources(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
