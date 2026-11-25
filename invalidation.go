package plancache

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type InvalidationItem struct {
	EventID     string   `json:"event_id"`
	PlanID      string   `json:"plan_id"`
	EntryID     string   `json:"entry_id"`
	StatementID string   `json:"statement_id"`
	CacheKey    string   `json:"cache_key"`
	Generation  int      `json:"generation"`
	Source      string   `json:"source"`
	Path        []string `json:"path"`
}

type ClosureReport struct {
	EventID  string             `json:"event_id"`
	Kind     string             `json:"kind"`
	Root     string             `json:"root_statement_id"`
	Source   string             `json:"source"`
	MaxDepth int                `json:"max_depth"`
	Items    []InvalidationItem `json:"items"`
}

// ReferenceGraph returns provider -> direct consumers. An edge means the
// consumer plan contains a SUBPLAN_REF (or equivalent) to the provider.
func ReferenceGraph(p *Project) map[string][]string {
	dependents := map[string]map[string]bool{}
	for _, consumer := range p.Statements {
		for _, sp := range consumer.Subplans {
			for _, n := range sp.Nodes {
				if n.Ref == nil || n.Ref.StatementID == "" {
					continue
				}
				provider := n.Ref.StatementID
				if dependents[provider] == nil {
					dependents[provider] = map[string]bool{}
				}
				dependents[provider][consumer.ID] = true
			}
		}
	}
	out := map[string][]string{}
	for provider, set := range dependents {
		out[provider] = make([]string, 0, len(set))
		for id := range set {
			out[provider] = append(out[provider], id)
		}
		sort.Strings(out[provider])
	}
	return out
}

func detectReferenceCycle(root string, graph map[string][]string) []string {
	state := map[string]int{}
	var stack []string
	var cycle []string
	var dfs func(string) bool
	dfs = func(node string) bool {
		state[node] = 1
		stack = append(stack, node)
		for _, next := range graph[node] {
			if state[next] == 0 {
				if dfs(next) {
					return true
				}
			} else if state[next] == 1 {
				start := 0
				for i, id := range stack {
					if id == next {
						start = i
						break
					}
				}
				cycle = append(append([]string{}, stack[start:]...), next)
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[node] = 2
		return false
	}
	if dfs(root) {
		return cycle
	}
	return nil
}

// AuditReferences checks explicit PlanID references and cache fingerprints.
// allEntries should include tombstones so evicted references can be reported
// distinctly from references that never existed.
func AuditReferences(p *Project, active []CacheEntry, allEntries map[string]CacheEntry) error {
	if allEntries == nil {
		allEntries = map[string]CacheEntry{}
		for _, e := range active {
			allEntries[e.ID] = e
			allEntries[e.PlanID] = e
		}
	}
	for _, st := range p.Statements {
		for _, sp := range st.Subplans {
			for _, n := range sp.Nodes {
				if n.Ref == nil || n.Ref.PlanID == "" {
					continue
				}
				path := fmt.Sprintf("statements[%s].nodes[%s].ref.plan_id", st.ID, n.ID)
				ent, ok := allEntries[n.Ref.PlanID]
				if !ok {
					return withContext(newError(CodeReferenceDangling, path, "reference points to plan absent from cache registry"), "plan_id", n.Ref.PlanID, "node_id", n.ID)
				}
				if ent.Evicted || ent.Invalidated {
					return withContext(newError(CodeReferenceEvicted, path, "reference points to an evicted or invalidated plan"), "plan_id", n.Ref.PlanID, "node_id", n.ID, "evicted", ent.Evicted, "invalidated", ent.Invalidated)
				}
			}
		}
	}
	for _, ent := range active {
		st, ok := statementByID(p, ent.StatementID)
		if !ok {
			continue
		}
		expected := ComputeFingerprint(*st)
		if ent.Fingerprint != "" && ent.Fingerprint != expected {
			return withContext(newError(CodeFingerprintConflict, "cache_entries["+ent.ID+"].fingerprint", "cache entry fingerprint conflicts with canonical plan"), "entry_id", ent.ID, "expected", expected, "actual", ent.Fingerprint)
		}
	}
	return nil
}

// SolveInvalidations computes deterministic closure reports. activeEntries are
// the only plans that can be invalidated. allEntries may contain tombstones
// for precise E_REFERENCE_TO_EVICTED_PLAN reports.
func SolveInvalidations(p *Project, activeEntries []CacheEntry, allEntries map[string]CacheEntry) ([]ClosureReport, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	if err := AuditReferences(p, activeEntries, allEntries); err != nil {
		return nil, err
	}
	graph := ReferenceGraph(p)
	byStmt := map[string][]CacheEntry{}
	for _, ent := range activeEntries {
		if ent.Evicted || ent.Invalidated {
			continue
		}
		byStmt[ent.StatementID] = append(byStmt[ent.StatementID], ent)
	}
	for k := range byStmt {
		sort.Slice(byStmt[k], func(i, j int) bool { return byStmt[k][i].ID < byStmt[k][j].ID })
	}
	events := append([]InvalidationEdit(nil), p.InvalidationEdits...)
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	reports := make([]ClosureReport, 0, len(events))
	for _, event := range events {
		report, err := solveOneInvalidationEvent(p, event, graph, byStmt)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func solveOneInvalidationEvent(p *Project, event InvalidationEdit, graph map[string][]string, byStmt map[string][]CacheEntry) (ClosureReport, error) {
	if event.StatementID == "" {
		return ClosureReport{}, newError(CodeSchemaMissingField, "invalidation_edits["+event.ID+"].statement_id", "invalidation event requires statement id")
	}
	if cycle := detectReferenceCycle(event.StatementID, graph); cycle != nil {
		return ClosureReport{}, withContext(newError(CodeReferenceCycle, "reference_graph", "subplan reference cycle reaches invalidation root"), "root", event.StatementID, "cycle", strings.Join(cycle, " -> "))
	}
	report := ClosureReport{
		EventID: event.ID, Kind: event.Kind, Root: event.StatementID,
		Source: event.Source, MaxDepth: p.PropagationDepth, Items: []InvalidationItem{},
	}
	visitedGen := map[string]int{}
	type queueNode struct {
		stmt string
		gen  int
		path []string
	}
	queue := []queueNode{{stmt: event.StatementID, gen: 0, path: []string{event.StatementID}}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if gen, seen := visitedGen[cur.stmt]; seen && gen <= cur.gen {
			continue
		}
		visitedGen[cur.stmt] = cur.gen
		for _, ent := range byStmt[cur.stmt] {
			report.Items = append(report.Items, InvalidationItem{
				EventID: event.ID, PlanID: ent.PlanID, EntryID: ent.ID,
				StatementID: cur.stmt, CacheKey: ent.CacheKey, Generation: cur.gen,
				Source: event.Source, Path: append([]string(nil), cur.path...),
			})
		}
		if cur.gen >= p.PropagationDepth {
			continue
		}
		deps := append([]string(nil), graph[cur.stmt]...)
		sort.Strings(deps)
		for _, dep := range deps {
			path := append(append([]string(nil), cur.path...), dep)
			queue = append(queue, queueNode{stmt: dep, gen: cur.gen + 1, path: path})
		}
	}
	sort.Slice(report.Items, func(i, j int) bool {
		if report.Items[i].Generation != report.Items[j].Generation {
			return report.Items[i].Generation < report.Items[j].Generation
		}
		return report.Items[i].PlanID < report.Items[j].PlanID
	})
	return report, nil
}

// SolveInvalidationsParallel computes independent event closures with a fixed
// worker budget. Output ordering remains event-ID order.
func SolveInvalidationsParallel(p *Project, activeEntries []CacheEntry, allEntries map[string]CacheEntry, workers int) ([]ClosureReport, error) {
	if workers <= 0 {
		return nil, withContext(newError(CodeBudgetWorkersInvalid, "runtime.workers", "worker budget must be positive"), "workers", workers)
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	if err := AuditReferences(p, activeEntries, allEntries); err != nil {
		return nil, err
	}
	graph := ReferenceGraph(p)
	byStmt := map[string][]CacheEntry{}
	for _, ent := range activeEntries {
		if ent.Evicted || ent.Invalidated {
			continue
		}
		byStmt[ent.StatementID] = append(byStmt[ent.StatementID], ent)
	}
	events := append([]InvalidationEdit(nil), p.InvalidationEdits...)
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	reports := make([]ClosureReport, len(events))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for i, event := range events {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, event InvalidationEdit) {
			defer wg.Done()
			defer func() { <-sem }()
			report, err := solveOneInvalidationEvent(p, event, graph, byStmt)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			reports[i] = report
		}(i, event)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return reports, nil
}

func (e *Engine) AllEntriesMap() map[string]CacheEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]CacheEntry{}
	for id, ent := range e.entries {
		cp := *ent
		out[id] = cp
		out[ent.PlanID] = cp
	}
	return out
}

func (e *Engine) SolveInvalidations() ([]ClosureReport, error) {
	return SolveInvalidations(e.project, e.ActiveEntries(), e.AllEntriesMap())
}

// ApplyClosure removes closure entries from active lookup indexes and records
// propagation progress. It is deterministic because closure items are sorted.
func (e *Engine) ApplyClosure(reports []ClosureReport) []InvalidationProgress {
	e.mu.Lock()
	defer e.mu.Unlock()
	progress := make([]InvalidationProgress, 0, len(reports))
	for _, report := range reports {
		for _, item := range report.Items {
			ent := e.entries[item.EntryID]
			if ent == nil || ent.Invalidated {
				continue
			}
			ent.Invalidated = true
			ent.InvalidationSource = item.Source
			ent.InvalidationGeneration = item.Generation
			e.ActiveBytes -= ent.SizeBytes
			e.removeMapIndex(e.keyIndex, ent.CacheKey, ent.ID)
			e.removeMapIndex(e.stmtIndex, ent.StatementID, ent.ID)
		}
		maxGen := 0
		for _, item := range report.Items {
			if item.Generation > maxGen {
				maxGen = item.Generation
			}
		}
		progress = append(progress, InvalidationProgress{
			EventID: report.EventID, AppliedItems: len(report.Items),
			MaxGeneration: maxGen, Status: "APPLIED",
		})
	}
	return progress
}

type InvalidationProgress struct {
	EventID       string `json:"event_id"`
	AppliedItems  int    `json:"applied_items"`
	MaxGeneration int    `json:"max_generation"`
	Status        string `json:"status"`
}

func PrimeAllPlans(p *Project) (*Engine, error) {
	engine, err := NewEngine(p)
	if err != nil {
		return nil, err
	}
	for i, st := range p.Statements {
		req := SessionRequest{
			Seq: 1000 + i, RequestID: fmt.Sprintf("prime-%s", st.ID),
			SessionID: "prime", StatementID: st.ID, Settings: map[string]string{},
		}
		if _, err := engine.ProcessRequest(req); err != nil {
			return nil, err
		}
	}
	return engine, nil
}
