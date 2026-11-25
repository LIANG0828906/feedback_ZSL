package durable

import (
	"sort"
	"syscall"

	"grayrelease/internal/checkpoint"
	"grayrelease/internal/engine"
	"grayrelease/internal/model"
)

const (
	StageAfterRoutes  = "after-routes"
	StageAfterQueues  = "after-queues"
	StageBeforeCommit = "before-commit"
	StageAfterCommit  = "after-commit"
)

type CrashPlan struct {
	Tick  int    `json:"tick"`
	Stage string `json:"stage"`
}

type RunReport struct {
	StartedAtTick  int                      `json:"started_at_tick"`
	EndedAtTick    int                      `json:"ended_at_tick"`
	ProcessedTicks []int                    `json:"processed_ticks"`
	TickReports    []engine.TickReport      `json:"tick_reports"`
	Decisions      []engine.Decision        `json:"decisions"`
	Recovery       *checkpoint.RecoveryData `json:"recovery,omitempty"`
}

type Runner struct {
	project  *model.Project
	requests []model.Request
	config   engine.Config
	store    *checkpoint.Store
}

func NewRunner(project *model.Project, requests []model.Request, config engine.Config, store *checkpoint.Store) *Runner {
	return &Runner{project: project, requests: requests, config: config, store: store}
}

func (r *Runner) RunUntil(endTick int, crash *CrashPlan) (*RunReport, error) {
	recovery, err := r.store.Recover()
	if err != nil {
		return nil, err
	}
	runtime, err := engine.NewRuntime(r.project, r.config, recovery.QueueStates)
	if err != nil {
		return nil, err
	}
	start := recovery.CommittedTick + 1
	report := &RunReport{StartedAtTick: start, Recovery: recovery}

	for tick := start; tick <= endTick; tick++ {
		requestsAtTick := make([]model.Request, 0)
		for _, req := range r.requests {
			if req.Tick == tick {
				requestsAtTick = append(requestsAtTick, req)
			}
		}
		tickReport, decisions, err := runtime.ProcessTick(tick, requestsAtTick)
		if err != nil {
			return nil, err
		}
		if err := r.store.WriteRouteMappings(tick, decisions); err != nil {
			return nil, err
		}
		if crash != nil && crash.Tick == tick && crash.Stage == StageAfterRoutes {
			KillProcess()
		}

		states := runtime.StateSnapshots()
		if err := r.store.WriteMirrorQueues(tick, states); err != nil {
			return nil, err
		}
		if crash != nil && crash.Tick == tick && (crash.Stage == StageAfterQueues || crash.Stage == StageBeforeCommit) {
			KillProcess()
		}

		if err := r.store.CommitTick(tick); err != nil {
			return nil, err
		}
		if crash != nil && crash.Tick == tick && crash.Stage == StageAfterCommit {
			KillProcess()
		}

		report.ProcessedTicks = append(report.ProcessedTicks, tick)
		report.TickReports = append(report.TickReports, tickReport)
		for _, decision := range decisions {
			report.Decisions = append(report.Decisions, *decision)
		}
	}
	report.EndedAtTick = endTick
	sort.SliceStable(report.Decisions, func(i, j int) bool {
		if report.Decisions[i].Tick != report.Decisions[j].Tick {
			return report.Decisions[i].Tick < report.Decisions[j].Tick
		}
		return report.Decisions[i].RequestID < report.Decisions[j].RequestID
	})
	return report, nil
}

func KillProcess() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	select {}
}
