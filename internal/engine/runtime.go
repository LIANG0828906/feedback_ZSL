package engine

import (
	"fmt"
	"sort"

	"grayrelease/internal/model"
)

const (
	CodeWorkerBudget      = "E_WORKER_BUDGET"
	CodeRouteBufferFull   = "E_ROUTE_BUFFER_FULL"
	CodeShadowBufferFull  = "E_SHADOW_BUFFER_FULL"
	CodeMirrorConcurrency = "E_MIRROR_CONCURRENCY_LIMIT"
)

func (c Config) Validate() error {
	if c.WorkerBudget <= 0 {
		return model.NewError(CodeWorkerBudget, "worker budget must be positive").With("worker_budget", c.WorkerBudget)
	}
	if c.RouteBufferPerService < 0 {
		return model.NewError(CodeRouteBufferFull, "route buffer cannot be negative").With("route_buffer_per_service", c.RouteBufferPerService)
	}
	if c.MirrorConcurrencyPerService <= 0 {
		return model.NewError(CodeMirrorConcurrency, "mirror concurrency must be positive").With("mirror_concurrency_per_service", c.MirrorConcurrencyPerService)
	}
	if c.ShadowBufferPerService < 0 {
		return model.NewError(CodeShadowBufferFull, "shadow buffer cannot be negative").With("shadow_buffer_per_service", c.ShadowBufferPerService)
	}
	if c.ShadowOnFull == "" {
		c.ShadowOnFull = "drop"
	}
	if c.ShadowOnFull != "drop" && c.ShadowOnFull != "defer" {
		return model.NewError("E_CONFIG_INVALID", "shadow_on_full must be drop or defer").With("value", c.ShadowOnFull)
	}
	return nil
}

type Call struct {
	CallID       string    `json:"call_id"`
	RequestID    string    `json:"request_id"`
	Service      string    `json:"service"`
	RuleID       string    `json:"rule_id"`
	Status       string    `json:"status"`
	EnqueuedTick int       `json:"enqueued_tick"`
	StartTick    int       `json:"start_tick,omitempty"`
	FinishTick   int       `json:"finish_tick,omitempty"`
	Outcome      string    `json:"outcome,omitempty"`
	Fault        string    `json:"fault,omitempty"`
	Decision     *Decision `json:"-"`
}

type BrokerState struct {
	Waiting []Call `json:"waiting"`
	Active  []Call `json:"active"`
	NextSeq int    `json:"next_seq"`
}

type Broker struct {
	service string
	config  Config
	waiting []*Call
	active  []*Call
	nextSeq int
}

func NewBroker(service string, config Config, state *BrokerState) *Broker {
	broker := &Broker{service: service, config: config, nextSeq: 1}
	if state != nil {
		broker.nextSeq = state.NextSeq
		for i := range state.Waiting {
			call := state.Waiting[i]
			broker.waiting = append(broker.waiting, &call)
		}
		for i := range state.Active {
			call := state.Active[i]
			broker.active = append(broker.active, &call)
		}
	}
	return broker
}

func (b *Broker) Complete(tick int) []string {
	completed := make([]string, 0)
	remaining := b.active[:0]
	for _, call := range b.active {
		if call.FinishTick <= tick {
			call.Status = "completed"
			if call.Decision != nil {
				call.Decision.ShadowStatus = "completed"
				call.Decision.ShadowOutcome = call.Outcome
			}
			completed = append(completed, call.RequestID+":"+call.Outcome)
			continue
		}
		remaining = append(remaining, call)
	}
	b.active = remaining
	return completed
}

func (b *Broker) Pump(tick int) []string {
	dispatched := make([]string, 0)
	for len(b.active) < b.config.MirrorConcurrencyPerService && len(b.waiting) > 0 {
		call := b.waiting[0]
		b.waiting = b.waiting[1:]
		b.start(call, tick)
		b.active = append(b.active, call)
		dispatched = append(dispatched, call.RequestID)
	}
	return dispatched
}

func (b *Broker) start(call *Call, tick int) {
	latency := b.config.ShadowLatencyTicks
	if latency <= 0 {
		latency = 1
	}
	call.StartTick = tick
	call.Status = "dispatched"
	switch call.Fault {
	case "timeout":
		timeout := b.config.ShadowTimeoutTicks
		if timeout <= 0 {
			timeout = 2
		}
		if timeout < latency {
			timeout = latency
		}
		call.Outcome = "timeout"
		call.FinishTick = tick + timeout
	case "reject", "rejected":
		call.Outcome = "rejected"
		call.FinishTick = tick + latency
	default:
		call.Outcome = "fulfilled"
		call.FinishTick = tick + latency
	}
	if call.Decision != nil {
		call.Decision.ShadowStatus = "dispatched"
		call.Decision.ShadowOutcome = ""
	}
}

func (b *Broker) Admit(tick int, decision *Decision, fault string) (*ConstraintViolation, string) {
	if !decision.MirrorSelected {
		return nil, ""
	}
	call := &Call{
		CallID:       fmt.Sprintf("shadow-%s-%06d", b.service, b.nextSeq),
		RequestID:    decision.RequestID,
		Service:      b.service,
		RuleID:       decision.MirrorRuleID,
		Status:       "selected",
		EnqueuedTick: tick,
		Fault:        fault,
		Decision:     decision,
	}
	b.nextSeq++

	if len(b.active) < b.config.MirrorConcurrencyPerService {
		b.start(call, tick)
		b.active = append(b.active, call)
		return nil, call.RequestID
	}
	if len(b.waiting) < b.config.ShadowBufferPerService {
		call.Status = "queued"
		b.waiting = append(b.waiting, call)
		decision.ShadowStatus = "queued"
		return nil, call.RequestID
	}

	constraint := fmt.Sprintf("service=%s;waiting=%d;buffer_capacity=%d;active=%d;concurrency_limit=%d",
		b.service, len(b.waiting), b.config.ShadowBufferPerService, len(b.active), b.config.MirrorConcurrencyPerService)
	decision.Constraint = constraint
	decision.ShadowCode = CodeShadowBufferFull
	violation := &ConstraintViolation{
		Tick: tick, Service: b.service, RequestID: decision.RequestID,
		Code: CodeShadowBufferFull, Constraint: constraint, Action: b.config.ShadowOnFull,
	}
	if b.config.ShadowOnFull == "defer" {
		decision.ShadowStatus = "deferred"
		return violation, ""
	}
	decision.ShadowStatus = "dropped"
	return violation, ""
}

func (b *Broker) State() BrokerState {
	state := BrokerState{NextSeq: b.nextSeq}
	for _, call := range b.waiting {
		state.Waiting = append(state.Waiting, *call)
	}
	for _, call := range b.active {
		state.Active = append(state.Active, *call)
	}
	return state
}

type Runtime struct {
	project *model.Project
	config  Config
	brokers map[string]*Broker
	states  map[string]BrokerState
}

func NewRuntime(project *model.Project, config Config, initial map[string]BrokerState) (*Runtime, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	runtime := &Runtime{project: project, config: config, brokers: map[string]*Broker{}, states: map[string]BrokerState{}}
	for _, service := range project.Services {
		var state *BrokerState
		if initial != nil {
			if loaded, ok := initial[service.Name]; ok {
				loadedCopy := loaded
				state = &loadedCopy
			}
		}
		runtime.brokers[service.Name] = NewBroker(service.Name, config, state)
	}
	return runtime, nil
}

func (r *Runtime) StateSnapshots() map[string]BrokerState {
	out := map[string]BrokerState{}
	for service, broker := range r.brokers {
		out[service] = broker.State()
	}
	return out
}

func (r *Runtime) ProcessTick(tick int, requests []model.Request) (TickReport, []*Decision, error) {
	report := TickReport{
		Tick: tick, QueueOccupancy: map[string]int{}, ActiveOccupancy: map[string]int{},
		Services: map[string]ServiceTickReport{}, CacheState: "fresh",
	}
	pointers := make([]*Decision, 0, len(requests))

	byService := map[string][]model.Request{}
	for _, req := range requests {
		byService[req.Service] = append(byService[req.Service], req)
	}
	services := make([]string, 0, len(r.project.Services))
	for _, service := range r.project.Services {
		services = append(services, service.Name)
	}
	for service := range byService {
		if _, ok := r.brokers[service]; !ok {
			return TickReport{}, nil, model.NewError(CodeServiceMissingRuntime, "request service has no shadow broker").With("service", service)
		}
	}
	sort.Strings(services)

	for _, service := range services {
		reqs := byService[service]
		if reqs != nil {
			sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].Sequence < reqs[j].Sequence })
		}
		broker := r.brokers[service]
		serviceReport := ServiceTickReport{Tick: tick, Service: service, CacheState: "fresh"}
		serviceReport.Completed = append(serviceReport.Completed, broker.Complete(tick)...)
		serviceReport.Dispatched = append(serviceReport.Dispatched, broker.Pump(tick)...)

		for _, req := range reqs {
			decision, err := Route(r.project, req)
			if err != nil {
				return TickReport{}, nil, err
			}
			pointer := &decision
			if pointer.MirrorSelected {
				serviceReport.MirrorSelected++
				violation, dispatchedID := broker.Admit(tick, pointer, req.ShadowFault)
				if dispatchedID != "" {
					serviceReport.Dispatched = append(serviceReport.Dispatched, dispatchedID)
				}
				if violation != nil {
					serviceReport.Violations = append(serviceReport.Violations, *violation)
					report.Violations = append(report.Violations, *violation)
					if pointer.ShadowStatus == "dropped" {
						serviceReport.Dropped = append(serviceReport.Dropped, pointer.RequestID)
						report.DroppedSet = append(report.DroppedSet, pointer.RequestID)
					} else {
						serviceReport.Deferred = append(serviceReport.Deferred, pointer.RequestID)
						report.DeferredSet = append(report.DeferredSet, pointer.RequestID)
					}
				}
			}
			if pointer.MainAllowed {
				serviceReport.MainAllowed++
				if pointer.Side == model.SideCanary {
					serviceReport.CanaryCount++
				} else {
					serviceReport.BaselineCount++
				}
			}
			pointers = append(pointers, pointer)
		}

		state := broker.State()
		serviceReport.QueueOccupancy = len(state.Waiting)
		serviceReport.ActiveOccupancy = len(state.Active)
		report.QueueOccupancy[service] = serviceReport.QueueOccupancy
		report.ActiveOccupancy[service] = serviceReport.ActiveOccupancy
		report.MainAllowed += serviceReport.MainAllowed
		report.BaselineCount += serviceReport.BaselineCount
		report.CanaryCount += serviceReport.CanaryCount
		report.MirrorSelected += serviceReport.MirrorSelected
		report.Services[service] = serviceReport
	}

	sort.Strings(report.DroppedSet)
	sort.Strings(report.DeferredSet)
	sort.SliceStable(report.Violations, func(i, j int) bool {
		if report.Violations[i].Service != report.Violations[j].Service {
			return report.Violations[i].Service < report.Violations[j].Service
		}
		return report.Violations[i].RequestID < report.Violations[j].RequestID
	})
	return report, pointers, nil
}

const CodeServiceMissingRuntime = "E_SERVICE_MISSING"

func (r *Runtime) ProcessServiceTick(service string, tick int, requests []model.Request) (ServiceTickReport, []*Decision, error) {
	broker, ok := r.brokers[service]
	if !ok {
		return ServiceTickReport{}, nil, model.NewError(CodeServiceMissingRuntime, "no runtime broker for service").With("service", service)
	}
	reqs := append([]model.Request{}, requests...)
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].Sequence < reqs[j].Sequence })
	serviceReport := ServiceTickReport{Tick: tick, Service: service, CacheState: "fresh"}
	serviceReport.Completed = append(serviceReport.Completed, broker.Complete(tick)...)
	serviceReport.Dispatched = append(serviceReport.Dispatched, broker.Pump(tick)...)
	pointers := make([]*Decision, 0, len(reqs))

	for _, req := range reqs {
		decision, err := Route(r.project, req)
		if err != nil {
			return ServiceTickReport{}, nil, err
		}
		pointer := &decision
		if pointer.MirrorSelected {
			serviceReport.MirrorSelected++
			violation, dispatchedID := broker.Admit(tick, pointer, req.ShadowFault)
			if dispatchedID != "" {
				serviceReport.Dispatched = append(serviceReport.Dispatched, dispatchedID)
			}
			if violation != nil {
				serviceReport.Violations = append(serviceReport.Violations, *violation)
				if pointer.ShadowStatus == "dropped" {
					serviceReport.Dropped = append(serviceReport.Dropped, pointer.RequestID)
				} else {
					serviceReport.Deferred = append(serviceReport.Deferred, pointer.RequestID)
				}
			}
		}
		if pointer.MainAllowed {
			serviceReport.MainAllowed++
			if pointer.Side == model.SideCanary {
				serviceReport.CanaryCount++
			} else {
				serviceReport.BaselineCount++
			}
		}
		pointers = append(pointers, pointer)
	}
	state := broker.State()
	serviceReport.QueueOccupancy = len(state.Waiting)
	serviceReport.ActiveOccupancy = len(state.Active)
	return serviceReport, pointers, nil
}

func cloneLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	out := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		out[key] = value
	}
	return out
}
