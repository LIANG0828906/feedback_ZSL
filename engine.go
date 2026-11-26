package cachebench

import (
	"fmt"
	"hash/fnv"
	"sort"
)

type completion struct {
	nodeID    string
	requestID string
	isError   bool
	latencyMS int64
	atMS      int64
}

type nodeRuntime struct {
	node      Node
	inflight  int
	highSince int64
	open      bool
	openSince int64
	events    []BreakerEvent
	recent    []completion
	due       map[int64][]completion
}

// Simulate runs the deterministic logical-millisecond kernel. The input project
// is validated by value and is never modified.
func Simulate(project Project) (*SimulationResult, error) {
	res, _, err := simulateRange(project, 0, nil, false)
	if err != nil {
		return nil, err
	}
	res.Windows = BuildWindows(project, res)
	return res, nil
}

// SimulateWithBoundaries additionally returns deep-copyable kernel states at
// every committed logical tick. Incremental edits use these as cache seeds.
func SimulateWithBoundaries(project Project) (*SimulationResult, map[int64]*BoundaryState, error) {
	res, boundaries, err := simulateRange(project, 0, nil, true)
	if err != nil {
		return nil, nil, err
	}
	res.Windows = BuildWindows(project, res)
	return res, boundaries, nil
}

func simulateRange(project Project, start int64, initial *BoundaryState, collect bool) (*SimulationResult, map[int64]*BoundaryState, error) {
	if err := ValidateProject(project); err != nil {
		return nil, nil, err
	}
	byID := map[string]Node{}
	runtimes := map[string]*nodeRuntime{}
	for i := range project.Nodes {
		if project.Nodes[i].DeclarationOrder == 0 {
			project.Nodes[i].DeclarationOrder = i
		}
		n := project.Nodes[i]
		byID[n.ID] = n
		runtimes[n.ID] = &nodeRuntime{node: n, highSince: -1, openSince: -1}
	}

	reqs := append([]Request(nil), project.Requests...)
	sort.SliceStable(reqs, func(i, j int) bool {
		if reqs[i].AtMS != reqs[j].AtMS {
			return reqs[i].AtMS < reqs[j].AtMS
		}
		if reqs[i].Ordinal != reqs[j].Ordinal {
			return reqs[i].Ordinal < reqs[j].Ordinal
		}
		return reqs[i].ID < reqs[j].ID
	})
	byArrival := map[int64][]Request{}
	var maxArrival, maxService int64
	for _, r := range reqs {
		byArrival[r.AtMS] = append(byArrival[r.AtMS], r)
		if r.AtMS > maxArrival {
			maxArrival = r.AtMS
		}
	}
	for _, n := range project.Nodes {
		if n.ServiceMS > maxService {
			maxService = n.ServiceMS
		}
	}
	horizon := project.DurationMS
	if maxArrival > horizon {
		horizon = maxArrival
	}
	horizon += maxService + 1

	due := map[int64][]completion{}
	if initial != nil {
		for id, bs := range initial.Nodes {
			n, ok := byID[id]
			if !ok {
				return nil, nil, NewError(CodeNodeNotFound, "boundary references missing node %s", id).WithNode(id)
			}
			rt := &nodeRuntime{node: n, inflight: bs.Inflight, highSince: bs.HighSinceMS, open: bs.CircuitOpen, openSince: bs.OpenSinceMS}
			for _, bc := range bs.Recent {
				rt.recent = append(rt.recent, bc.toCompletion())
			}
			runtimes[id] = rt
		}
		for _, sc := range initial.Due {
			c := sc.toCompletion()
			due[sc.AtMS] = append(due[sc.AtMS], c)
		}
	}
	result := &SimulationResult{ProjectID: project.ProjectID, CheckpointVer: CheckpointSchemaVersion, FinalTick: horizon}
	result.NodeIDs = append(result.NodeIDs, nodeIDsSorted(project.Nodes)...)
	boundaries := map[int64]*BoundaryState{}
	applyPriorOverrides(project, start, byID, runtimes)

	for tick := start; tick <= horizon; tick++ {
		tr := TickResult{TickMS: tick}
		applyOverridesAt(project, tick, byID, runtimes)

		// 1. Complete work whose integer-ms deadline has arrived.
		completedNow := due[tick]
		delete(due, tick)
		completedByNode := map[string]int{}
		errorsByNode := map[string]int{}
		latencyByNode := map[string]int64{}
		acceptedByNode := map[string]int{}
		for _, c := range completedNow {
			rt := runtimes[c.nodeID]
			if rt.inflight > 0 {
				rt.inflight--
			}
			rt.recent = append(rt.recent, c)
			completedByNode[c.nodeID]++
			latencyByNode[c.nodeID] += c.latencyMS
			if c.isError {
				errorsByNode[c.nodeID]++
			}
			tr.TotalThroughput++
		}

		// 2. Recovery is time-based but uses only normalized logical ms.
		for _, id := range result.NodeIDs {
			rt := runtimes[id]
			pruneRecent(rt, tick, project.WindowMS)
			if rt.open {
				rate, samples := currentErrorRate(rt)
				n := rt.node
				recoveryRate := n.ErrorRateRecoveryPPM
				if recoveryRate == 0 {
					recoveryRate = n.ErrorRateLimitPPM
				}
				rateOK := samples < n.MinErrorSamples || rate <= recoveryRate
				if tick-rt.openSince >= n.RecoveryMS && rt.inflight <= n.RecoveryInflightThreshold && rateOK {
					rt.open = false
					ev := BreakerEvent{TickMS: tick, NodeID: id, Recovered: true, ReasonCode: "RECOVERED", BasisID: "CB-RECOVER-" + id, RecoveryAt: tick}
					rt.events = append(rt.events, ev)
					tr.BreakerEvents = append(tr.BreakerEvents, ev)
					rt.openSince = -1
					rt.highSince = -1
				}
			}
		}

		// 3. Evaluate error-rate and sustained-inflight overload before routing.
		for _, id := range result.NodeIDs {
			rt := runtimes[id]
			if rt.open {
				continue
			}
			if ev := evaluateBreaker(rt, tick, project.WindowMS, false); ev != nil {
				tr.BreakerEvents = append(tr.BreakerEvents, *ev)
			}
		}

		// 4. Route and admit arrivals in stable request order.
		for _, req := range byArrival[tick] {
			candidateNodes, derr := resolveCandidates(req, project, byID)
			if derr != nil {
				return nil, nil, derr
			}
			eligible := make([]Node, 0, len(candidateNodes))
			for _, n := range candidateNodes {
				rt := runtimes[n.ID]
				if !rt.open && n.WeightPermyriad > 0 {
					eligible = append(eligible, n)
				}
			}
			if len(eligible) == 0 {
				id := ""
				constraint := "ROUTE_NO_ELIGIBLE"
				if len(candidateNodes) > 0 {
					id = candidateNodes[0].ID
					constraint = candidateNodes[0].ConstraintID
				}
				ov := Overflow{RequestID: req.ID, NodeID: id, ConstraintID: constraint, TickMS: tick, ReasonCode: CodeNoEligibleNode, BasisID: "ROUTE-DENY-" + id}
				tr.Overflow = append(tr.Overflow, ov)
				continue
			}
			decision := Route(req, eligible, project.RoutingPolicy)
			decision.TickMS = tick
			tr.Decisions = append(tr.Decisions, decision)
			rt := runtimes[decision.NodeID]
			if rt.inflight >= rt.node.MaxInflight {
				ov := Overflow{RequestID: req.ID, NodeID: decision.NodeID, ConstraintID: rt.node.ConstraintID, TickMS: tick, ReasonCode: CodeCapacityOverflow, BasisID: "CAPACITY-" + rt.node.ConstraintID}
				tr.Overflow = append(tr.Overflow, ov)
				continue
			}
			rt.inflight++
			acceptedByNode[decision.NodeID]++
			latency := req.LatencyMS
			if latency <= 0 {
				latency = rt.node.ServiceMS
			}
			finish := tick + rt.node.ServiceMS
			c := completion{nodeID: rt.node.ID, requestID: req.ID, isError: req.IsError, latencyMS: latency, atMS: finish}
			if rt.node.ServiceMS == 0 {
				// Complete zero-duration work immediately after admission.
				rt.inflight--
				rt.recent = append(rt.recent, c)
				tr.TotalThroughput++
				completedByNode[rt.node.ID]++
				latencyByNode[rt.node.ID] += c.latencyMS
				if c.isError {
					errorsByNode[rt.node.ID]++
				}
			} else {
				due[finish] = append(due[finish], c)
			}
			_ = latency
		}

		// 5. Admission can cross the sustained threshold; open now so the event
		// is visible at this logical tick and affects future ticks.
		for _, id := range result.NodeIDs {
			rt := runtimes[id]
			if rt.open {
				continue
			}
			if ev := evaluateBreaker(rt, tick, project.WindowMS, true); ev != nil {
				tr.BreakerEvents = append(tr.BreakerEvents, *ev)
			}
		}

		for _, id := range result.NodeIDs {
			rt := runtimes[id]
			openIDs := []string{}
			if rt.open {
				openIDs = append(openIDs, id)
				tr.OpenCircuits = append(tr.OpenCircuits, id)
			}
			nt := NodeTick{
				NodeID: id, TickMS: tick, Inflight: rt.inflight,
				Accepted: acceptedByNode[id], Completed: completedByNode[id], ErrorsCompleted: errorsByNode[id],
				LatencyTotalMS: latencyByNode[id],
				CircuitOpen:    rt.open, HighSinceMS: rt.highSince,
				OpenSinceMS: rt.openSince,
			}
			if rt.open {
				nt.RecoveryAtMS = rt.openSince + rt.node.RecoveryMS
			}
			if len(tr.Overflow) > 0 {
				for _, ov := range tr.Overflow {
					if ov.NodeID == id {
						nt.Overflow++
					}
				}
			}
			_ = openIDs
			tr.Nodes = append(tr.Nodes, nt)
		}
		sort.Slice(tr.Nodes, func(i, j int) bool { return tr.Nodes[i].NodeID < tr.Nodes[j].NodeID })
		sort.Slice(tr.OpenCircuits, func(i, j int) bool { return tr.OpenCircuits[i] < tr.OpenCircuits[j] })
		sort.Slice(tr.Overflow, func(i, j int) bool {
			if tr.Overflow[i].TickMS != tr.Overflow[j].TickMS {
				return tr.Overflow[i].TickMS < tr.Overflow[j].TickMS
			}
			if tr.Overflow[i].NodeID != tr.Overflow[j].NodeID {
				return tr.Overflow[i].NodeID < tr.Overflow[j].NodeID
			}
			return tr.Overflow[i].RequestID < tr.Overflow[j].RequestID
		})
		result.Ticks = append(result.Ticks, tr)
		if collect {
			boundaries[tick] = cloneBoundary(tick, result.NodeIDs, runtimes, due)
		}
	}

	return result, boundaries, nil
}

func applyPriorOverrides(p Project, before int64, byID map[string]Node, rts map[string]*nodeRuntime) {
	for _, o := range p.Overrides {
		if o.AtMS < before {
			applyNodeOverride(o, byID, rts)
		}
	}
}

func applyOverridesAt(p Project, tick int64, byID map[string]Node, rts map[string]*nodeRuntime) {
	for _, o := range p.Overrides {
		if o.AtMS == tick {
			applyNodeOverride(o, byID, rts)
		}
	}
}

func applyNodeOverride(o NodeOverride, byID map[string]Node, rts map[string]*nodeRuntime) {
	n, ok := byID[o.NodeID]
	if !ok {
		return
	}
	if o.SetMaxInflight {
		n.MaxInflight = o.MaxInflight
	}
	if o.SetWeight {
		n.WeightPermyriad = o.WeightPermyriad
	}
	if o.SetInflightThreshold {
		n.InflightThreshold = o.InflightThreshold
	}
	byID[o.NodeID] = n
	if rt := rts[o.NodeID]; rt != nil {
		rt.node = n
	}
}

func nodeIDsSorted(nodes []Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	return ids
}

func resolveCandidates(req Request, p Project, byID map[string]Node) ([]Node, error) {
	ids := req.Candidates
	if len(ids) == 0 {
		ids = nodeIDsSorted(p.Nodes)
	}
	out := make([]Node, 0, len(ids))
	for _, id := range ids {
		n, ok := byID[id]
		if !ok {
			return nil, NewError(CodeNodeNotFound, "request %s points to missing node %s", req.ID, id).
				WithNode(id).WithField("candidates").WithContext("request_id", req.ID)
		}
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DeclarationOrder < out[j].DeclarationOrder })
	return out, nil
}

// Arbitrate applies declared priority (smaller wins) then stable declaration
// order and returns the basis identifier for audit output.
func Arbitrate(nodes []Node) (Node, string) {
	if len(nodes) == 0 {
		return Node{}, "ARB-EMPTY"
	}
	best := nodes[0]
	for _, n := range nodes[1:] {
		if n.Priority < best.Priority || (n.Priority == best.Priority && n.DeclarationOrder < best.DeclarationOrder) {
			best = n
		}
	}
	minPriority := best.Priority
	countMin := 0
	for _, n := range nodes {
		if n.Priority == minPriority {
			countMin++
		}
	}
	if countMin == 1 {
		return best, "ARB-PRI-" + best.ID
	}
	return best, "ARB-ORD-" + best.ID
}

// Route performs deterministic weighted routing. Canonical weights are per
// 10000, so permille and permyriad source documents share one bucket space.
func Route(req Request, candidates []Node, policy RoutingPolicy) RoutingDecision {
	sorted := append([]Node(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].DeclarationOrder < sorted[j].DeclarationOrder })
	_, arbBasis := Arbitrate(sorted)
	d := RoutingDecision{RequestID: req.ID, Candidates: nodeIDsSorted(sorted), ArbitrationBasisID: arbBasis}

	eligible := sorted
	if policy.Mode == "weighted_priority" || policy.Mode == "" {
		best, _ := Arbitrate(sorted)
		top := make([]Node, 0, len(sorted))
		for _, n := range sorted {
			if n.Priority == best.Priority {
				top = append(top, n)
			}
		}
		if len(top) > 0 {
			eligible = top
		}
	}
	if policy.Mode == "priority" {
		best, basis := Arbitrate(sorted)
		d.NodeID = best.ID
		d.BasisID = basis
		d.ArbitrationBasisID = basis
		return d
	}

	var total int64
	for _, n := range eligible {
		total += n.WeightPermyriad
	}
	d.TotalWeight = total
	if total <= 0 {
		best, basis := Arbitrate(eligible)
		d.NodeID = best.ID
		d.BasisID = basis
		return d
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(req.ID))
	bucket := int64(h.Sum32()) % total
	d.Bucket = bucket
	var cumulative int64
	selected := eligible[len(eligible)-1]
	for _, n := range eligible {
		cumulative += n.WeightPermyriad
		if bucket < cumulative {
			selected = n
			break
		}
	}
	d.NodeID = selected.ID
	d.BasisID = fmt.Sprintf("B-WEIGHT-%s-bucket-%d", selected.ID, bucket)
	return d
}

func pruneRecent(rt *nodeRuntime, tick, windowMS int64) {
	if windowMS <= 0 {
		return
	}
	kept := rt.recent[:0]
	for _, c := range rt.recent {
		if c.atMS > tick-windowMS {
			kept = append(kept, c)
		}
	}
	rt.recent = kept
}

func currentErrorRate(rt *nodeRuntime) (ratePPM int64, samples int64) {
	var errors int64
	for _, c := range rt.recent {
		samples++
		if c.isError {
			errors++
		}
	}
	rate, _ := ErrorRatePPM(errors, samples)
	return rate, samples
}

func evaluateBreaker(rt *nodeRuntime, tick, windowMS int64, afterAdmission bool) *BreakerEvent {
	n := rt.node
	if rt.highSince == -1 && rt.inflight > n.InflightThreshold {
		rt.highSince = tick
	}
	inflightOpen := false
	if rt.inflight > n.InflightThreshold && rt.highSince >= 0 && tick-rt.highSince >= n.TriggerMS {
		inflightOpen = true
	}
	rate, samples := currentErrorRate(rt)
	errorOpen := samples >= n.MinErrorSamples && rate > n.ErrorRateLimitPPM
	if !inflightOpen && !errorOpen {
		if rt.inflight <= n.InflightThreshold {
			rt.highSince = -1
		}
		return nil
	}
	reason := "REASON_INFLIGHT_HIGH"
	basis := "CB-INFLIGHT-" + n.ID
	if errorOpen && (!inflightOpen || afterAdmission == false) {
		reason = "REASON_ERROR_RATE"
		basis = "CB-ERROR-" + n.ID
	}
	rt.open = true
	rt.openSince = tick
	ev := BreakerEvent{TickMS: tick, NodeID: n.ID, Opened: true, ReasonCode: reason, BasisID: basis, RecoveryAt: tick + n.RecoveryMS}
	rt.events = append(rt.events, ev)
	return &ev
}

// BuildWindows derives fixed-point window statistics from completed work.
func BuildWindows(p Project, sim *SimulationResult) []WindowStat {
	if p.WindowMS <= 0 {
		return nil
	}
	maxWindow := int(sim.FinalTick/p.WindowMS) + 1
	windows := make([]WindowStat, 0, maxWindow)
	for w := 0; w <= maxWindow; w++ {
		start := int64(w) * p.WindowMS
		end := start + p.WindowMS
		var reqs, errs, latency int64
		for _, t := range sim.Ticks {
			if t.TickMS < start || t.TickMS >= end {
				continue
			}
			for _, nt := range t.Nodes {
				reqs += int64(nt.Completed)
				errs += int64(nt.ErrorsCompleted)
				latency += nt.LatencyTotalMS
			}
		}
		rate, _ := ErrorRatePPM(errs, reqs)
		throughputNumerator, ok := safeMul(reqs, 1_000_000)
		throughputQ3 := int64(0)
		if ok && p.WindowMS > 0 {
			throughputQ3 = throughputNumerator / p.WindowMS
		}
		avg := AverageFixed(latency, reqs, 1000, 64)
		tolerancePPM := p.StatTolerancePPM
		if tolerancePPM == 0 {
			tolerancePPM = 5000
		}
		rateToMyriad := ConvertScale(rate, PPMScale, ScalePermyriad, 64)
		rateBackToPPM := ConvertScale(rateToMyriad.Value, ScalePermyriad, PPMScale, 64)
		ws := WindowStat{
			WindowIndex: w, StartMS: start, EndMS: end,
			Requests: reqs, Errors: errs, TotalLatencyMS: latency,
			ThroughputQ3: throughputQ3, ErrorRatePPM: rate,
			AvgLatencyQ3: avg.Value,
			TolerancePPM: tolerancePPM, ConvertedValue: rateBackToPPM.Value,
			Saturated:       rateToMyriad.Saturated || rateBackToPPM.Saturated || avg.Saturated,
			WithinTolerance: WithinTolerancePPM(rateBackToPPM.Value, rate, tolerancePPM),
		}
		windows = append(windows, ws)
	}
	return windows
}
