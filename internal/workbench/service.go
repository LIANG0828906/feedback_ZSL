package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"tunnelworkbench/internal/checkpoint"
	"tunnelworkbench/internal/forward"
)

type EquivalenceReport struct {
	Consistent         bool               `json:"consistent"`
	TunnelIDs          []string           `json:"tunnel_ids"`
	PeerIDs            []string           `json:"peer_ids"`
	RulePriorityIDs    []string           `json:"rule_priority_ids"`
	SchemaHashes       map[string]string  `json:"schema_hashes"`
	Checks             []EquivalenceCheck `json:"checks"`
	VerdictSignatures  map[string]string  `json:"verdict_signatures"`
	FragmentSignatures map[string]string  `json:"fragment_signatures"`
}

type EquivalenceCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type CheckpointDemo struct {
	DatabasePath       string               `json:"database_path"`
	FirstCommittedSeq  int                  `json:"first_committed_seq"`
	Recovery           *checkpoint.Recovery `json:"recovery,omitempty"`
	FullReplay         bool                 `json:"full_replay"`
	ReplayLastSeq      int                  `json:"replay_last_seq"`
	MatchingFinalState bool                 `json:"matching_final_state"`
	Note               string               `json:"note"`
}

type AnomalyResult struct {
	Kind         string            `json:"kind"`
	Code         string            `json:"code,omitempty"`
	Error        *forward.BizError `json:"error,omitempty"`
	Context      map[string]any    `json:"context,omitempty"`
	HealthyAfter bool              `json:"healthy_after"`
	Note         string            `json:"note"`
}

type Service struct {
	mu        sync.Mutex
	workDir   string
	sample    *forward.Sample
	engineer  map[string]*forward.Engineering
	increment *forward.IncrementalService
	incSchema string
}

func NewService(workDir string) (*Service, error) {
	if workDir == "" {
		workDir = filepath.Join("artifacts", "workbench")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	sample := forward.GenerateSample(forward.DefaultSeed)
	engs := map[string]*forward.Engineering{}
	for _, schema := range []string{forward.SchemaV1, forward.SchemaV2, forward.SchemaV3} {
		eng, err := forward.LoadEngineering(schema, sample.Documents[schema])
		if err != nil {
			return nil, fmt.Errorf("generated %s document invalid: %w", schema, err)
		}
		engs[schema] = eng
	}
	return &Service{workDir: workDir, sample: sample, engineer: engs}, nil
}

func (s *Service) SampleOverview() map[string]any {
	return map[string]any{
		"seed": s.sample.Seed, "project_id": s.sample.Project.ID,
		"event_count": len(s.sample.Events), "packet_count": s.sample.Truth.PacketCount,
		"schemas": []string{forward.SchemaV1, forward.SchemaV2, forward.SchemaV3},
		"workdir": s.workDir,
	}
}

func (s *Service) Document(schema string) (json.RawMessage, error) {
	if _, ok := forward.NormalizeSchema(schema); !ok {
		return nil, forward.NewBizError(forward.CodeSchemaUnknown, "schema", "unknown schema %q", schema)
	}
	return s.sample.Documents[schema], nil
}

func (s *Service) Load(schema string) (*forward.Engineering, error) {
	normalized, ok := forward.NormalizeSchema(schema)
	if !ok {
		return nil, forward.NewBizError(forward.CodeSchemaUnknown, "schema", "unknown schema %q", schema)
	}
	return s.engineer[normalized], nil
}

func (s *Service) Simulate(schema string, parallel bool, budget forward.Budget) (*forward.Report, error) {
	eng, err := s.Load(schema)
	if err != nil {
		return nil, err
	}
	if parallel {
		return forward.RunParallelReport(eng.Project, eng.Events, budget)
	}
	return forward.RunReport(eng.Project, eng.Events)
}

func (s *Service) ResetIncremental(schema string) (*forward.IncrementalService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetIncrementalLocked(schema)
}

func (s *Service) resetIncrementalLocked(schema string) (*forward.IncrementalService, error) {
	eng, err := s.Load(schema)
	if err != nil {
		return nil, err
	}
	s.increment = forward.NewIncrementalService(eng.Project, eng.Events)
	if _, err := s.increment.Build(); err != nil {
		return nil, err
	}
	s.incSchema = schema
	return s.increment, nil
}

func (s *Service) ApplyEdit(req forward.EditRequest) (*forward.IncrementalResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	schema := forward.SchemaV2
	if normalized, ok := forward.NormalizeSchema(req.Schema); ok {
		schema = normalized
	} else if s.incSchema != "" {
		schema = s.incSchema
	}
	if _, err := s.resetIncrementalLocked(schema); err != nil {
		return nil, err
	}
	return s.increment.ApplyAndReplay(req)
}

func (s *Service) Equivalence() (*EquivalenceReport, error) {
	rep := &EquivalenceReport{SchemaHashes: map[string]string{}, VerdictSignatures: map[string]string{}, FragmentSignatures: map[string]string{}}
	tunnelSets := [][]string{}
	peerSets := [][]string{}
	ruleSets := [][]string{}
	verdictSig := map[string]string{}
	fragSig := map[string]string{}

	for _, schema := range []string{forward.SchemaV1, forward.SchemaV2, forward.SchemaV3} {
		eng := s.engineer[schema]
		rep.SchemaHashes[schema] = eng.Project.ContentHash
		tids := []string{}
		for _, t := range eng.Project.Tunnels {
			tids = append(tids, t.ID)
		}
		pids := []string{}
		for _, p := range eng.Project.Peers {
			pids = append(pids, p.ID)
		}
		rids := []string{}
		for _, r := range forward.SortedRules(eng.Project.Rules) {
			rids = append(rids, r.ID)
		}
		tunnelSets = append(tunnelSets, tids)
		peerSets = append(peerSets, pids)
		ruleSets = append(ruleSets, rids)
		if rep.TunnelIDs == nil {
			rep.TunnelIDs = tids
			rep.PeerIDs = pids
			rep.RulePriorityIDs = rids
		}

		report, err := forward.RunReport(eng.Project, eng.Events)
		if err != nil {
			return nil, err
		}
		vsig, fsig := decisionSignatures(report)
		verdictSig[schema] = vsig
		fragSig[schema] = fsig
		rep.VerdictSignatures[schema] = shortSig(vsig)
		rep.FragmentSignatures[schema] = shortSig(fsig)
	}

	rep.Checks = appendCheck(rep.Checks, "tunnel_id_order", equalStringSlices(tunnelSets[0], tunnelSets[1]) && equalStringSlices(tunnelSets[0], tunnelSets[2]), fmt.Sprintf("%v", tunnelSets[0]))
	rep.Checks = appendCheck(rep.Checks, "peer_members", equalStringSlices(peerSets[0], peerSets[1]) && equalStringSlices(peerSets[0], peerSets[2]), fmt.Sprintf("%v", peerSets[0]))
	rep.Checks = appendCheck(rep.Checks, "rule_priority_order", equalStringSlices(ruleSets[0], ruleSets[1]) && equalStringSlices(ruleSets[0], ruleSets[2]), fmt.Sprintf("%v", ruleSets[0]))
	verdictOK := verdictSig[forward.SchemaV1] == verdictSig[forward.SchemaV2] && verdictSig[forward.SchemaV1] == verdictSig[forward.SchemaV3]
	fragOK := fragSig[forward.SchemaV1] == fragSig[forward.SchemaV2] && fragSig[forward.SchemaV1] == fragSig[forward.SchemaV3]
	rep.Checks = appendCheck(rep.Checks, "verdict_hit_set_and_path", verdictOK, "winner and rule path signatures identical after unit/address normalization")
	rep.Checks = appendCheck(rep.Checks, "fragment_count_and_bytes", fragOK, "piece counts, sizes and reject codes identical")
	rep.Consistent = true
	for _, c := range rep.Checks {
		if !c.OK {
			rep.Consistent = false
		}
	}
	return rep, nil
}

func decisionSignatures(r *forward.Report) (string, string) {
	type verdictSig struct {
		PacketID string
		Winner   string
		Basis    string
		Matches  []string
		PathIDs  []string
	}
	type fragSig struct {
		PacketID string
		Code     string
		Count    int
		Sizes    []int64
		Total    int64
	}
	vs := []verdictSig{}
	fs := []fragSig{}
	for _, ev := range r.Events {
		for _, pe := range ev.Packets {
			pathIDs := []string{}
			for _, step := range pe.Verdict.Path {
				if step.Evaluated {
					pathIDs = append(pathIDs, step.RuleID)
				}
			}
			vs = append(vs, verdictSig{pe.Packet.ID, pe.Verdict.WinnerRuleID, pe.Verdict.Basis, pe.Verdict.MatchingIDs, pathIDs})
			sizes := []int64{}
			for _, pc := range pe.Plan.Pieces {
				sizes = append(sizes, pc.PayloadBytes)
			}
			fs = append(fs, fragSig{pe.Packet.ID, pe.Plan.RejectCode, pe.Plan.PieceCount, sizes, pe.Plan.TotalOuterBytes})
		}
	}
	vb, _ := json.Marshal(vs)
	fb, _ := json.Marshal(fs)
	return string(vb), string(fb)
}

func shortSig(s string) string {
	p := &forward.Project{ID: s}
	hash := p.CanonicalHash()
	return forward.ShortHash(hash)
}

func appendCheck(checks []EquivalenceCheck, name string, ok bool, detail string) []EquivalenceCheck {
	if !ok {
		return append(checks, EquivalenceCheck{Name: name, OK: false, Detail: detail})
	}
	return append(checks, EquivalenceCheck{Name: name, OK: true, Detail: detail})
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Service) CheckpointDemo(ctx context.Context, schema string, stopAfter int) (*CheckpointDemo, error) {
	eng, err := s.Load(schema)
	if err != nil {
		return nil, err
	}
	if stopAfter <= 0 || stopAfter > len(eng.Events) {
		stopAfter = 7
	}
	dbPath := filepath.Join(s.workDir, fmt.Sprintf("checkpoint-%s.db", schema))
	_ = os.Remove(dbPath)
	store, err := checkpoint.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	first, err := checkpoint.RunAndCheckpointPrefix(ctx, store, eng.Project, eng.Events, stopAfter)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	firstSeq := first.LastCommitted
	if err := store.Close(); err != nil {
		return nil, err
	}

	store2, err := checkpoint.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer store2.Close()
	recovery, biz := store2.LoadCommitted(ctx, eng.Project.SourceSchema, eng.Project.ContentHash)
	if biz != nil {
		return nil, biz
	}
	replay, rec, full, err := checkpoint.ReplayFromRecovery(ctx, store2, eng.Project, eng.Events)
	if err != nil {
		return nil, err
	}
	finalMatch := false
	if replay.Report != nil {
		fullReport, err := forward.RunReport(eng.Project, eng.Events)
		if err == nil {
			combined := combineRecoveredFinal(rec, replay.Report.FinalStates, eng.Project)
			finalMatch = finalStatesEqual(combined, fullReport.FinalStates)
		}
	}
	return &CheckpointDemo{
		DatabasePath: dbPath, FirstCommittedSeq: firstSeq, Recovery: recovery,
		FullReplay: full, ReplayLastSeq: replay.LastCommitted, MatchingFinalState: finalMatch,
		Note: "state history rows beyond last_event_seq are never read; restart replays only committed-seq suffix",
	}, nil
}

func combineRecoveredFinal(rec *checkpoint.Recovery, replayed []forward.TunnelStatus, p *forward.Project) []forward.TunnelStatus {
	byID := map[string]forward.TunnelStatus{}
	if rec != nil {
		for _, st := range rec.States {
			byID[st.TunnelID] = st
		}
	}
	for _, st := range replayed {
		byID[st.TunnelID] = st
	}
	out := []forward.TunnelStatus{}
	for _, t := range p.Tunnels {
		if st, ok := byID[t.ID]; ok {
			out = append(out, st)
		}
	}
	return out
}

func finalStatesEqual(a, b []forward.TunnelStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].TunnelID != b[i].TunnelID || a[i].State != b[i].State {
			return false
		}
	}
	return true
}
