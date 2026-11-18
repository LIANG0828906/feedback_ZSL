package stress

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"configcenter/internal/model"
	"configcenter/internal/seed"
)

type Options struct {
	BaseURL      string
	Namespace    string
	ClientCount  int
	Seed         int64
	MergeWindow  time.Duration
	PublishGap   time.Duration
	ConvergeWait time.Duration
}

type clientStatus struct {
	ClientID       string  `json:"client_id"`
	KnownVersion   int64   `json:"known_version"`
	AppliedVersion int64   `json:"applied_version"`
	StaleResponses uint64  `json:"stale_responses"`
	AppliedHistory []int64 `json:"applied_history"`
}

type namespaceStatus struct {
	Name          string         `json:"name"`
	LatestVersion int64          `json:"latest_version"`
	Clients       []clientStatus `json:"clients"`
}

type statusResponse struct {
	Namespaces []namespaceStatus `json:"namespaces"`
}

type snapshotResponse struct {
	Namespace    string            `json:"namespace"`
	Version      int64             `json:"version"`
	Config       map[string]string `json:"config"`
	ItemVersions map[string]int64  `json:"item_versions"`
}

type resultSummary struct {
	Result               string         `json:"result"`
	Namespace            string         `json:"namespace"`
	InitialVersion       int64          `json:"initial_version"`
	FinalVersion         int64          `json:"final_version"`
	PublishedVersions    []int64        `json:"published_versions"`
	MergedBroadcastCount uint64         `json:"merged_broadcast_count"`
	RawChangesMerged     uint64         `json:"raw_changes_merged"`
	Clients              []clientStatus `json:"clients"`
	Rollback             rollbackResult `json:"rollback"`
	Snapshot             snapshotResult `json:"snapshot"`
	TimeoutPoll          timeoutResult  `json:"timeout_poll"`
	VersionAhead         string         `json:"version_ahead_error"`
}

type rollbackResult struct {
	ErrorCode     string `json:"error_code"`
	VersionBefore int64  `json:"version_before"`
	VersionAfter  int64  `json:"version_after"`
}

type snapshotResult struct {
	IntermediateVersion int64  `json:"intermediate_version"`
	ItemCount           int    `json:"item_count"`
	TimeoutValue        string `json:"timeout_value"`
}

type timeoutResult struct {
	Version int64 `json:"version"`
	Changed bool  `json:"changed"`
	Changes int   `json:"changes"`
}

type httpError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d %s: %s", e.StatusCode, e.Code, e.Message)
}

func Run(ctx context.Context, options Options, logger *log.Logger) error {
	if options.BaseURL == "" {
		options.BaseURL = "http://127.0.0.1:18473"
	}
	if options.Namespace == "" {
		options.Namespace = seed.DefaultNamespace
	}
	if options.ClientCount <= 0 {
		options.ClientCount = 3
	}
	if options.Seed == 0 {
		options.Seed = seed.DefaultSeed
	}
	if options.MergeWindow <= 0 {
		options.MergeWindow = 500 * time.Millisecond
	}
	if options.PublishGap <= 0 {
		options.PublishGap = 8 * time.Millisecond
	}
	if options.ConvergeWait <= 0 {
		options.ConvergeWait = 15 * time.Second
	}
	if logger == nil {
		logger = log.Default()
	}
	client := &http.Client{Timeout: 10 * time.Second}
	baseURL := strings.TrimRight(options.BaseURL, "/")

	var current model.CurrentResponse
	if err := getJSON(ctx, client, baseURL+"/v1/namespaces/"+url.PathEscape(options.Namespace)+"/current", &current); err != nil {
		return fmt.Errorf("read current version: %w", err)
	}
	initialVersion := current.Version
	logger.Printf("STRESS initial namespace=%s version=%d", options.Namespace, initialVersion)

	burst := seed.Burst(1, options.Seed)
	if len(burst) != seed.ConfigItemCount {
		return fmt.Errorf("generator returned %d changes; expected %d", len(burst), seed.ConfigItemCount)
	}
	publishedVersions := make([]int64, 0, len(burst))
	previousVersion := initialVersion
	for index, change := range burst {
		var response model.PublishResponse
		payload := model.PublishRequest{Changes: []model.Change{change}}
		if err := postJSON(ctx, client, baseURL+"/v1/namespaces/"+url.PathEscape(options.Namespace)+"/publish", payload, &response, http.StatusOK); err != nil {
			return fmt.Errorf("publish change %d (%s): %w", index, change.Key, err)
		}
		if response.Version != previousVersion+1 {
			return fmt.Errorf("non-contiguous version: got %d after %d", response.Version, previousVersion)
		}
		previousVersion = response.Version
		publishedVersions = append(publishedVersions, response.Version)
		if index != len(burst)-1 && !sleep(ctx, options.PublishGap) {
			return ctx.Err()
		}
	}
	finalVersion := previousVersion
	logger.Printf(
		"STRESS published raw_changes=%d versions=%d..%d in one merge window",
		len(publishedVersions),
		publishedVersions[0],
		finalVersion,
	)

	if err := waitForClients(
		ctx,
		client,
		baseURL,
		options.Namespace,
		options.ClientCount,
		initialVersion,
		finalVersion,
		options.ConvergeWait,
	); err != nil {
		return err
	}
	converged, err := fetchStatus(ctx, client, baseURL)
	if err != nil {
		return err
	}
	beforeRollback, err := findNamespaceStatus(converged, options.Namespace)
	if err != nil {
		return err
	}

	snapshotVersion := initialVersion + 1
	if snapshotVersion >= finalVersion {
		snapshotVersion = initialVersion
	}
	var snapshot snapshotResponse
	if err := getJSON(
		ctx,
		client,
		baseURL+"/v1/namespaces/"+url.PathEscape(options.Namespace)+"/snapshots/"+strconv.FormatInt(snapshotVersion, 10),
		&snapshot,
	); err != nil {
		return fmt.Errorf("read intermediate snapshot: %w", err)
	}
	if len(snapshot.Config) != seed.ConfigItemCount {
		return fmt.Errorf("intermediate snapshot has %d items; expected %d", len(snapshot.Config), seed.ConfigItemCount)
	}
	for key, version := range snapshot.ItemVersions {
		if version > snapshotVersion {
			return fmt.Errorf("snapshot item %s has future version %d", key, version)
		}
	}

	var finalSnapshot snapshotResponse
	if err := getJSON(
		ctx,
		client,
		baseURL+"/v1/namespaces/"+url.PathEscape(options.Namespace)+"/snapshots/"+strconv.FormatInt(finalVersion, 10),
		&finalSnapshot,
	); err != nil {
		return fmt.Errorf("read final snapshot: %w", err)
	}
	if finalSnapshot.Config["order.timeout_ms"] != "1200" {
		return fmt.Errorf("final timeout is %q; expected 1200", finalSnapshot.Config["order.timeout_ms"])
	}
	if len(finalSnapshot.Config) != seed.ConfigItemCount {
		return fmt.Errorf("final snapshot has %d items; expected %d", len(finalSnapshot.Config), seed.ConfigItemCount)
	}

	var timeoutResponse model.WatchResponse
	if err := getJSONStatus(
		ctx,
		client,
		baseURL+"/v1/namespaces/"+url.PathEscape(options.Namespace)+"/watch?version="+
			strconv.FormatInt(finalVersion, 10)+"&timeout=250ms",
		&timeoutResponse,
		http.StatusOK,
	); err != nil {
		return fmt.Errorf("timeout long-poll: %w", err)
	}
	if timeoutResponse.Changed || timeoutResponse.Version != finalVersion || len(timeoutResponse.Changes) != 0 {
		return fmt.Errorf("timeout long-poll returned changed=%v version=%d changes=%d",
			timeoutResponse.Changed, timeoutResponse.Version, len(timeoutResponse.Changes))
	}

	aheadCode, err := expectWatchError(
		ctx,
		client,
		baseURL+"/v1/namespaces/"+url.PathEscape(options.Namespace)+"/watch?version="+
			strconv.FormatInt(finalVersion+7, 10)+"&timeout=250ms",
		http.StatusConflict,
	)
	if err != nil {
		return fmt.Errorf("version-ahead check: %w", err)
	}
	if aheadCode != "E_VERSION_AHEAD" {
		return fmt.Errorf("version-ahead returned %q; expected E_VERSION_AHEAD", aheadCode)
	}

	invalidChanges := seed.Burst(2, options.Seed)
	invalidChanges = append(invalidChanges, model.Change{Key: "", Value: "invalid"})
	invalidPayload := model.PublishRequest{Changes: invalidChanges}
	rollbackCode, err := postJSONExpectError(
		ctx,
		client,
		baseURL+"/v1/namespaces/"+url.PathEscape(options.Namespace)+"/publish",
		invalidPayload,
		http.StatusBadRequest,
	)
	if err != nil {
		return fmt.Errorf("invalid publish: %w", err)
	}
	if rollbackCode != "E_VALIDATION" {
		return fmt.Errorf("invalid publish returned %q; expected E_VALIDATION", rollbackCode)
	}

	afterVersion, err := fetchCurrentVersion(ctx, client, baseURL, options.Namespace)
	if err != nil {
		return err
	}
	if afterVersion != finalVersion {
		return fmt.Errorf("failed publish advanced version from %d to %d", finalVersion, afterVersion)
	}
	if !sleep(ctx, options.MergeWindow+250*time.Millisecond) {
		return ctx.Err()
	}
	afterRollbackStatus, err := fetchStatus(ctx, client, baseURL)
	if err != nil {
		return err
	}
	afterRollback, err := findNamespaceStatus(afterRollbackStatus, options.Namespace)
	if err != nil {
		return err
	}
	if afterRollback.LatestVersion != finalVersion {
		return fmt.Errorf("version changed after rollback: %d", afterRollback.LatestVersion)
	}
	if err := compareClientHistories(beforeRollback.Clients, afterRollback.Clients); err != nil {
		return fmt.Errorf("rollback notified a client: %w", err)
	}

	metricsText, err := getText(ctx, client, baseURL+"/metrics")
	if err != nil {
		return err
	}
	mergedBroadcasts, rawChanges, longPolls, err := parseMetrics(metricsText, options.Namespace)
	if err != nil {
		return err
	}
	if rawChanges < uint64(len(publishedVersions)) {
		return fmt.Errorf("raw merged changes = %d; expected at least %d", rawChanges, len(publishedVersions))
	}
	if mergedBroadcasts == 0 {
		return fmt.Errorf("no merged broadcast was counted")
	}
	if longPolls == 0 {
		return fmt.Errorf("no long-poll request was counted")
	}

	summary := resultSummary{
		Result:               "PASS",
		Namespace:            options.Namespace,
		InitialVersion:       initialVersion,
		FinalVersion:         finalVersion,
		PublishedVersions:    publishedVersions,
		MergedBroadcastCount: mergedBroadcasts,
		RawChangesMerged:     rawChanges,
		Clients:              afterRollback.Clients,
		Rollback: rollbackResult{
			ErrorCode:     rollbackCode,
			VersionBefore: finalVersion,
			VersionAfter:  afterVersion,
		},
		Snapshot: snapshotResult{
			IntermediateVersion: snapshotVersion,
			ItemCount:           len(snapshot.Config),
			TimeoutValue:        finalSnapshot.Config["order.timeout_ms"],
		},
		TimeoutPoll: timeoutResult{
			Version: timeoutResponse.Version,
			Changed: timeoutResponse.Changed,
			Changes: len(timeoutResponse.Changes),
		},
		VersionAhead: aheadCode,
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	logger.Printf("DEMO_RESULT %s", encoded)
	return nil
}

func waitForClients(
	ctx context.Context,
	client *http.Client,
	baseURL, namespace string,
	clientCount int,
	initialVersion, finalVersion int64,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var lastProblem string
	for {
		status, err := fetchStatus(ctx, client, baseURL)
		if err == nil {
			namespaceStatus, findErr := findNamespaceStatus(status, namespace)
			if findErr == nil {
				if namespaceStatus.LatestVersion == finalVersion && len(namespaceStatus.Clients) >= clientCount {
					problem := ""
					for _, reported := range namespaceStatus.Clients {
						if reported.KnownVersion != finalVersion || reported.AppliedVersion != finalVersion {
							problem = fmt.Sprintf("%s is at %d/%d", reported.ClientID, reported.KnownVersion, reported.AppliedVersion)
							break
						}
						if reported.StaleResponses != 0 {
							problem = fmt.Sprintf("%s reported %d stale responses", reported.ClientID, reported.StaleResponses)
							break
						}
						if historyErr := validateHistory(reported.AppliedHistory, initialVersion, finalVersion); historyErr != nil {
							problem = fmt.Sprintf("%s: %v", reported.ClientID, historyErr)
							break
						}
					}
					if problem == "" {
						clients := append([]clientStatus(nil), namespaceStatus.Clients...)
						sort.Slice(clients, func(i, j int) bool { return clients[i].ClientID < clients[j].ClientID })
						namespaceStatus.Clients = clients
						return nil
					}
					lastProblem = problem
				} else {
					lastProblem = fmt.Sprintf(
						"latest=%d clients=%d/%d",
						namespaceStatus.LatestVersion,
						len(namespaceStatus.Clients),
						clientCount,
					)
				}
			}
		}
		if time.Now().After(deadline) {
			if lastProblem == "" {
				lastProblem = "clients did not report"
			}
			return fmt.Errorf("clients did not converge within %s: %s", timeout, lastProblem)
		}
		if !sleep(ctx, 100*time.Millisecond) {
			return ctx.Err()
		}
	}
}

func validateHistory(history []int64, initialVersion, finalVersion int64) error {
	if len(history) == 0 {
		return errors.New("empty applied history")
	}
	previous := int64(-1)
	for _, version := range history {
		if version <= previous {
			return fmt.Errorf("applied history is not strictly increasing: %v", history)
		}
		if version > initialVersion && version < finalVersion {
			return fmt.Errorf("intermediate version %d leaked to business process: %v", version, history)
		}
		previous = version
	}
	if history[len(history)-1] != finalVersion {
		return fmt.Errorf("last applied version is %d; expected %d", history[len(history)-1], finalVersion)
	}
	return nil
}

func compareClientHistories(before, after []clientStatus) error {
	if len(before) != len(after) {
		return fmt.Errorf("client count changed from %d to %d", len(before), len(after))
	}
	beforeMap := make(map[string]clientStatus, len(before))
	for _, client := range before {
		beforeMap[client.ClientID] = client
	}
	for _, client := range after {
		previous, ok := beforeMap[client.ClientID]
		if !ok {
			return fmt.Errorf("new client %s appeared", client.ClientID)
		}
		if client.AppliedVersion != previous.AppliedVersion ||
			client.KnownVersion != previous.KnownVersion ||
			!reflect.DeepEqual(client.AppliedHistory, previous.AppliedHistory) {
			return fmt.Errorf("client %s history changed", client.ClientID)
		}
	}
	return nil
}

func fetchStatus(ctx context.Context, client *http.Client, baseURL string) (statusResponse, error) {
	var status statusResponse
	err := getJSON(ctx, client, baseURL+"/v1/status", &status)
	return status, err
}

func findNamespaceStatus(status statusResponse, namespace string) (namespaceStatus, error) {
	for _, item := range status.Namespaces {
		if item.Name == namespace {
			return item, nil
		}
	}
	return namespaceStatus{}, fmt.Errorf("namespace %q is absent from status", namespace)
}

func fetchCurrentVersion(ctx context.Context, client *http.Client, baseURL, namespace string) (int64, error) {
	var current model.CurrentResponse
	err := getJSON(ctx, client, baseURL+"/v1/namespaces/"+url.PathEscape(namespace)+"/current", &current)
	return current.Version, err
}

func getJSON(ctx context.Context, client *http.Client, requestURL string, target any) error {
	return getJSONStatus(ctx, client, requestURL, target, http.StatusOK)
}

func getJSONStatus(ctx context.Context, client *http.Client, requestURL string, target any, expectedStatus int) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		var apiError model.ErrorResponse
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&apiError)
		return &httpError{StatusCode: response.StatusCode, Code: apiError.Code, Message: apiError.Message}
	}
	return json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(target)
}

func postJSON(ctx context.Context, client *http.Client, requestURL string, payload, target any, expectedStatus int) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		var apiError model.ErrorResponse
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&apiError)
		return &httpError{StatusCode: response.StatusCode, Code: apiError.Code, Message: apiError.Message}
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(target)
}

func postJSONExpectError(
	ctx context.Context,
	client *http.Client,
	requestURL string,
	payload any,
	expectedStatus int,
) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var apiError model.ErrorResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&apiError); err != nil {
		return "", err
	}
	if response.StatusCode != expectedStatus {
		return apiError.Code, fmt.Errorf("got HTTP %d; expected %d", response.StatusCode, expectedStatus)
	}
	return apiError.Code, nil
}

func expectWatchError(
	ctx context.Context,
	client *http.Client,
	requestURL string,
	expectedStatus int,
) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var apiError model.ErrorResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&apiError); err != nil {
		return "", err
	}
	if response.StatusCode != expectedStatus {
		return apiError.Code, fmt.Errorf("got HTTP %d; expected %d", response.StatusCode, expectedStatus)
	}
	return apiError.Code, nil
}

func getText(ctx context.Context, client *http.Client, requestURL string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metrics returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return string(body), err
}

func parseMetrics(text, namespace string) (merged, raw, longPolls uint64, err error) {
	needleSuffix := "{namespace=\"" + namespace + "\"}"
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil {
			continue
		}
		switch {
		case fields[0] == "config_merged_broadcasts_total"+needleSuffix:
			merged = value
		case fields[0] == "config_merged_raw_changes_total"+needleSuffix:
			raw = value
		case fields[0] == "config_long_poll_requests_total"+needleSuffix:
			longPolls = value
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, 0, err
	}
	return merged, raw, longPolls, nil
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
