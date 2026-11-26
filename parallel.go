package cachebench

import (
	"sort"
	"sync"
	"time"
)

type BackpressureEvent struct {
	Code      ErrorCode
	TickMS    int64
	NodeID    string
	RequestID string
	Message   string
}

type Dispatcher struct {
	cfg       Budget
	workerSem chan struct{}
	bufferSem chan struct{}

	mu     sync.Mutex
	Events []BackpressureEvent
	Errors []*DomainError
	NodeNS map[string]int64
}

func NewDispatcher(cfg Budget) (*Dispatcher, error) {
	if err := ValidateBudget(cfg); err != nil {
		return nil, err
	}
	return &Dispatcher{
		cfg: cfg, workerSem: make(chan struct{}, cfg.Workers),
		bufferSem: make(chan struct{}, cfg.Workers*cfg.BufferPerWorker),
		NodeNS:    map[string]int64{},
	}, nil
}

func (d *Dispatcher) recordError(err *DomainError) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Errors = append(d.Errors, err)
	d.Events = append(d.Events, BackpressureEvent{Code: err.Code, NodeID: err.NodeID, RequestID: err.Context["request_id"], Message: err.Message})
}

// AcquireWorker returns E_BACKPRESSURE_THREAD_BUDGET when the declared worker
// budget is already occupied. Callers may then perform a blocking send on the
// same semaphore to apply backpressure rather than allocate beyond budget.
func (d *Dispatcher) AcquireWorker(nodeID string) error {
	select {
	case d.workerSem <- struct{}{}:
		return nil
	default:
		err := NewError(CodeBackpressureThread, "all %d workers are busy", d.cfg.Workers).
			WithNode(nodeID).WithField("workers").
			WithContext("budget", itoa(d.cfg.Workers))
		d.recordError(err)
		return err
	}
}

func (d *Dispatcher) blockWorker()   { d.workerSem <- struct{}{} }
func (d *Dispatcher) ReleaseWorker() { <-d.workerSem }

func (d *Dispatcher) AcquireBuffer(requestID string) error {
	select {
	case d.bufferSem <- struct{}{}:
		return nil
	default:
		err := NewError(CodeBackpressureBuffer, "per-worker inflight buffer budget exhausted").
			WithField("bufferPerWorker").
			WithContext("capacity", itoa(d.cfg.Workers*d.cfg.BufferPerWorker)).
			WithContext("request_id", requestID)
		d.recordError(err)
		return err
	}
}

// AcquireBufferBlocking records the stable business code at the exhaustion
// edge and then waits for a completion buffer slot.
func (d *Dispatcher) AcquireBufferBlocking(requestID string) {
	if err := d.AcquireBuffer(requestID); err != nil {
		d.bufferSem <- struct{}{}
	}
}

func (d *Dispatcher) ReleaseBuffer() { <-d.bufferSem }

func (d *Dispatcher) addDuration(nodeID string, dur time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.NodeNS[nodeID] += int64(dur)
}

func itoa(v int) string {
	// Avoid strconv in this tiny hot path wrapper while keeping context stable.
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

type ParallelReport struct {
	Result          *SimulationResult
	Workers         int
	BufferPerWorker int
	Backpressure    []BackpressureEvent
	NodeComputeNS   map[string]int64
}

// ParallelSimulate advances node capacity shards with a bounded worker pool.
// Routing remains a stable ordered stage; node transitions are independent and
// are merged in canonical node/request order.
func ParallelSimulate(project Project, cfg Budget) (*ParallelReport, error) {
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	d, err := NewDispatcher(cfg)
	if err != nil {
		return nil, err
	}
	ids := nodeIDsSorted(project.Nodes)
	byID := map[string]Node{}
	rts := map[string]*nodeRuntime{}
	for i := range project.Nodes {
		n := project.Nodes[i]
		if n.DeclarationOrder == 0 {
			n.DeclarationOrder = i
		}
		byID[n.ID] = n
		rts[n.ID] = &nodeRuntime{node: n, highSince: -1, openSince: -1, due: map[int64][]completion{}}
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
	res := &SimulationResult{ProjectID: project.ProjectID, CheckpointVer: CheckpointSchemaVersion, FinalTick: horizon, NodeIDs: append([]string(nil), ids...)}
	applyPriorOverrides(project, 0, byID, rts)

	for tick := int64(0); tick <= horizon; tick++ {
		applyOverridesAt(project, tick, byID, rts)
		type preOut struct {
			nt       NodeTick
			recovery *BreakerEvent
			pre      *BreakerEvent
		}
		pre := make([]preOut, len(ids))
		runNodeStage(d, ids, func(i int, id string) {
			start := time.Now()
			rt := rts[id]
			out := preOut{nt: NodeTick{NodeID: id, TickMS: tick, HighSinceMS: rt.highSince, OpenSinceMS: rt.openSince}}
			for _, c := range rt.due[tick] {
				if rt.inflight > 0 {
					rt.inflight--
				}
				rt.recent = append(rt.recent, c)
				out.nt.Completed++
				out.nt.LatencyTotalMS += c.latencyMS
				if c.isError {
					out.nt.ErrorsCompleted++
				}
			}
			delete(rt.due, tick)
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
					rt.openSince = -1
					rt.highSince = -1
					out.recovery = &ev
				}
			}
			if !rt.open {
				out.pre = evaluateBreaker(rt, tick, project.WindowMS, false)
			}
			out.nt.Inflight = rt.inflight
			out.nt.CircuitOpen = rt.open
			out.nt.HighSinceMS = rt.highSince
			out.nt.OpenSinceMS = rt.openSince
			if rt.open {
				out.nt.RecoveryAtMS = rt.openSince + rt.node.RecoveryMS
			}
			pre[i] = out
			d.addDuration(id, time.Since(start))
		})

		tr := TickResult{TickMS: tick}
		preByNode := map[string]preOut{}
		for _, out := range pre {
			preByNode[out.nt.NodeID] = out
			if out.recovery != nil {
				tr.BreakerEvents = append(tr.BreakerEvents, *out.recovery)
			}
			if out.pre != nil {
				tr.BreakerEvents = append(tr.BreakerEvents, *out.pre)
			}
			tr.TotalThroughput += int64(out.nt.Completed)
		}

		admitGroups := map[string][]Request{}
		for _, req := range byArrival[tick] {
			candidateNodes, derr := resolveCandidates(req, project, byID)
			if derr != nil {
				return nil, derr
			}
			eligible := make([]Node, 0, len(candidateNodes))
			for _, n := range candidateNodes {
				rt := rts[n.ID]
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
				tr.Overflow = append(tr.Overflow, Overflow{RequestID: req.ID, NodeID: id, ConstraintID: constraint, TickMS: tick, ReasonCode: CodeNoEligibleNode, BasisID: "ROUTE-DENY-" + id})
				continue
			}
			decision := Route(req, eligible, project.RoutingPolicy)
			decision.TickMS = tick
			tr.Decisions = append(tr.Decisions, decision)
			admitGroups[decision.NodeID] = append(admitGroups[decision.NodeID], req)
		}

		type admitOut struct {
			nodeID    string
			accepted  int
			overflows []Overflow
			completed int
			errors    int
			latency   int64
		}
		admitOuts := make([]admitOut, 0, len(admitGroups))
		var admitMu sync.Mutex
		groupIDs := make([]string, 0, len(admitGroups))
		for id := range admitGroups {
			groupIDs = append(groupIDs, id)
		}
		sort.Strings(groupIDs)
		runNodeStage(d, groupIDs, func(_ int, id string) {
			start := time.Now()
			rt := rts[id]
			out := admitOut{nodeID: id}
			for _, req := range admitGroups[id] {
				d.AcquireBufferBlocking(req.ID)
				if rt.inflight >= rt.node.MaxInflight {
					out.overflows = append(out.overflows, Overflow{RequestID: req.ID, NodeID: id, ConstraintID: rt.node.ConstraintID, TickMS: tick, ReasonCode: CodeCapacityOverflow, BasisID: "CAPACITY-" + rt.node.ConstraintID})
					d.ReleaseBuffer()
					continue
				}
				rt.inflight++
				out.accepted++
				latency := req.LatencyMS
				if latency <= 0 {
					latency = rt.node.ServiceMS
				}
				c := completion{nodeID: id, requestID: req.ID, isError: req.IsError, latencyMS: latency, atMS: tick + rt.node.ServiceMS}
				if rt.node.ServiceMS == 0 {
					rt.inflight--
					rt.recent = append(rt.recent, c)
					out.completed++
					out.latency += latency
					if c.isError {
						out.errors++
					}
				} else {
					rt.due[c.atMS] = append(rt.due[c.atMS], c)
				}
				d.ReleaseBuffer()
			}
			admitMu.Lock()
			admitOuts = append(admitOuts, out)
			admitMu.Unlock()
			d.addDuration(id, time.Since(start))
		})

		admitByNode := map[string]admitOut{}
		for _, out := range admitOuts {
			admitByNode[out.nodeID] = out
			tr.Overflow = append(tr.Overflow, out.overflows...)
		}

		postEvents := make([]*BreakerEvent, len(ids))
		runNodeStage(d, ids, func(i int, id string) {
			if rts[id].open {
				return
			}
			postEvents[i] = evaluateBreaker(rts[id], tick, project.WindowMS, true)
		})
		for _, ev := range postEvents {
			if ev != nil {
				tr.BreakerEvents = append(tr.BreakerEvents, *ev)
			}
		}

		for _, id := range ids {
			rt := rts[id]
			base := preByNode[id].nt
			if ao, ok := admitByNode[id]; ok {
				base.Accepted = ao.accepted
				base.Completed += ao.completed
				base.ErrorsCompleted += ao.errors
				base.LatencyTotalMS += ao.latency
				tr.TotalThroughput += int64(ao.completed)
			}
			base.Overflow = 0
			for _, ov := range tr.Overflow {
				if ov.NodeID == id {
					base.Overflow++
				}
			}
			base.Inflight = rt.inflight
			base.CircuitOpen = rt.open
			base.HighSinceMS = rt.highSince
			base.OpenSinceMS = rt.openSince
			if rt.open {
				base.RecoveryAtMS = rt.openSince + rt.node.RecoveryMS
				tr.OpenCircuits = append(tr.OpenCircuits, id)
			}
			tr.Nodes = append(tr.Nodes, base)
		}
		sort.Slice(tr.Nodes, func(i, j int) bool { return tr.Nodes[i].NodeID < tr.Nodes[j].NodeID })
		sort.Strings(tr.OpenCircuits)
		sort.Slice(tr.Overflow, func(i, j int) bool {
			if tr.Overflow[i].TickMS != tr.Overflow[j].TickMS {
				return tr.Overflow[i].TickMS < tr.Overflow[j].TickMS
			}
			if tr.Overflow[i].NodeID != tr.Overflow[j].NodeID {
				return tr.Overflow[i].NodeID < tr.Overflow[j].NodeID
			}
			return tr.Overflow[i].RequestID < tr.Overflow[j].RequestID
		})
		res.Ticks = append(res.Ticks, tr)
	}
	res.Windows = BuildWindows(project, res)
	d.mu.Lock()
	backpressure := append([]BackpressureEvent(nil), d.Events...)
	nodeNS := make(map[string]int64, len(d.NodeNS))
	for k, v := range d.NodeNS {
		nodeNS[k] = v
	}
	d.mu.Unlock()
	return &ParallelReport{Result: res, Workers: cfg.Workers, BufferPerWorker: cfg.BufferPerWorker, Backpressure: backpressure, NodeComputeNS: nodeNS}, nil
}

func runNodeStage(d *Dispatcher, ids []string, fn func(i int, id string)) {
	var wg sync.WaitGroup
	for i, id := range ids {
		if err := d.AcquireWorker(id); err != nil {
			d.blockWorker()
		}
		wg.Add(1)
		go func(i int, id string) {
			defer d.ReleaseWorker()
			defer wg.Done()
			fn(i, id)
		}(i, id)
	}
	wg.Wait()
}
