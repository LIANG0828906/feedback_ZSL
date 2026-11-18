package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"example.com/dualring-config/internal/model"
)

type check struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details"`
}

type summary struct {
	SchemaVersion string   `json:"schema_version"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	Server        string   `json:"server"`
	Namespace     string   `json:"namespace"`
	Releases      []string `json:"releases"`
	Checks        []check  `json:"checks"`
	Passed        bool     `json:"passed"`
	ReportPath    string   `json:"report_path,omitempty"`
	ReportError   string   `json:"report_error,omitempty"`
}

type clientListResponse struct {
	Clients []model.Client `json:"clients"`
}

type releaseListResponse struct {
	Releases []model.Release `json:"releases"`
}

type auditListResponse struct {
	Audit []model.AuditEntry `json:"audit"`
}

type historyResponse struct {
	Versions []struct {
		Version int64 `json:"version"`
	} `json:"versions"`
}

type simulationReport struct {
	SchemaVersion string `json:"schema_version"`
	Instances     []struct {
		InstanceID         string `json:"instance_id"`
		Ring               string `json:"ring"`
		FinalLoadedVersion int64  `json:"final_loaded_version"`
	} `json:"instances"`
	CanaryObservations []struct {
		ReleaseID      string  `json:"release_id"`
		State          string  `json:"state"`
		ErrorRate      float64 `json:"error_rate"`
		ErrorThreshold float64 `json:"error_threshold"`
		RolledBack     bool    `json:"rolled_back"`
	} `json:"canary_observations"`
	RollbackCount           int   `json:"rollback_count"`
	OfflineFallbackTriggers int64 `json:"offline_fallback_triggers"`
	ReadConsistency         struct {
		Requests          int64   `json:"requests"`
		DigestMismatches  int64   `json:"digest_mismatches"`
		DuplicateKeyViews int64   `json:"duplicate_key_views"`
		InvalidVersions   int64   `json:"invalid_versions"`
		P95MS             float64 `json:"p95_ms"`
		P95Within100MS    bool    `json:"p95_within_100ms"`
		Passed            bool    `json:"passed"`
	} `json:"read_consistency"`
}

type runner struct {
	opts    options
	client  *http.Client
	started time.Time
	checks  []check
	release []string
}

type options struct {
	server      string
	namespace   string
	instances   int
	observation time.Duration
	clientWait  time.Duration
	stateWait   time.Duration
	reportPath  string
	reportWait  time.Duration
	summaryPath string
}

func main() {
	var opts options
	flag.StringVar(&opts.server, "server", "http://127.0.0.1:8080", "config center base URL")
	flag.StringVar(&opts.namespace, "namespace", "payments", "configuration namespace")
	flag.IntVar(&opts.instances, "instances", 24, "expected simulated client count")
	flag.DurationVar(&opts.observation, "observation", 8*time.Second, "observation period for each release")
	flag.DurationVar(&opts.clientWait, "client-wait", 30*time.Second, "time to wait for all simulated clients")
	flag.DurationVar(&opts.stateWait, "state-wait", 30*time.Second, "time to wait for a release state")
	flag.StringVar(&opts.reportPath, "report", "reports/simulation-report.json", "simulator report to validate")
	flag.DurationVar(&opts.reportWait, "report-wait", 90*time.Second, "time to wait for the simulator report")
	flag.StringVar(&opts.summaryPath, "summary", "reports/scenario-summary.json", "scenario result output")
	flag.Parse()

	started := time.Now().UTC()
	r := &runner{
		opts:    opts,
		started: started,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
	if err := r.run(context.Background()); err != nil {
		r.add("scenario execution", false, err.Error())
	}
	result := summary{
		SchemaVersion: "1.0",
		StartedAt:     started.Format(time.RFC3339Nano),
		FinishedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		Server:        opts.server,
		Namespace:     opts.namespace,
		Releases:      r.release,
		Checks:        r.checks,
		Passed:        true,
		ReportPath:    opts.reportPath,
	}
	for _, item := range r.checks {
		if !item.Passed {
			result.Passed = false
		}
	}
	if err := writeJSONAtomic(opts.summaryPath, result); err != nil {
		fmt.Fprintln(os.Stderr, "write scenario summary:", err)
		os.Exit(1)
	}
	for _, item := range r.checks {
		status := "PASS"
		if !item.Passed {
			status = "FAIL"
		}
		fmt.Printf("%s  %-42s %s\n", status, item.Name, item.Details)
	}
	if !result.Passed {
		os.Exit(1)
	}
	fmt.Printf("scenario summary: %s\n", opts.summaryPath)
}

func (r *runner) run(ctx context.Context) error {
	clients, err := r.waitForClients(ctx)
	if err != nil {
		return err
	}
	canaryClients, fullClients := splitClients(clients)
	expectedCanary := float64(r.opts.instances) * 0.10
	ratioOK := len(canaryClients) >= 1 && len(canaryClients) <= r.opts.instances/2 &&
		float64(len(canaryClients)) >= expectedCanary*0.25
	r.add("canary cohort assigned", ratioOK,
		fmt.Sprintf("canary=%d full=%d expected_canary~=%.1f", len(canaryClients), len(fullClients), expectedCanary))
	if !ratioOK {
		return errors.New("canary cohort is not approximately 10 percent")
	}

	baseline, err := r.getConfig(ctx)
	if err != nil {
		return err
	}
	baseGen := majorityGeneration(baseline.Items)
	beforeVersion := baseline.Version

	release1, err := r.createRelease(ctx, "release-one", 1)
	if err != nil {
		return err
	}
	r.release = append(r.release, release1.ID)
	release1, err = r.transition(ctx, release1.ID, "canary")
	if err != nil {
		return err
	}
	r.add("release one entered canary", release1.State == model.ReleaseCanary,
		fmt.Sprintf("release=%s candidate=%d", release1.ID, release1.CandidateSnapshot))

	rejectedState, rejectedCode := r.transitionExpectError(ctx, release1.ID, "full")
	stateAfterReject, _ := r.getRelease(ctx, release1.ID)
	r.add("observation blocks forced full", rejectedCode == model.CodeObservationActive &&
		rejectedState == http.StatusConflict && stateAfterReject.State == model.ReleaseCanary,
		fmt.Sprintf("code=%s http=%d state=%s", rejectedCode, rejectedState, stateAfterReject.State))

	r.addRingIsolation(ctx, "release one", release1,
		canaryClients[0].InstanceID, fullClients[0].InstanceID, beforeVersion, baseGen, 1)

	release1, err = r.waitReleaseState(ctx, release1.ID, model.ReleaseObservedPass)
	if err != nil {
		return err
	}
	r.add("release one passed observation", release1.ErrorRate <= release1.ErrorThreshold,
		fmt.Sprintf("success=%d error=%d rate=%.6f", release1.SuccessCount, release1.ErrorCount, release1.ErrorRate))
	release1, err = r.transition(ctx, release1.ID, "full")
	if err != nil {
		return err
	}
	r.add("release one promoted to full", release1.State == model.ReleaseFull,
		fmt.Sprintf("version=%d", release1.CandidateSnapshot))
	r.addRingConverged(ctx, "release one", release1.CandidateSnapshot, 1,
		canaryClients[0].InstanceID, fullClients[0].InstanceID)

	release2, err := r.createRelease(ctx, "release-two", 2)
	if err != nil {
		return err
	}
	r.release = append(r.release, release2.ID)
	release2, err = r.transition(ctx, release2.ID, "canary")
	if err != nil {
		return err
	}
	r.add("release two entered canary", release2.State == model.ReleaseCanary,
		fmt.Sprintf("release=%s candidate=%d", release2.ID, release2.CandidateSnapshot))
	r.addRingIsolation(ctx, "release two", release2,
		canaryClients[0].InstanceID, fullClients[0].InstanceID, release1.CandidateSnapshot, 1, 2)

	release2, err = r.waitReleaseState(ctx, release2.ID, model.ReleaseRolledBack)
	if err != nil {
		return err
	}
	r.add("release two automatically rolled back",
		release2.ErrorRate > release2.ErrorThreshold && release2.RollbackReason != "",
		fmt.Sprintf("success=%d error=%d rate=%.6f reason=%s",
			release2.SuccessCount, release2.ErrorCount, release2.ErrorRate, release2.RollbackReason))
	r.addRingConverged(ctx, "rollback preserves full version", release1.CandidateSnapshot, 1,
		canaryClients[0].InstanceID, fullClients[0].InstanceID)

	r.addAuditChecks(ctx, release1.ID, release2.ID)
	r.addHistoryCheck(ctx, "payment.param.000")

	report, err := r.waitForReport(ctx, r.started)
	if err != nil {
		r.add("simulation report present", false, err.Error())
		return nil
	}
	r.addReportChecks(report, release1.CandidateSnapshot, release2.ID)
	return nil
}

func (r *runner) add(name string, passed bool, details string) {
	r.checks = append(r.checks, check{Name: name, Passed: passed, Details: details})
}

func (r *runner) waitForClients(ctx context.Context) ([]model.Client, error) {
	deadline := time.Now().Add(r.opts.clientWait)
	for {
		var response clientListResponse
		if err := r.request(ctx, http.MethodGet,
			r.opts.server+"/v1/admin/clients", nil, &response); err != nil {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("wait for clients: %w", err)
			}
		} else if len(response.Clients) >= r.opts.instances {
			sort.Slice(response.Clients, func(i, j int) bool {
				return response.Clients[i].InstanceID < response.Clients[j].InstanceID
			})
			return response.Clients, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for %d clients", r.opts.instances)
		}
		sleepContext(ctx, 200*time.Millisecond)
	}
}

func splitClients(clients []model.Client) ([]model.Client, []model.Client) {
	var canary, full []model.Client
	for _, client := range clients {
		if client.Ring == model.RingCanary {
			canary = append(canary, client)
		} else {
			full = append(full, client)
		}
	}
	return canary, full
}

func (r *runner) getConfig(ctx context.Context) (model.ConfigSet, error) {
	var config model.ConfigSet
	endpoint := r.opts.server + "/v1/config?namespace=" + url.QueryEscape(r.opts.namespace)
	err := r.request(ctx, http.MethodGet, endpoint, nil, &config)
	return config, err
}

func (r *runner) createRelease(ctx context.Context, operator string, generation int) (model.Release, error) {
	config, err := r.getConfig(ctx)
	if err != nil {
		return model.Release{}, err
	}
	var changes []model.ItemChange
	for _, item := range config.Items {
		if !strings.HasPrefix(item.Key, "payment.param.") {
			continue
		}
		changes = append(changes, model.ItemChange{
			Key:   item.Key,
			Value: scenarioValue(item.Key, generation),
		})
	}
	errorRate := "0"
	if generation == 2 {
		errorRate = "0.6"
	}
	changes = append(changes, model.ItemChange{Key: "_sim.effective_error_rate", Value: errorRate})
	body := map[string]any{
		"namespace":           r.opts.namespace,
		"operator":            operator,
		"changes":             changes,
		"observation_seconds": r.opts.observation.Seconds(),
	}
	var release model.Release
	if err := r.request(ctx, http.MethodPost, r.opts.server+"/v1/releases", body, &release); err != nil {
		return release, err
	}
	return release, nil
}

func scenarioValue(key string, generation int) string {
	suffix := strings.TrimPrefix(key, "payment.param.")
	return fmt.Sprintf(`{"gen":%d,"payload":"scenario-%s"}`, generation, suffix)
}

func (r *runner) transition(ctx context.Context, releaseID, target string) (model.Release, error) {
	body := map[string]any{"target": target, "operator": "scenario"}
	var release model.Release
	endpoint := r.opts.server + "/v1/releases/" + url.PathEscape(releaseID) + "/transition"
	if err := r.request(ctx, http.MethodPost, endpoint, body, &release); err != nil {
		return release, err
	}
	return release, nil
}

func (r *runner) transitionExpectError(ctx context.Context, releaseID, target string) (int, string) {
	body := map[string]any{"target": target, "operator": "scenario"}
	endpoint := r.opts.server + "/v1/releases/" + url.PathEscape(releaseID) + "/transition"
	err := r.request(ctx, http.MethodPost, endpoint, body, nil)
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.status, apiErr.code
	}
	return 0, ""
}

func (r *runner) getRelease(ctx context.Context, releaseID string) (model.Release, error) {
	var release model.Release
	endpoint := r.opts.server + "/v1/releases/" + url.PathEscape(releaseID)
	err := r.request(ctx, http.MethodGet, endpoint, nil, &release)
	return release, err
}

func (r *runner) waitReleaseState(ctx context.Context, releaseID, target string) (model.Release, error) {
	deadline := time.Now().Add(r.opts.stateWait)
	for {
		release, err := r.getRelease(ctx, releaseID)
		if err != nil {
			return release, err
		}
		if release.State == target {
			return release, nil
		}
		if release.State == model.ReleaseRolledBack && target != model.ReleaseRolledBack {
			return release, fmt.Errorf("release rolled back while waiting for %s: %s",
				target, release.RollbackReason)
		}
		if time.Now().After(deadline) {
			return release, fmt.Errorf("timed out waiting for release state %s; current=%s", target, release.State)
		}
		sleepContext(ctx, 200*time.Millisecond)
	}
}

func (r *runner) pull(ctx context.Context, instanceID string) (model.PullResponse, error) {
	values := url.Values{}
	values.Set("instance_id", instanceID)
	values.Set("namespace", r.opts.namespace)
	values.Set("local_version", "0")
	var pull model.PullResponse
	err := r.request(ctx, http.MethodGet,
		r.opts.server+"/v1/config/pull?"+values.Encode(), nil, &pull)
	return pull, err
}

func (r *runner) addRingIsolation(
	ctx context.Context,
	label string,
	release model.Release,
	canaryID, fullID string,
	fullVersion int64,
	fullGeneration, candidateGeneration int,
) {
	canaryPull, err1 := r.pull(ctx, canaryID)
	fullPull, err2 := r.pull(ctx, fullID)
	if err1 != nil || err2 != nil {
		r.add(label+" ring isolation", false, fmt.Sprintf("canary_error=%v full_error=%v", err1, err2))
		return
	}
	ok := canaryPull.Version == release.CandidateSnapshot &&
		canaryPull.ReleaseID == release.ID &&
		generationOf(canaryPull.Items) == candidateGeneration &&
		fullPull.Version == fullVersion &&
		fullPull.ReleaseID == "" &&
		generationOf(fullPull.Items) == fullGeneration
	r.add(label+" ring isolation", ok,
		fmt.Sprintf("canary_version=%d gen=%d full_version=%d gen=%d expected_full=%d",
			canaryPull.Version, generationOf(canaryPull.Items),
			fullPull.Version, generationOf(fullPull.Items), fullVersion))
}

func (r *runner) addRingConverged(
	ctx context.Context,
	label string,
	version int64,
	generation int,
	canaryID, fullID string,
) {
	canaryPull, err1 := r.pull(ctx, canaryID)
	fullPull, err2 := r.pull(ctx, fullID)
	if err1 != nil || err2 != nil {
		r.add(label+" convergence", false, fmt.Sprintf("canary_error=%v full_error=%v", err1, err2))
		return
	}
	ok := canaryPull.Version == version && fullPull.Version == version &&
		generationOf(canaryPull.Items) == generation &&
		generationOf(fullPull.Items) == generation
	r.add(label+" convergence", ok,
		fmt.Sprintf("canary_version=%d full_version=%d expected=%d gen=%d",
			canaryPull.Version, fullPull.Version, version, generationOf(fullPull.Items)))
}

func (r *runner) addAuditChecks(ctx context.Context, release1ID, release2ID string) {
	var response auditListResponse
	endpoint := r.opts.server + "/v1/admin/audit?namespace=" +
		url.QueryEscape(r.opts.namespace) + "&limit=1000"
	if err := r.request(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		r.add("audit trail", false, err.Error())
		return
	}
	var release1Full, release2Rollback, rollbackReason bool
	for _, entry := range response.Audit {
		if entry.StatusCode != model.CodeOK {
			continue
		}
		if entry.ReleaseID == release2ID && entry.Operator == "system:auto-rollback" {
			release2Rollback = true
			rollbackReason = strings.Contains(entry.Details, "from=canary to=rolled_back")
		}
		if entry.ReleaseID == release1ID && strings.Contains(entry.Details, "to=full") {
			release1Full = true
		}
	}
	r.add("audit trail records full and rollback",
		release1Full && release2Rollback && rollbackReason,
		fmt.Sprintf("release1_full=%t release2_auto_rollback=%t reason=%t",
			release1Full, release2Rollback, rollbackReason))
}

func (r *runner) addHistoryCheck(ctx context.Context, key string) {
	values := url.Values{}
	values.Set("namespace", r.opts.namespace)
	values.Set("key", key)
	values.Set("limit", "10")
	var response historyResponse
	if err := r.request(ctx, http.MethodGet,
		r.opts.server+"/v1/admin/items/history?"+values.Encode(), nil, &response); err != nil {
		r.add("item history retained", false, err.Error())
		return
	}
	r.add("item history retained", len(response.Versions) >= 2,
		fmt.Sprintf("versions=%d", len(response.Versions)))
}

func (r *runner) waitForReport(ctx context.Context, after time.Time) (simulationReport, error) {
	deadline := time.Now().Add(r.opts.reportWait)
	for {
		var report simulationReport
		info, err := os.Stat(r.opts.reportPath)
		if err == nil && info.ModTime().After(after) {
			data, readErr := os.ReadFile(r.opts.reportPath)
			if readErr == nil && json.Unmarshal(data, &report) == nil {
				return report, nil
			}
		}
		if time.Now().After(deadline) {
			return report, fmt.Errorf("timed out waiting for report %s", r.opts.reportPath)
		}
		sleepContext(ctx, 500*time.Millisecond)
	}
}

func (r *runner) addReportChecks(report simulationReport, expectedVersion int64, rollbackReleaseID string) {
	allVersion := len(report.Instances) == r.opts.instances
	for _, instance := range report.Instances {
		if instance.FinalLoadedVersion != expectedVersion {
			allVersion = false
		}
	}
	r.add("report instances and final versions", allVersion,
		fmt.Sprintf("instances=%d expected_version=%d", len(report.Instances), expectedVersion))
	r.add("report rollback count and observation rate", report.RollbackCount >= 1,
		fmt.Sprintf("rollbacks=%d observations=%d", report.RollbackCount, len(report.CanaryObservations)))
	latestRate := 0.0
	latestState := ""
	for _, observation := range report.CanaryObservations {
		if observation.ReleaseID == rollbackReleaseID {
			latestRate = observation.ErrorRate
			latestState = observation.State
		}
	}
	r.add("report shows rollback error rate above threshold", latestState == model.ReleaseRolledBack && latestRate > 0.005,
		fmt.Sprintf("state=%s error_rate=%.6f", latestState, latestRate))
	r.add("report shows offline fallback", report.OfflineFallbackTriggers > 0,
		fmt.Sprintf("triggers=%d", report.OfflineFallbackTriggers))
	readOK := report.ReadConsistency.Passed &&
		report.ReadConsistency.Requests > 0 &&
		report.ReadConsistency.DigestMismatches == 0 &&
		report.ReadConsistency.DuplicateKeyViews == 0 &&
		report.ReadConsistency.InvalidVersions == 0 &&
		report.ReadConsistency.P95Within100MS
	r.add("report read consistency and P95", readOK,
		fmt.Sprintf("requests=%d digest_mismatch=%d duplicates=%d invalid=%d p95_ms=%.3f",
			report.ReadConsistency.Requests, report.ReadConsistency.DigestMismatches,
			report.ReadConsistency.DuplicateKeyViews, report.ReadConsistency.InvalidVersions,
			report.ReadConsistency.P95MS))
}

func generationOf(items []model.Item) int {
	for _, item := range items {
		if item.Key != "payment.param.000" {
			continue
		}
		var value struct {
			Gen int `json:"gen"`
		}
		if json.Unmarshal([]byte(item.Value), &value) == nil {
			return value.Gen
		}
	}
	return -1
}

func majorityGeneration(items []model.Item) int {
	return generationOf(items)
}

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("HTTP %d %s: %s", e.status, e.code, e.message)
}

func (r *runner) request(
	ctx context.Context,
	method, endpoint string,
	requestBody any,
	responseBody any,
) error {
	var body io.Reader
	if requestBody != nil {
		data, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &payload)
		return &apiError{status: response.StatusCode, code: payload.Code, message: payload.Message}
	}
	if responseBody != nil {
		if err := json.Unmarshal(data, responseBody); err != nil {
			return err
		}
	}
	return nil
}

func sleepContext(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".scenario-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempName, 0o644); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}
