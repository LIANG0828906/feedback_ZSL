package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"driftconverge/internal/compare"
	"driftconverge/internal/gitsource"
	"driftconverge/internal/kube"
	"driftconverge/internal/manifest"
	"driftconverge/internal/metrics"
	"driftconverge/internal/model"
	"driftconverge/internal/policy"
	"driftconverge/internal/store"
)

var ErrBusy = errors.New("a reconciliation round is already running")

type SnapshotReader interface {
	ReadSnapshot(ctx context.Context, branch string) (gitsource.SnapshotFiles, error)
}

type RoundStore interface {
	CommitRound(ctx context.Context, report model.RoundReport) error
	LastObservable(ctx context.Context, cluster string) (model.PreviousObservation, error)
}

type ClusterSpec struct {
	Name       string
	Branch     string
	Role       string
	Namespace  string
	Kubeconfig string
}

type Options struct {
	Clusters         []ClusterSpec
	Source           SnapshotReader
	Store            RoundStore
	Metrics          *metrics.Registry
	AdmissionTimeout time.Duration
	AdmissionPoll    time.Duration
	ClientFactory    func(cluster ClusterSpec) (kube.Client, error)
}

type Engine struct {
	options Options
	roundMu sync.Mutex
}

type loadedSnapshot struct {
	snapshot *model.Snapshot
	commit   string
	err      error
}

func New(options Options) (*Engine, error) {
	if len(options.Clusters) == 0 {
		return nil, fmt.Errorf("at least one cluster is required")
	}
	if options.Source == nil {
		return nil, fmt.Errorf("snapshot source is required")
	}
	if options.Store == nil {
		return nil, fmt.Errorf("round store is required")
	}
	if options.AdmissionTimeout <= 0 {
		options.AdmissionTimeout = 20 * time.Second
	}
	if options.AdmissionPoll <= 0 {
		options.AdmissionPoll = 1 * time.Second
	}
	if options.ClientFactory == nil {
		options.ClientFactory = func(cluster ClusterSpec) (kube.Client, error) {
			return kube.New(cluster.Kubeconfig)
		}
	}
	sort.Slice(options.Clusters, func(i, j int) bool {
		return options.Clusters[i].Name < options.Clusters[j].Name
	})
	return &Engine{options: options}, nil
}

func (e *Engine) RunRound(ctx context.Context) (model.RoundReport, error) {
	if !e.roundMu.TryLock() {
		return model.RoundReport{}, ErrBusy
	}
	defer e.roundMu.Unlock()

	started := time.Now().UTC()
	report := model.RoundReport{
		RoundID:   newRoundID(started),
		StartedAt: started,
		Clusters:  make([]model.ClusterReport, 0, len(e.options.Clusters)),
	}

	loaded := make(map[string]loadedSnapshot, len(e.options.Clusters))
	for _, cluster := range e.options.Clusters {
		loaded[cluster.Name] = e.load(ctx, cluster)
	}

	var devSnapshot *model.Snapshot
	for _, cluster := range e.options.Clusters {
		if roleOf(cluster) == "dev" && loaded[cluster.Name].err == nil {
			devSnapshot = loaded[cluster.Name].snapshot
		}
	}

	for _, cluster := range e.options.Clusters {
		item := loaded[cluster.Name]
		clusterReport := model.ClusterReport{
			Cluster:      cluster.Name,
			Branch:       cluster.Branch,
			GitCommit:    item.commit,
			Observable:   true,
			Differences:  []model.Difference{},
			PolicyBlocks: []model.PolicyBlock{},
			ApplyResults: []model.ApplyResult{},
		}
		if item.snapshot != nil {
			clusterReport.ExpectedCount = len(item.snapshot.Deployments)
		}
		if item.err != nil {
			applyBusinessError(&clusterReport, item.err)
			clusterReport.Convergence = model.ConclusionFailed
			report.Clusters = append(report.Clusters, clusterReport)
			continue
		}
		if roleOf(cluster) == "prod" && item.snapshot != nil && devSnapshot == nil {
			err := model.NewError(
				model.CodePolicy,
				"cannot validate prod snapshot because the dev policy snapshot is invalid or unavailable",
			)
			applyBusinessError(&clusterReport, err)
			clusterReport.Convergence = model.ConclusionFailed
			report.Clusters = append(report.Clusters, clusterReport)
			continue
		}
		if roleOf(cluster) == "prod" && item.snapshot != nil && devSnapshot != nil {
			blocks := policy.CheckProd(item.snapshot, policy.Build(devSnapshot, item.snapshot))
			clusterReport.PolicyBlocks = blocks
			clusterReport.PolicyBlockCount = len(blocks)
			if len(blocks) > 0 {
				err := &model.BusinessError{
					Code:         model.CodePolicy,
					Message:      fmt.Sprintf("prod snapshot rejected: %d dev-only field violation(s)", len(blocks)),
					ResourceKey:  blocks[0].ResourceKey,
					Field:        blocks[0].Field,
					SourceBranch: cluster.Branch,
				}
				applyBusinessError(&clusterReport, err)
				clusterReport.Convergence = model.ConclusionFailed
				report.Clusters = append(report.Clusters, clusterReport)
				continue
			}
		}
		e.reconcileCluster(ctx, cluster, item.snapshot, &clusterReport)
		report.Clusters = append(report.Clusters, clusterReport)
	}

	report.CommittedAt = time.Now().UTC()
	report.DurationMS = report.CommittedAt.Sub(started).Milliseconds()
	if report.DurationMS < 0 {
		report.DurationMS = 0
	}
	sort.Slice(report.Clusters, func(i, j int) bool {
		return report.Clusters[i].Cluster < report.Clusters[j].Cluster
	})

	commitErr := e.options.Store.CommitRound(ctx, report)
	for attempt := 0; errors.Is(commitErr, store.ErrRoundExists) && attempt < 2; attempt++ {
		report.RoundID = newRoundID(report.StartedAt.Add(time.Duration(attempt+1) * time.Nanosecond))
		commitErr = e.options.Store.CommitRound(ctx, report)
	}
	if commitErr != nil {
		return report, commitErr
	}
	if e.options.Metrics != nil {
		for _, cluster := range report.Clusters {
			e.options.Metrics.UpdateReport(cluster)
		}
	}
	return report, nil
}

func (e *Engine) load(ctx context.Context, cluster ClusterSpec) loadedSnapshot {
	files, err := e.options.Source.ReadSnapshot(ctx, cluster.Branch)
	if err != nil {
		return loadedSnapshot{err: err}
	}
	snapshot, parseErr := manifest.ParseSnapshot(cluster.Name, cluster.Branch, files.Commit, files.Files)
	if parseErr != nil {
		return loadedSnapshot{commit: files.Commit, err: parseErr}
	}
	for _, deployment := range snapshot.Deployments {
		if deployment.Namespace != cluster.Namespace {
			return loadedSnapshot{
				commit: files.Commit,
				err: &model.BusinessError{
					Code:         model.CodeManifest,
					Message:      fmt.Sprintf("Deployment namespace %s does not match target namespace %s", deployment.Namespace, cluster.Namespace),
					ResourceKey:  deployment.ResourceKey,
					Field:        "metadata.namespace",
					SourceBranch: cluster.Branch,
				},
			}
		}
	}
	return loadedSnapshot{snapshot: snapshot, commit: files.Commit}
}

func (e *Engine) reconcileCluster(
	ctx context.Context,
	cluster ClusterSpec,
	snapshot *model.Snapshot,
	report *model.ClusterReport,
) {
	client, err := e.options.ClientFactory(cluster)
	if err != nil {
		e.markUnobservable(ctx, cluster, report, fmt.Errorf("initialize cluster client: %w", err))
		return
	}
	actual, err := client.ListDeployments(ctx, cluster.Namespace)
	if err != nil {
		e.markUnobservable(ctx, cluster, report, err)
		return
	}
	report.ComparisonSuccess = true

	initial := compare.Deployments(snapshot.Deployments, actual)
	report.Differences = initial
	report.DiffCount = len(initial)
	if len(initial) == 0 {
		report.ConvergedResources = len(snapshot.Deployments)
		report.Convergence = model.ConclusionConverged
		return
	}

	desiredByKey := make(map[string]model.DesiredDeployment, len(snapshot.Deployments))
	for _, item := range snapshot.Deployments {
		desiredByKey[item.ResourceKey] = item
	}
	actualByName := make(map[string]model.DeploymentState, len(actual))
	for _, item := range actual {
		actualByName[item.Name] = item
	}

	keys := compare.ResourceKeys(initial)
	applied := make(map[string]bool, len(keys))
	blockedResources := make(map[string]bool)
	failedResources := make(map[string]string)
	applyResults := make([]model.ApplyResult, 0, len(keys))

	for _, key := range keys {
		if applied[key] {
			continue
		}
		desired, found := desiredByKey[key]
		if !found {
			continue
		}
		state, exists := actualByName[desired.Name]
		if exists {
			stableState, stable, detail, waitErr := e.awaitStable(ctx, client, cluster.Namespace, desired.Name, state)
			if waitErr != nil {
				message := fmt.Sprintf("admission check failed: %v", waitErr)
				failedResources[key] = message
				applyResults = append(applyResults, model.ApplyResult{
					ResourceKey: key,
					Action:      "wait",
					Status:      "failed",
					ErrorCode:   model.CodeCluster,
					Message:     message,
					AppliedAt:   time.Now().UTC(),
				})
				continue
			}
			if !stable {
				blockedResources[key] = true
				applyResults = append(applyResults, model.ApplyResult{
					ResourceKey: key,
					Action:      "wait",
					Status:      "blocked",
					ErrorCode:   model.CodeWait,
					Message:     detail,
					AppliedAt:   time.Now().UTC(),
				})
				_ = stableState
				continue
			}
		}
		applied[key] = true
		action, applyErr := client.ApplyDeployment(ctx, desired)
		result := model.ApplyResult{
			ResourceKey: key,
			Action:      action,
			Status:      "succeeded",
			AppliedAt:   time.Now().UTC(),
		}
		if applyErr != nil {
			result.Status = "failed"
			result.ErrorCode = model.CodeCluster
			result.Message = applyErr.Error()
			failedResources[key] = result.Message
		}
		applyResults = append(applyResults, result)
	}
	report.ApplyResults = applyResults

	after, err := client.ListDeployments(ctx, cluster.Namespace)
	if err != nil {
		e.markUnobservable(ctx, cluster, report, err)
		report.ApplyResults = applyResults
		report.Convergence = model.ConclusionFailed
		return
	}
	remaining := compare.Deployments(snapshot.Deployments, after)
	compare.ApplyResolutions(report.Differences, remaining, blockedResources)
	for index := range report.Differences {
		item := &report.Differences[index]
		if message, failed := failedResources[item.ResourceKey]; failed {
			item.Resolution = model.ResolutionNotApplied
			item.Reason = "apply failed: " + message
		}
	}

	report.BlockedCount = countResolution(report.Differences, model.ResolutionBlocked)
	remainingKeys := compare.ResourceKeys(remaining)
	remainingSet := make(map[string]bool, len(remainingKeys))
	for _, key := range remainingKeys {
		remainingSet[key] = true
	}
	for _, desired := range snapshot.Deployments {
		if !remainingSet[desired.ResourceKey] {
			report.ConvergedResources++
		}
	}

	switch {
	case len(failedResources) > 0:
		report.Convergence = model.ConclusionFailed
		report.ErrorCode = model.CodeCluster
		report.ErrorMessage = joinFailureMessages(failedResources)
	case len(remaining) > 0 || report.BlockedCount > 0:
		report.Convergence = model.ConclusionPartial
		if report.BlockedCount > 0 {
			report.ErrorCode = model.CodeWait
			report.ErrorMessage = fmt.Sprintf(
				"%d difference(s) blocked waiting for a stable rollout within %s",
				report.BlockedCount,
				e.options.AdmissionTimeout,
			)
		} else {
			report.ErrorMessage = fmt.Sprintf("%d difference(s) remain after apply", len(remaining))
		}
	default:
		report.Convergence = model.ConclusionConverged
	}
}

func (e *Engine) awaitStable(
	ctx context.Context,
	client kube.Client,
	namespace, name string,
	initial model.DeploymentState,
) (model.DeploymentState, bool, string, error) {
	deadline := time.Now().Add(e.options.AdmissionTimeout)
	state := initial
	for {
		stable, detail := IsStable(state)
		if stable {
			return state, true, detail, nil
		}
		if !time.Now().Before(deadline) {
			return state, false, fmt.Sprintf(
				"workload %s remained unstable for %s: %s",
				name,
				e.options.AdmissionTimeout,
				detail,
			), nil
		}
		timer := time.NewTimer(e.options.AdmissionPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return state, false, "context cancelled while waiting", ctx.Err()
		case <-timer.C:
		}
		next, err := client.GetDeployment(ctx, namespace, name)
		if errors.Is(err, kube.ErrNotFound) {
			return next, true, "deployment absent; create is admissible", nil
		}
		if err != nil {
			return state, false, "cannot read admission state", err
		}
		state = next
	}
}

func IsStable(state model.DeploymentState) (bool, string) {
	if state.Generation > 0 && state.Status.ObservedGeneration < state.Generation {
		return false, fmt.Sprintf(
			"controller observed generation %d of %d",
			state.Status.ObservedGeneration,
			state.Generation,
		)
	}
	for _, condition := range state.Status.Conditions {
		if condition.Type == "Progressing" && condition.Status == "False" {
			return false, "Progressing condition is False: " + condition.Reason
		}
		if condition.Reason == "ProgressDeadlineExceeded" {
			return false, "ProgressDeadlineExceeded"
		}
	}
	if state.Status.UnavailableReplicas > 0 {
		return false, fmt.Sprintf("%d replica(s) unavailable", state.Status.UnavailableReplicas)
	}
	if state.Status.Replicas != state.Replicas {
		return false, fmt.Sprintf(
			"%d pods exist for desired replica count %d",
			state.Status.Replicas,
			state.Replicas,
		)
	}
	if state.Status.UpdatedReplicas < state.Replicas {
		return false, fmt.Sprintf(
			"%d of %d replicas updated",
			state.Status.UpdatedReplicas,
			state.Replicas,
		)
	}
	if state.Status.AvailableReplicas < state.Replicas {
		return false, fmt.Sprintf(
			"%d of %d replicas available",
			state.Status.AvailableReplicas,
			state.Replicas,
		)
	}
	return true, "rollout is stable"
}

func (e *Engine) markUnobservable(
	ctx context.Context,
	cluster ClusterSpec,
	report *model.ClusterReport,
	cause error,
) {
	report.Observable = false
	report.ComparisonSuccess = false
	report.Convergence = model.ConclusionFailed
	report.ErrorCode = model.CodeCluster
	report.ErrorMessage = cause.Error()
	report.ErrorSourceBranch = cluster.Branch
	previous, err := e.options.Store.LastObservable(ctx, cluster.Name)
	if err == nil {
		report.Differences = previous.Differences
		report.DiffCount = previous.DiffCount
		report.ConvergedResources = previous.ConvergedResources
		report.LastObservedRoundID = previous.RoundID
	}
}

func applyBusinessError(report *model.ClusterReport, err error) {
	report.ErrorCode = model.ErrorCode(err)
	report.ErrorMessage = err.Error()
	report.ErrorSourceBranch = report.Branch
	var businessErr *model.BusinessError
	if errors.As(err, &businessErr) {
		report.ErrorResourceKey = businessErr.ResourceKey
		report.ErrorField = businessErr.Field
		if businessErr.SourceBranch != "" {
			report.ErrorSourceBranch = businessErr.SourceBranch
		}
	}
}

func countResolution(items []model.Difference, resolution string) int {
	count := 0
	for _, item := range items {
		if item.Resolution == resolution {
			count++
		}
	}
	return count
}

func joinFailureMessages(failures map[string]string) string {
	keys := make([]string, 0, len(failures))
	for key := range failures {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+": "+failures[key])
	}
	return strings.Join(parts, "; ")
}

func newRoundID(started time.Time) string {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("round-%d", started.UnixNano())
	}
	return "round-" + started.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(random)
}

func roleOf(cluster ClusterSpec) string {
	if cluster.Role != "" {
		return cluster.Role
	}
	if cluster.Branch == "cluster-prod" {
		return "prod"
	}
	return "dev"
}
