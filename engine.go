package plancache

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
)

const (
	DecisionReuseHit                = "REUSE_HIT"
	DecisionDerivedMiss             = "DERIVED_MISS"
	DecisionDerivedSettingsRejected = "DERIVED_AFTER_SETTINGS_REJECT"
	DecisionAdmissionRejected       = "ADMISSION_REJECTED"
)

type CacheEntry struct {
	ID                     string            `json:"id"`
	PlanID                 string            `json:"plan_id"`
	StatementID            string            `json:"statement_id"`
	CacheKey               string            `json:"cache_key"`
	Fingerprint            string            `json:"fingerprint"`
	Settings               map[string]string `json:"settings"`
	SizeBytes              int64             `json:"size_bytes"`
	ReuseCount             int               `json:"reuse_count"`
	WriteSeq               int               `json:"write_seq"`
	LastUsedSeq            int               `json:"last_used_seq"`
	LastUsedTickMS         int64             `json:"last_used_tick_ms"`
	Evicted                bool              `json:"evicted"`
	EvictionRule           string            `json:"eviction_rule,omitempty"`
	Invalidated            bool              `json:"invalidated,omitempty"`
	InvalidationSource     string            `json:"invalidation_source,omitempty"`
	InvalidationGeneration int               `json:"invalidation_generation,omitempty"`
}

type SettingDiff struct {
	Key          string `json:"key"`
	EntryValue   string `json:"entry_value"`
	RequestValue string `json:"request_value"`
	Compatible   bool   `json:"compatible"`
	Rule         string `json:"rule"`
	Reason       string `json:"reason"`
}

type CandidateCheck struct {
	PlanID     string        `json:"plan_id"`
	CacheKey   string        `json:"cache_key"`
	Compatible bool          `json:"compatible"`
	Diffs      []SettingDiff `json:"diffs"`
}

type EvictionRecord struct {
	PlanID      string `json:"plan_id"`
	StatementID string `json:"statement_id"`
	CacheKey    string `json:"cache_key"`
	Rule        string `json:"rule"`
	SizeBytes   int64  `json:"size_bytes"`
	ReuseCount  int    `json:"reuse_count"`
	LastUsedSeq int    `json:"last_used_seq"`
}

type WriteReport struct {
	Seq                      int              `json:"seq"`
	PlanID                   string           `json:"plan_id"`
	StatementID              string           `json:"statement_id"`
	CacheKey                 string           `json:"cache_key"`
	WrittenBytes             int64            `json:"written_bytes"`
	OccupiedBytesBeforeWrite int64            `json:"occupied_bytes_before_write"`
	OccupiedBytesAfterWrite  int64            `json:"occupied_bytes_after_write"`
	OccupiedBytesFinal       int64            `json:"occupied_bytes_final"`
	PerStatementLimit        int              `json:"per_statement_limit"`
	ByteLimit                int64            `json:"byte_limit"`
	PerStatementRule         string           `json:"per_statement_rule"`
	TotalBytesRule           string           `json:"total_bytes_rule"`
	Evictions                []EvictionRecord `json:"evictions"`
}

type RequestReport struct {
	Seq           int              `json:"seq"`
	RequestID     string           `json:"request_id"`
	SessionID     string           `json:"session_id"`
	StatementID   string           `json:"statement_id"`
	CacheKey      string           `json:"cache_key"`
	KeyComponents KeyComponents    `json:"key_components"`
	Decision      string           `json:"decision"`
	HitPlanID     string           `json:"hit_plan_id,omitempty"`
	Candidates    []CandidateCheck `json:"candidates"`
	SettingDiffs  []SettingDiff    `json:"setting_diffs,omitempty"`
	RejectCode    string           `json:"reject_code,omitempty"`
	Write         *WriteReport     `json:"write,omitempty"`
	ElapsedMicros int64            `json:"elapsed_micros,omitempty"`
}

type Engine struct {
	mu           sync.Mutex
	project      *Project
	entries      map[string]*CacheEntry
	keyIndex     map[string][]string
	stmtIndex    map[string][]string
	ActiveBytes  int64
	Reports      []RequestReport
	WriteReports []WriteReport
	Evictions    []EvictionRecord
}

func NewEngine(p *Project) (*Engine, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return &Engine{
		project:   p,
		entries:   map[string]*CacheEntry{},
		keyIndex:  map[string][]string{},
		stmtIndex: map[string][]string{},
	}, nil
}

func PlanSizeBytes(stmt Statement) int64 {
	nodes := 0
	refs := 0
	for _, sp := range stmt.Subplans {
		nodes += len(sp.Nodes)
		for _, n := range sp.Nodes {
			if n.Ref != nil {
				refs++
			}
		}
	}
	return int64(384 + len(stmt.Template) + 96*nodes + 48*len(stmt.Params) + 64*refs)
}

func planIDForSettings(stmtID string, p *Project, settings map[string]string) string {
	identity := map[string]string{}
	for k, v := range settings {
		rule, hasRule := p.CompatibilityRules[k]
		if hasRule && rule.Mode == "ignore" {
			continue
		}
		identity[k] = v
	}
	defaultIdentity := map[string]string{}
	for k, v := range p.DefaultSettings {
		rule, hasRule := p.CompatibilityRules[k]
		if hasRule && rule.Mode == "ignore" {
			continue
		}
		defaultIdentity[k] = v
	}
	if canonicalJSON(identity) == canonicalJSON(defaultIdentity) {
		return "PLAN-" + stmtID
	}
	keys := make([]string, 0, len(identity))
	for k := range identity {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+identity[k])
	}
	return "PLAN-" + stmtID + "-" + hashText("plan-id/v1", parts...)[:10]
}

func CheckSettings(p *Project, entrySettings, requestSettings map[string]string) []SettingDiff {
	keys := map[string]bool{}
	for k := range entrySettings {
		keys[k] = true
	}
	for k := range requestSettings {
		keys[k] = true
	}
	keyList := make([]string, 0, len(keys))
	for k := range keys {
		keyList = append(keyList, k)
	}
	sort.Strings(keyList)
	diffs := make([]SettingDiff, 0)
	for _, k := range keyList {
		ev := entrySettings[k]
		rv := requestSettings[k]
		if ev == rv {
			continue
		}
		rule := p.CompatibilityRules[k]
		mode := rule.Mode
		if mode == "" {
			mode = "exact"
		}
		diff := SettingDiff{Key: k, EntryValue: ev, RequestValue: rv, Rule: mode}
		switch mode {
		case "ignore":
			diff.Compatible = true
			diff.Reason = "declared settings difference is ignored for reuse"
		case "exact":
			diff.Compatible = false
			diff.Reason = "settings require exact equality"
		case "one_of":
			allowed := map[string]bool{}
			for _, v := range rule.AllowedValues {
				allowed[v] = true
			}
			diff.Compatible = allowed[rv]
			diff.Reason = "request value is not in declared compatible set"
		case "numeric_at_least_entry":
			en, eErr := strconv.ParseInt(ev, 10, 64)
			rn, rErr := strconv.ParseInt(rv, 10, 64)
			if eErr != nil || rErr != nil {
				diff.Compatible = false
				diff.Reason = "numeric setting is not an integer"
			} else if rn >= en {
				diff.Compatible = true
				diff.Reason = "requested numeric budget meets cached plan requirement"
			} else {
				diff.Compatible = false
				diff.Reason = fmt.Sprintf("requested numeric budget %d is below cached requirement %d", rn, en)
			}
		default:
			diff.Compatible = false
			diff.Reason = "unknown compatibility mode"
		}
		diffs = append(diffs, diff)
	}
	return diffs
}

func (e *Engine) statement(id string) (Statement, bool) {
	for _, st := range e.project.Statements {
		if st.ID == id {
			return st, true
		}
	}
	return Statement{}, false
}

// ProcessRequest applies one request in the caller's chosen logical order.
// Runtime always calls this after restoring global sequence ordering.
func (e *Engine) ProcessRequest(req SessionRequest) (*RequestReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	st, ok := e.statement(req.StatementID)
	if !ok {
		return nil, withContext(newError(CodeRequestNotFound, "request.statement_id", "statement is not declared"), "statement_id", req.StatementID, "request_id", req.RequestID)
	}
	if e.project.Capacity.MaxCacheBytes == 0 {
		return nil, newError(CodeCapacityBytesZero, "capacity.max_cache_bytes", "cannot process request with zero byte cache capacity")
	}
	effective := e.project.EffectiveSettings(req.Settings)
	keyResult := DeriveCacheKey(e.project, st, effective)
	report := &RequestReport{
		Seq: req.Seq, RequestID: req.RequestID, SessionID: req.SessionID,
		StatementID: req.StatementID, CacheKey: keyResult.CacheKey,
		KeyComponents: keyResult.Components, Candidates: []CandidateCheck{},
	}

	candidateIDs := append([]string(nil), e.keyIndex[keyResult.CacheKey]...)
	sort.Slice(candidateIDs, func(i, j int) bool { return candidateIDs[i] < candidateIDs[j] })
	compatible := make([]*CacheEntry, 0)
	for _, id := range candidateIDs {
		candidate := e.entries[id]
		if candidate == nil || candidate.Evicted || candidate.Invalidated {
			continue
		}
		diffs := CheckSettings(e.project, candidate.Settings, effective)
		okCandidate := true
		for _, d := range diffs {
			if !d.Compatible {
				okCandidate = false
			}
		}
		report.Candidates = append(report.Candidates, CandidateCheck{
			PlanID: candidate.PlanID, CacheKey: candidate.CacheKey,
			Compatible: okCandidate, Diffs: diffs,
		})
		if okCandidate {
			compatible = append(compatible, candidate)
		} else {
			report.SettingDiffs = append(report.SettingDiffs, diffs...)
		}
	}

	if len(compatible) > 0 {
		sort.Slice(compatible, func(i, j int) bool {
			a, b := compatible[i], compatible[j]
			if a.ReuseCount != b.ReuseCount {
				return a.ReuseCount > b.ReuseCount
			}
			if a.LastUsedSeq != b.LastUsedSeq {
				return a.LastUsedSeq > b.LastUsedSeq
			}
			return a.ID < b.ID
		})
		hit := compatible[0]
		hit.ReuseCount++
		hit.LastUsedSeq = req.Seq
		hit.LastUsedTickMS = int64(req.Seq) * e.project.Eviction.TickGranularityMS
		report.Decision = DecisionReuseHit
		report.HitPlanID = hit.PlanID
		e.Reports = append(e.Reports, *report)
		return report, nil
	}

	if len(report.Candidates) > 0 {
		anyIncompatible := false
		for _, c := range report.Candidates {
			if !c.Compatible {
				anyIncompatible = true
			}
		}
		if anyIncompatible {
			report.Decision = DecisionDerivedSettingsRejected
			report.RejectCode = "E_REUSE_SETTINGS_INCOMPATIBLE"
		}
	}
	if report.Decision == "" {
		report.Decision = DecisionDerivedMiss
	}

	planID := planIDForSettings(st.ID, e.project, effective)
	cacheEntryID := planID
	if existing := e.entries[cacheEntryID]; existing != nil && !existing.Evicted && !existing.Invalidated {
		// Deterministic re-derivation of an active equivalent plan is an idempotent touch.
		existing.LastUsedSeq = req.Seq
		existing.LastUsedTickMS = int64(req.Seq) * e.project.Eviction.TickGranularityMS
		report.HitPlanID = existing.PlanID
		e.Reports = append(e.Reports, *report)
		return report, nil
	}
	entry := &CacheEntry{
		ID: cacheEntryID, PlanID: planID, StatementID: st.ID, CacheKey: keyResult.CacheKey,
		Fingerprint: st.Fingerprint, Settings: effective, SizeBytes: PlanSizeBytes(st),
		WriteSeq: req.Seq, LastUsedSeq: req.Seq,
		LastUsedTickMS: int64(req.Seq) * e.project.Eviction.TickGranularityMS,
	}
	writeReport, err := e.admit(entry, req.Seq)
	if err != nil {
		return nil, err
	}
	report.Write = writeReport
	e.Reports = append(e.Reports, *report)
	return report, nil
}

func (e *Engine) admit(entry *CacheEntry, seq int) (*WriteReport, error) {
	if e.project.Capacity.MaxCacheBytes <= 0 {
		return nil, newError(CodeCapacityBytesZero, "capacity.max_cache_bytes", "cache byte capacity must be positive")
	}
	before := e.ActiveBytes
	e.entries[entry.ID] = entry
	e.keyIndex[entry.CacheKey] = append(e.keyIndex[entry.CacheKey], entry.ID)
	e.stmtIndex[entry.StatementID] = append(e.stmtIndex[entry.StatementID], entry.ID)
	e.ActiveBytes += entry.SizeBytes
	wr := &WriteReport{
		Seq: seq, PlanID: entry.PlanID, StatementID: entry.StatementID, CacheKey: entry.CacheKey,
		WrittenBytes: entry.SizeBytes, OccupiedBytesBeforeWrite: before,
		OccupiedBytesAfterWrite: e.ActiveBytes, PerStatementLimit: e.project.Capacity.MaxPlansPerStatement,
		ByteLimit:        e.project.Capacity.MaxCacheBytes,
		PerStatementRule: e.project.Eviction.PerStatementRule,
		TotalBytesRule:   e.project.Eviction.TotalBytesRule,
		Evictions:        []EvictionRecord{},
	}

	for len(e.activeStmtIDs(entry.StatementID)) > e.project.Capacity.MaxPlansPerStatement {
		victim := e.chooseVictim(e.activeStmtIDs(entry.StatementID))
		rec := e.evict(victim.ID, e.project.Eviction.PerStatementRule)
		wr.Evictions = append(wr.Evictions, rec)
	}
	for e.ActiveBytes > e.project.Capacity.MaxCacheBytes {
		victim := e.chooseVictim(e.allActiveIDs())
		rec := e.evict(victim.ID, e.project.Eviction.TotalBytesRule)
		wr.Evictions = append(wr.Evictions, rec)
	}
	wr.OccupiedBytesFinal = e.ActiveBytes
	e.WriteReports = append(e.WriteReports, *wr)
	return wr, nil
}

func (e *Engine) activeStmtIDs(stmtID string) []string {
	out := make([]string, 0)
	for _, id := range e.stmtIndex[stmtID] {
		if ent := e.entries[id]; ent != nil && !ent.Evicted && !ent.Invalidated {
			out = append(out, id)
		}
	}
	e.stmtIndex[stmtID] = out
	return out
}

func (e *Engine) allActiveIDs() []string {
	out := make([]string, 0, len(e.entries))
	for id, ent := range e.entries {
		if ent != nil && !ent.Evicted && !ent.Invalidated {
			out = append(out, id)
		}
	}
	return out
}

func (e *Engine) chooseVictim(ids []string) *CacheEntry {
	active := make([]*CacheEntry, 0, len(ids))
	for _, id := range ids {
		if ent := e.entries[id]; ent != nil && !ent.Evicted && !ent.Invalidated {
			active = append(active, ent)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		a, b := active[i], active[j]
		if a.ReuseCount != b.ReuseCount {
			return a.ReuseCount < b.ReuseCount
		}
		if a.LastUsedTickMS != b.LastUsedTickMS {
			return a.LastUsedTickMS < b.LastUsedTickMS
		}
		if a.LastUsedSeq != b.LastUsedSeq {
			return a.LastUsedSeq < b.LastUsedSeq
		}
		return a.ID < b.ID
	})
	return active[0]
}

func (e *Engine) evict(id, rule string) EvictionRecord {
	ent := e.entries[id]
	ent.Evicted = true
	ent.EvictionRule = rule
	e.ActiveBytes -= ent.SizeBytes
	rec := EvictionRecord{
		PlanID: ent.PlanID, StatementID: ent.StatementID, CacheKey: ent.CacheKey,
		Rule: rule, SizeBytes: ent.SizeBytes, ReuseCount: ent.ReuseCount, LastUsedSeq: ent.LastUsedSeq,
	}
	e.Evictions = append(e.Evictions, rec)
	e.removeMapIndex(e.keyIndex, ent.CacheKey, id)
	e.removeMapIndex(e.stmtIndex, ent.StatementID, id)
	return rec
}

func (e *Engine) removeMapIndex(m map[string][]string, key, id string) {
	slice := m[key]
	e.removeFromIndex(&slice, id)
	m[key] = slice
}

func (e *Engine) removeFromIndex(slice *[]string, id string) {
	out := (*slice)[:0]
	for _, x := range *slice {
		if x != id {
			out = append(out, x)
		}
	}
	*slice = out
}

func (e *Engine) ActiveEntries() []CacheEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]CacheEntry, 0)
	for _, ent := range e.entries {
		if !ent.Evicted && !ent.Invalidated {
			out = append(out, *ent)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (e *Engine) Entry(planOrEntryID string) (CacheEntry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ent, ok := e.entries[planOrEntryID]
	if !ok {
		for _, candidate := range e.entries {
			if candidate.PlanID == planOrEntryID {
				ent = candidate
				ok = true
				break
			}
		}
	}
	if ent == nil {
		return CacheEntry{}, false
	}
	return *ent, ok
}

func RunSequential(p *Project) (*Engine, []RequestReport, error) {
	engine, err := NewEngine(p)
	if err != nil {
		return nil, nil, err
	}
	reqs := append([]SessionRequest(nil), p.Requests...)
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].Seq < reqs[j].Seq })
	for _, req := range reqs {
		report, err := engine.ProcessRequest(req)
		if err != nil {
			return engine, engine.Reports, err
		}
		_ = report
	}
	return engine, engine.Reports, nil
}
