package slikernel

import (
	"context"
	"fmt"
	"math"
	"sort"
)

type EditKind string

const (
	EditSetSLOTarget        EditKind = "set_slo_target"
	EditSetWindowLength     EditKind = "set_window_length"
	EditSetLatencyThreshold EditKind = "set_latency_threshold"
	EditSetQuantile         EditKind = "set_quantile"
	EditSetRollingWindows   EditKind = "set_rolling_windows"
	EditSetThresholds       EditKind = "set_thresholds"
	EditAppendObservations  EditKind = "append_observations"
)

type Edit struct {
	ID                 string        `json:"id"`
	Kind               EditKind      `json:"kind"`
	Service            string        `json:"service,omitempty"`
	SLI                string        `json:"sli,omitempty"`
	TargetPPM          *int64        `json:"target_ppm,omitempty"`
	WindowMs           *int64        `json:"window_ms,omitempty"`
	LatencyThresholdUs *int64        `json:"latency_threshold_us,omitempty"`
	QuantilePPM        *int64        `json:"quantile_ppm,omitempty"`
	RollingWindows     *int          `json:"rolling_windows,omitempty"`
	Thresholds         *[]Threshold  `json:"thresholds,omitempty"`
	Observations       []Observation `json:"observations,omitempty"`
}

type DependencyNode struct {
	Stage   string `json:"stage"`
	Service string `json:"service"`
	SLI     string `json:"sli"`
	Index   int64  `json:"index"`
	Key     string `json:"key"`
}

type InvalidationSource struct {
	EditID              string           `json:"edit_id"`
	Kind                EditKind         `json:"kind"`
	Service             string           `json:"service,omitempty"`
	SLI                 string           `json:"sli,omitempty"`
	Reason              string           `json:"reason"`
	AffectedBaseWindows []int64          `json:"affected_base_windows,omitempty"`
	AffectedEvalWindows []int64          `json:"affected_eval_windows,omitempty"`
	PropagationClosure  []DependencyNode `json:"propagation_closure"`

	evalKeys []resultCacheKey
}

type InvalidationReport struct {
	Report                *Report              `json:"report"`
	RuleVersion           string               `json:"rule_version"`
	Sources               []InvalidationSource `json:"sources"`
	RecomputedEval        []ResultKey          `json:"recomputed_eval"`
	ReusedEval            []ResultKey          `json:"reused_eval"`
	RecomputedAlerts      []ResultKey          `json:"recomputed_alerts"`
	ReusedBaseWindows     int                  `json:"reused_base_windows"`
	RecomputedBaseWindows int                  `json:"recomputed_base_windows"`
	Diagnostics           Diagnostics          `json:"diagnostics,omitempty"`
}

type invalidationPlan struct {
	fullAgg    map[sliIdentity]bool
	dirtyBase  map[sliIdentity]map[int64]bool
	fullEval   map[sliIdentity]bool
	dirtyEval  map[sliIdentity]map[int64]bool
	alertFrom  map[sliIdentity]int64
	directKeys map[resultCacheKey][]string
	alertIDs   map[sliIdentity][]string
	sources    []InvalidationSource
}

func newInvalidationPlan() *invalidationPlan {
	return &invalidationPlan{
		fullAgg: make(map[sliIdentity]bool), dirtyBase: make(map[sliIdentity]map[int64]bool),
		fullEval: make(map[sliIdentity]bool), dirtyEval: make(map[sliIdentity]map[int64]bool),
		alertFrom: make(map[sliIdentity]int64), directKeys: make(map[resultCacheKey][]string),
		alertIDs: make(map[sliIdentity][]string),
	}
}

func (plan *invalidationPlan) addSource(
	edit Edit, project Project, serviceName string, sli SLI,
	fullAgg, fullEval, markEvalBudget bool, baseIndices []int64, evalIndices []int64, reason string,
) {
	identity := sliIdentity{Service: serviceName, SLI: sli.ID}
	revision := SLIAggregationRevision(serviceName, sli)
	if fullAgg {
		plan.fullAgg[identity] = true
	}
	if fullEval {
		plan.fullEval[identity] = true
	}
	if len(baseIndices) > 0 {
		if plan.dirtyBase[identity] == nil {
			plan.dirtyBase[identity] = make(map[int64]bool)
		}
		for _, index := range baseIndices {
			plan.dirtyBase[identity][index] = true
		}
	}
	if markEvalBudget && len(evalIndices) > 0 {
		if plan.dirtyEval[identity] == nil {
			plan.dirtyEval[identity] = make(map[int64]bool)
		}
		for _, index := range evalIndices {
			plan.dirtyEval[identity][index] = true
		}
	}
	firstAlert := int64(math.MaxInt64)
	if len(evalIndices) > 0 {
		firstAlert = evalIndices[0]
	}
	if current, ok := plan.alertFrom[identity]; !ok || firstAlert < current {
		plan.alertFrom[identity] = firstAlert
	}
	for _, index := range evalIndices {
		key := resultCacheKey{Service: serviceName, SLI: sli.ID, Revision: revision, Index: index}
		plan.directKeys[key] = appendUnique(plan.directKeys[key], edit.ID)
	}
	plan.alertIDs[identity] = appendUnique(plan.alertIDs[identity], edit.ID)
	source := InvalidationSource{
		EditID: edit.ID, Kind: edit.Kind, Service: serviceName, SLI: sli.ID, Reason: reason,
		AffectedBaseWindows: append([]int64(nil), baseIndices...),
		AffectedEvalWindows: append([]int64(nil), evalIndices...),
	}
	source.evalKeys = make([]resultCacheKey, 0, len(evalIndices))
	for _, index := range evalIndices {
		source.evalKeys = append(source.evalKeys, resultCacheKey{
			Service: serviceName, SLI: sli.ID, Revision: revision, Index: index,
		})
	}
	closure := make(map[string]DependencyNode)
	for _, index := range baseIndices {
		key := fmt.Sprintf("aggregate:%s:%s:%d", serviceName, sli.ID, index)
		closure[key] = DependencyNode{
			Stage: "aggregate", Service: serviceName, SLI: sli.ID, Index: index, Key: key,
		}
	}
	for _, index := range evalIndices {
		key := fmt.Sprintf("evaluate:%s:%s:%d", serviceName, sli.ID, index)
		closure[key] = DependencyNode{
			Stage: "evaluate", Service: serviceName, SLI: sli.ID, Index: index, Key: key,
		}
	}
	allEvalIndices, _ := evaluationIndices(project, sli)
	for _, index := range allEvalIndices {
		if index < firstAlert {
			continue
		}
		alertKey := fmt.Sprintf("alert:%s:%s:%d", serviceName, sli.ID, index)
		closure[alertKey] = DependencyNode{
			Stage: "alert", Service: serviceName, SLI: sli.ID, Index: index, Key: alertKey,
		}
	}
	keys := make([]string, 0, len(closure))
	for key := range closure {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		source.PropagationClosure = append(source.PropagationClosure, closure[key])
	}
	plan.sources = append(plan.sources, source)
}

func (engine *Engine) ApplyEdits(execCtx context.Context, edits []Edit) (*InvalidationReport, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(edits) == 0 {
		report := engine.buildReport(engine.results)
		return &InvalidationReport{Report: report, RuleVersion: engine.ruleVersion}, nil
	}
	normalized := make([]Edit, len(edits))
	seenIDs := make(map[string]struct{}, len(edits))
	for index, edit := range edits {
		if edit.ID == "" {
			edit.ID = fmt.Sprintf("edit-%d", index+1)
		}
		if _, exists := seenIDs[edit.ID]; exists {
			return nil, biz(CodeEdit, "apply_edits", nil, ctx("edit_id", edit.ID))
		}
		seenIDs[edit.ID] = struct{}{}
		normalized[index] = edit
	}
	newProject := CloneProject(engine.project)
	for _, edit := range normalized {
		if err := applyEdit(&newProject, edit); err != nil {
			return nil, err
		}
	}
	if err := ValidateProject(newProject); err != nil {
		return nil, err
	}

	plan := newInvalidationPlan()
	for _, edit := range normalized {
		if err := buildInvalidation(plan, newProject, edit); err != nil {
			return nil, err
		}
	}
	newRuleVersion := ProjectRuleVersion(newProject)

	allBase, err := allBaseTasks(newProject)
	if err != nil {
		return nil, err
	}
	newBaseCache := make(map[baseCacheKey]*BaseAgg, len(allBase))
	var baseToCompute []baseTask
	reusedBase := 0
	for _, task := range allBase {
		identity := sliIdentity{Service: task.service, SLI: task.sli.ID}
		key := task.key()
		if cached := engine.baseCache[key]; cached != nil &&
			!plan.fullAgg[identity] && !plan.dirtyBase[identity][task.index] {
			newBaseCache[key] = cached
			reusedBase++
			continue
		}
		baseToCompute = append(baseToCompute, task)
	}
	for index := range baseToCompute {
		baseToCompute[index].position = index
	}
	baseOutput, err := engine.runBaseTasks(execCtx, "aggregate", baseToCompute, func(task baseTask) (*BaseAgg, error) {
		return aggregateBase(newProject, task), nil
	})
	if err != nil {
		return nil, err
	}
	for index, task := range baseToCompute {
		newBaseCache[task.key()] = baseOutput.results[index]
	}

	allEval, err := allEvalTasks(newProject)
	if err != nil {
		return nil, err
	}
	newResults := make(map[resultCacheKey]*WindowResult, len(allEval))
	computedEvalKeys := make(map[resultCacheKey]bool)
	var evalToCompute []evalTask
	var reusedEval []ResultKey
	for _, task := range allEval {
		identity := sliIdentity{Service: task.service, SLI: task.sli.ID}
		key := task.cacheKey()
		if cached := engine.results[key]; cached != nil &&
			!plan.fullEval[identity] && !plan.dirtyEval[identity][task.evalIndex] {
			reused := *cached
			newResults[key] = &reused
			reusedEval = append(reusedEval, task.key())
			continue
		}
		evalToCompute = append(evalToCompute, task)
	}
	for index := range evalToCompute {
		evalToCompute[index].position = index
	}
	evalOutput, err := engine.runEvalTasks(execCtx, "evaluate", evalToCompute, newBaseCache, func(task evalTask) (*WindowResult, error) {
		return composeEvaluation(newProject, task, newBaseCache)
	})
	if err != nil {
		return nil, err
	}
	recomputedEval := make([]ResultKey, 0, len(evalToCompute))
	for index, task := range evalToCompute {
		result := evalOutput.results[index]
		key := task.cacheKey()
		newResults[key] = result
		computedEvalKeys[key] = true
		recomputedEval = append(recomputedEval, task.key())
	}

	provenance := make(map[resultCacheKey][]string)
	for key, ids := range plan.directKeys {
		provenance[key] = appendUnique(provenance[key], ids...)
	}
	for _, service := range newProject.Services {
		for _, sli := range service.SLIs {
			identity := sliIdentity{Service: service.Name, SLI: sli.ID}
			from, hasAlert := plan.alertFrom[identity]
			if !hasAlert || from == math.MaxInt64 {
				continue
			}
			indices, _ := evaluationIndices(newProject, sli)
			for _, evalIndex := range indices {
				if evalIndex < from {
					continue
				}
				key := resultCacheKey{
					Service: service.Name, SLI: sli.ID,
					Revision: SLIAggregationRevision(service.Name, sli), Index: evalIndex,
				}
				provenance[key] = appendUnique(provenance[key], plan.alertIDs[identity]...)
			}
		}
	}

	recomputedAlerts := make([]ResultKey, 0)
	for _, service := range newProject.Services {
		for _, sli := range service.SLIs {
			identity := sliIdentity{Service: service.Name, SLI: sli.ID}
			from, ok := plan.alertFrom[identity]
			if !ok || from == math.MaxInt64 {
				continue
			}
			revision := SLIAggregationRevision(service.Name, sli)
			indices, _ := evaluationIndices(newProject, sli)
			for _, evalIndex := range indices {
				if evalIndex >= from {
					recomputedAlerts = append(recomputedAlerts, ResultKey{
						Service: service.Name, SLI: sli.ID, EvalIndex: evalIndex,
					})
				}
			}
			applyAlertsFrom(newProject, service.Name, sli, revision, newResults, from, provenance)
		}
	}

	for _, service := range newProject.Services {
		for _, sli := range service.SLIs {
			identity := sliIdentity{Service: service.Name, SLI: sli.ID}
			revision := SLIAggregationRevision(service.Name, sli)
			indices, _ := evaluationIndices(newProject, sli)
			from, hasAlert := plan.alertFrom[identity]
			for _, evalIndex := range indices {
				key := resultCacheKey{Service: service.Name, SLI: sli.ID, Revision: revision, Index: evalIndex}
				result := newResults[key]
				if result == nil {
					continue
				}
				aggregationDirty := plan.fullAgg[identity]
				firstBase := evalIndex - int64(sli.RollingWindows) + 1
				for baseIndex := firstBase; baseIndex <= evalIndex && !aggregationDirty; baseIndex++ {
					if plan.dirtyBase[identity][baseIndex] {
						aggregationDirty = true
					}
				}
				budgetComputed := computedEvalKeys[key]
				alertComputed := hasAlert && evalIndex >= from
				closure := make(map[string]DependencyNode)
				for baseIndex := firstBase; baseIndex <= evalIndex; baseIndex++ {
					if plan.fullAgg[identity] || plan.dirtyBase[identity][baseIndex] {
						nodeKey := fmt.Sprintf("aggregate:%s:%s:%d", service.Name, sli.ID, baseIndex)
						closure[nodeKey] = DependencyNode{
							Stage: "aggregate", Service: service.Name, SLI: sli.ID,
							Index: baseIndex, Key: nodeKey,
						}
					}
				}
				if budgetComputed {
					nodeKey := fmt.Sprintf("evaluate:%s:%s:%d", service.Name, sli.ID, evalIndex)
					closure[nodeKey] = DependencyNode{
						Stage: "evaluate", Service: service.Name, SLI: sli.ID,
						Index: evalIndex, Key: nodeKey,
					}
				}
				if alertComputed {
					nodeKey := fmt.Sprintf("alert:%s:%s:%d", service.Name, sli.ID, evalIndex)
					closure[nodeKey] = DependencyNode{
						Stage: "alert", Service: service.Name, SLI: sli.ID,
						Index: evalIndex, Key: nodeKey,
					}
				}
				closureKeys := make([]string, 0, len(closure))
				for closureKey := range closure {
					closureKeys = append(closureKeys, closureKey)
				}
				sort.Strings(closureKeys)
				nodes := make([]DependencyNode, 0, len(closureKeys))
				for _, closureKey := range closureKeys {
					nodes = append(nodes, closure[closureKey])
				}
				result.Provenance = Provenance{
					InvalidationIDs:      append([]string(nil), provenance[key]...),
					PropagationClosure:   nodes,
					AggregationFromCache: !aggregationDirty,
					BudgetFromCache:      !budgetComputed,
					AlertFromCache:       !alertComputed,
					FromCache:            !aggregationDirty && !budgetComputed && !alertComputed,
				}
			}
		}
	}

	report := buildReportFor(newProject, newRuleVersion, newResults)
	report.Diagnostics.Backpressure = append(baseOutput.signals, evalOutput.signals...)
	report.Diagnostics.Durations = append(
		baseWindowDurations(baseToCompute, baseOutput.durations, "aggregate"),
		evalWindowDurations(evalToCompute, evalOutput.durations, "evaluate")...,
	)
	report.Diagnostics.Recomputed = append(report.Diagnostics.Recomputed, recomputedWindowDiagnostics(recomputedEval)...)
	report.Diagnostics.Reused = append(report.Diagnostics.Reused, reusedWindowDiagnostics(reusedEval)...)

	engine.project = newProject
	engine.ruleVersion = newRuleVersion
	engine.baseCache = newBaseCache
	engine.results = newResults

	return &InvalidationReport{
		Report: report, RuleVersion: newRuleVersion, Sources: plan.sources,
		RecomputedEval: recomputedEval, ReusedEval: reusedEval, RecomputedAlerts: recomputedAlerts,
		ReusedBaseWindows: reusedBase, RecomputedBaseWindows: len(baseToCompute),
		Diagnostics: report.Diagnostics,
	}, nil
}

func applyEdit(project *Project, edit Edit) error {
	if edit.Kind == EditAppendObservations {
		if len(edit.Observations) == 0 {
			return biz(CodeEdit, "apply_edit", nil, ctx("edit_id", edit.ID))
		}
		for _, observation := range edit.Observations {
			copied := observation
			if copied.Service == "" {
				copied.Service = edit.Service
			}
			copied.LatencySamplesUs = append([]int64(nil), observation.LatencySamplesUs...)
			project.Observations = append(project.Observations, copied)
		}
		return nil
	}
	sli, ok := project.FindSLI(edit.Service, edit.SLI)
	if !ok {
		return biz(CodeEdit, "apply_edit", nil,
			ctx3("edit_id", edit.ID, "service", edit.Service, "sli", edit.SLI))
	}
	switch edit.Kind {
	case EditSetSLOTarget:
		if edit.TargetPPM == nil {
			return biz(CodeEdit, "apply_edit", nil, ctx2("edit_id", edit.ID, "field", "target_ppm"))
		}
		if *edit.TargetPPM < 0 || *edit.TargetPPM > PPM {
			return biz(CodeRatioRange, "apply_edit", nil, ctx("edit_id", edit.ID))
		}
		sli.TargetPPM = *edit.TargetPPM
	case EditSetWindowLength:
		if edit.WindowMs == nil {
			return biz(CodeEdit, "apply_edit", nil, ctx2("edit_id", edit.ID, "field", "window_ms"))
		}
		if *edit.WindowMs <= 0 || *edit.WindowMs > MaxWindowMs {
			return biz(CodeWindowLength, "apply_edit", nil, ctx("edit_id", edit.ID))
		}
		sli.WindowMs = *edit.WindowMs
	case EditSetLatencyThreshold:
		if edit.LatencyThresholdUs == nil {
			return biz(CodeEdit, "apply_edit", nil, ctx2("edit_id", edit.ID, "field", "latency_threshold_us"))
		}
		if *edit.LatencyThresholdUs <= 0 {
			return biz(CodeEdit, "apply_edit", nil, ctx("edit_id", edit.ID))
		}
		sli.LatencyThresholdUs = *edit.LatencyThresholdUs
	case EditSetQuantile:
		if edit.QuantilePPM == nil {
			return biz(CodeEdit, "apply_edit", nil, ctx2("edit_id", edit.ID, "field", "quantile_ppm"))
		}
		if *edit.QuantilePPM <= 0 || *edit.QuantilePPM >= PPM {
			return biz(CodeQuantileRange, "apply_edit", nil, ctx("edit_id", edit.ID))
		}
		sli.QuantilePPM = *edit.QuantilePPM
	case EditSetRollingWindows:
		if edit.RollingWindows == nil {
			return biz(CodeEdit, "apply_edit", nil, ctx2("edit_id", edit.ID, "field", "rolling_windows"))
		}
		if *edit.RollingWindows < 1 || *edit.RollingWindows > MaxRollingWindows {
			return biz(CodeWindowLength, "apply_edit", nil, ctx("edit_id", edit.ID))
		}
		sli.RollingWindows = *edit.RollingWindows
	case EditSetThresholds:
		if edit.Thresholds == nil {
			return biz(CodeEdit, "apply_edit", nil, ctx2("edit_id", edit.ID, "field", "thresholds"))
		}
		sli.Thresholds = append([]Threshold(nil), (*edit.Thresholds)...)
	default:
		return biz(CodeEdit, "apply_edit", nil, ctx("kind", edit.Kind))
	}
	return nil
}

func buildInvalidation(plan *invalidationPlan, project Project, edit Edit) error {
	if edit.Kind == EditAppendObservations {
		return buildAppendInvalidation(plan, project, edit)
	}
	sli, ok := project.FindSLI(edit.Service, edit.SLI)
	if !ok {
		return biz(CodeEdit, "build_invalidation", nil,
			ctx3("edit_id", edit.ID, "service", edit.Service, "sli", edit.SLI))
	}
	evalIndices, err := evaluationIndices(project, *sli)
	if err != nil {
		return err
	}
	baseStart, baseEnd, err := baseWindowRange(project, *sli)
	if err != nil {
		return err
	}
	baseIndices := make([]int64, 0, baseEnd-baseStart)
	for index := baseStart; index < baseEnd; index++ {
		baseIndices = append(baseIndices, index)
	}
	switch edit.Kind {
	case EditSetSLOTarget:
		plan.addSource(edit, project, edit.Service, *sli, false, true, false, nil, evalIndices, "SLO target changed")
	case EditSetWindowLength:
		plan.addSource(edit, project, edit.Service, *sli, true, true, true, baseIndices, evalIndices, "aggregation window length changed")
	case EditSetLatencyThreshold, EditSetQuantile:
		plan.addSource(edit, project, edit.Service, *sli, true, true, true, baseIndices, evalIndices, "latency aggregation rule changed")
	case EditSetRollingWindows:
		plan.addSource(edit, project, edit.Service, *sli, false, true, false, nil, evalIndices, "SLO rolling window changed")
	case EditSetThresholds:
		plan.addSource(edit, project, edit.Service, *sli, false, false, false, nil, evalIndices, "alert thresholds changed")
	default:
		return biz(CodeEdit, "build_invalidation", nil, ctx("kind", edit.Kind))
	}
	return nil
}

func buildAppendInvalidation(plan *invalidationPlan, project Project, edit Edit) error {
	type affected struct {
		base map[int64]bool
	}
	affectedBySLI := make(map[sliIdentity]*affected)
	for _, observation := range edit.Observations {
		service, ok := project.FindService(observation.Service)
		if !ok {
			return biz(CodeEdit, "build_invalidation", nil,
				ctx2("edit_id", edit.ID, "service", observation.Service))
		}
		for _, sli := range service.SLIs {
			if !routeMatches(sli.Route, observation.Route) || !statusMatches(sli.StatusCodes, observation.StatusCode) {
				continue
			}
			identity := sliIdentity{Service: service.Name, SLI: sli.ID}
			entry := affectedBySLI[identity]
			if entry == nil {
				entry = &affected{base: make(map[int64]bool)}
				affectedBySLI[identity] = entry
			}
			if observation.TimestampMs < project.EvaluationStartMs ||
				observation.TimestampMs >= project.EvaluationEndMs {
				return biz(CodeObservationRange, "build_invalidation", nil,
					ctx2("edit_id", edit.ID, "seq", observation.Seq))
			}
			index := (observation.TimestampMs - project.TimeOriginMs) / sli.WindowMs
			entry.base[index] = true
		}
	}
	identities := make([]sliIdentity, 0, len(affectedBySLI))
	for identity := range affectedBySLI {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Service != identities[j].Service {
			return identities[i].Service < identities[j].Service
		}
		return identities[i].SLI < identities[j].SLI
	})
	for _, identity := range identities {
		service, _ := project.FindService(identity.Service)
		var sli *SLI
		for index := range service.SLIs {
			if service.SLIs[index].ID == identity.SLI {
				sli = &service.SLIs[index]
				break
			}
		}
		if sli == nil {
			return biz(CodeEdit, "build_invalidation", nil, ctx("sli", identity.SLI))
		}
		baseSet := affectedBySLI[identity].base
		var bases []int64
		for index := range baseSet {
			bases = append(bases, index)
		}
		sort.Slice(bases, func(i, j int) bool { return bases[i] < bases[j] })
		evalIndices, err := evaluationIndices(project, *sli)
		if err != nil {
			return err
		}
		var affectedEval []int64
		for _, evalIndex := range evalIndices {
			first := evalIndex - int64(sli.RollingWindows) + 1
			for _, baseIndex := range bases {
				if baseIndex >= first && baseIndex <= evalIndex {
					affectedEval = append(affectedEval, evalIndex)
					break
				}
			}
		}
		plan.addSource(edit, project, identity.Service, *sli, false, false, true, bases, affectedEval,
			"observations appended in affected base window")
	}
	return nil
}

func buildReportFor(project Project, ruleVersion string, results map[resultCacheKey]*WindowResult) *Report {
	report := &Report{
		ProjectID: project.ProjectID, RuleVersion: ruleVersion,
		CommittedObservationSeq: project.LastObservationSeq(),
	}
	for _, service := range project.Services {
		for _, sli := range service.SLIs {
			indices, _ := evaluationIndices(project, sli)
			revision := SLIAggregationRevision(service.Name, sli)
			for _, index := range indices {
				key := resultCacheKey{Service: service.Name, SLI: sli.ID, Revision: revision, Index: index}
				if result := results[key]; result != nil {
					report.Results = append(report.Results, *result)
				}
			}
		}
	}
	return report
}

func recomputedWindowDiagnostics(keys []ResultKey) []RecomputedWindow {
	output := make([]RecomputedWindow, 0, len(keys))
	for _, key := range keys {
		output = append(output, RecomputedWindow{
			Stage: "evaluate", Service: key.Service, SLI: key.SLI, Index: key.EvalIndex,
		})
	}
	return output
}

func reusedWindowDiagnostics(keys []ResultKey) []RecomputedWindow {
	output := make([]RecomputedWindow, 0, len(keys))
	for _, key := range keys {
		output = append(output, RecomputedWindow{
			Stage: "evaluate", Service: key.Service, SLI: key.SLI, Index: key.EvalIndex,
		})
	}
	return output
}
