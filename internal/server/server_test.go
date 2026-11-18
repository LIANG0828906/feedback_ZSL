package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"configcenter/internal/model"
	"configcenter/internal/notify"
	"configcenter/internal/seed"
	"configcenter/internal/server"
	"configcenter/internal/store"
	"configcenter/internal/telemetry"
)

type testEnvironment struct {
	store     *store.Store
	notifier  *notify.Notifier
	telemetry *telemetry.Telemetry
	server    *httptest.Server
	client    *http.Client
}

func newTestEnvironment(t *testing.T, mergeWindow time.Duration) *testEnvironment {
	t.Helper()
	configStore, err := store.Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	version, _, err := configStore.EnsureNamespace(context.Background(), seed.DefaultNamespace, seed.DefaultOrderConfig())
	if err != nil {
		t.Fatal(err)
	}
	metrics := telemetry.New()
	metrics.EnsureNamespace(seed.DefaultNamespace)
	notifier := notify.New(
		mergeWindow,
		2*time.Second,
		configStore.LatestVersion,
		func(broadcast notify.Broadcast) {
			metrics.RecordMerge(broadcast.Namespace, broadcast.RawChanges)
		},
	)
	notifier.Prime(seed.DefaultNamespace, version)
	handler := server.New(configStore, notifier, metrics, server.Options{
		MergeWindow:        mergeWindow,
		MergeMaxWait:       2 * time.Second,
		DefaultPollTimeout: 5 * time.Second,
		MaxPollTimeout:     10 * time.Second,
	})
	testServer := httptest.NewServer(handler.Handler())
	env := &testEnvironment{
		store:     configStore,
		notifier:  notifier,
		telemetry: metrics,
		server:    testServer,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
	t.Cleanup(func() {
		testServer.Close()
		notifier.Close()
		configStore.Close()
	})
	return env
}

func TestStormOnlyExposesFinalVersionAndRollsBack(t *testing.T) {
	env := newTestEnvironment(t, 500*time.Millisecond)
	initialVersion := int64(1)

	type watchResult struct {
		response model.WatchResponse
		err      error
	}
	results := make(chan watchResult, 3)
	var waitGroup sync.WaitGroup
	for i := 0; i < 3; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			response, err := getWatch(env.client, env.server.URL, initialVersion, "5s")
			results <- watchResult{response: response, err: err}
		}()
	}
	time.Sleep(100 * time.Millisecond)

	burst := seed.Burst(1, seed.DefaultSeed)
	var finalVersion int64
	for index, change := range burst {
		response, err := publish(env.client, env.server.URL, []model.Change{change})
		if err != nil {
			t.Fatalf("publish %d: %v", index, err)
		}
		finalVersion = response.Version
	}
	if finalVersion != int64(1+len(burst)) {
		t.Fatalf("final version = %d; want %d", finalVersion, 1+len(burst))
	}

	waitGroup.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.response.Version != finalVersion {
			t.Fatalf("client saw version %d; want final %d", result.response.Version, finalVersion)
		}
		if !result.response.Changed || len(result.response.Changes) != len(burst) {
			t.Fatalf("client response changed=%v changes=%d", result.response.Changed, len(result.response.Changes))
		}
		keys := make(map[string]struct{}, len(result.response.Changes))
		for _, item := range result.response.Changes {
			if item.Version <= initialVersion || item.Version > finalVersion {
				t.Fatalf("item version outside range: %+v", item)
			}
			keys[item.Key] = struct{}{}
		}
		if len(keys) != seed.ConfigItemCount {
			t.Fatalf("client received %d distinct keys; want %d", len(keys), seed.ConfigItemCount)
		}
	}

	timeoutResponse, err := getWatch(env.client, env.server.URL, finalVersion, "200ms")
	if err != nil {
		t.Fatal(err)
	}
	if timeoutResponse.Changed || timeoutResponse.Version != finalVersion || len(timeoutResponse.Changes) != 0 {
		t.Fatalf("timeout response = %+v", timeoutResponse)
	}

	statusCode, code, err := getWatchError(env.client, env.server.URL, finalVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	if statusCode != http.StatusConflict || code != "E_VERSION_AHEAD" {
		t.Fatalf("version-ahead response status=%d code=%s", statusCode, code)
	}

	snapshot, err := getSnapshot(env.client, env.server.URL, initialVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Config) != seed.ConfigItemCount {
		t.Fatalf("intermediate snapshot has %d items", len(snapshot.Config))
	}
	for key, version := range snapshot.ItemVersions {
		if version > initialVersion+1 {
			t.Fatalf("snapshot item %s has future version %d", key, version)
		}
	}

	valid := model.Change{Key: "order.timeout_ms", Value: "777"}
	invalid := model.Change{Key: "", Value: "broken"}
	rollbackStatus, rollbackCode, err := publishError(env.client, env.server.URL, []model.Change{valid, invalid})
	if err != nil {
		t.Fatal(err)
	}
	if rollbackStatus != http.StatusBadRequest || rollbackCode != "E_VALIDATION" {
		t.Fatalf("rollback response status=%d code=%s", rollbackStatus, rollbackCode)
	}
	current, err := getCurrent(env.client, env.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != finalVersion {
		t.Fatalf("rollback advanced version from %d to %d", finalVersion, current.Version)
	}

	waitResponse, err := getWatch(env.client, env.server.URL, finalVersion, "300ms")
	if err != nil {
		t.Fatal(err)
	}
	if waitResponse.Changed || waitResponse.Version != finalVersion {
		t.Fatalf("failed publish notified clients: %+v", waitResponse)
	}

	metricsText, err := getText(env.client, env.server.URL+"/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metricsText, `config_merged_broadcasts_total{namespace="orders"} 1`) {
		t.Fatalf("merged broadcast metric is not 1:\n%s", metricsText)
	}
	if !strings.Contains(metricsText, `config_merged_raw_changes_total{namespace="orders"} 23`) {
		t.Fatalf("raw change metric is not 23:\n%s", metricsText)
	}

	for i := 1; i <= 3; i++ {
		report := model.ClientReport{
			ClientID:            fmt.Sprintf("client-%d", i),
			Namespace:           seed.DefaultNamespace,
			KnownVersion:        finalVersion,
			AppliedVersion:      finalVersion,
			LastResponseVersion: finalVersion,
			AppliedHistory:      []int64{initialVersion, finalVersion},
		}
		reportBody, _ := json.Marshal(report)
		response, err := env.client.Post(
			env.server.URL+"/v1/clients/report",
			"application/json",
			bytes.NewReader(reportBody),
		)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("client report status = %d", response.StatusCode)
		}
	}
	status, err := getStatus(env.client, env.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Namespaces) != 1 || len(status.Namespaces[0].Clients) != 3 {
		t.Fatalf("status = %+v", status)
	}
	if status.Namespaces[0].RecentMerge == nil || status.Namespaces[0].RecentMerge.Version != finalVersion {
		t.Fatalf("recent merge = %+v", status.Namespaces[0].RecentMerge)
	}
	ids := make([]string, 0, len(status.Namespaces[0].Clients))
	for _, client := range status.Namespaces[0].Clients {
		ids = append(ids, client.ClientID)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("client status is not sorted: %v", ids)
	}
	itemKeys := make([]string, 0, len(status.Namespaces[0].RecentMerge.Items))
	for _, item := range status.Namespaces[0].RecentMerge.Items {
		itemKeys = append(itemKeys, item.Key)
	}
	if !sort.StringsAreSorted(itemKeys) {
		t.Fatalf("merge items are not sorted: %v", itemKeys)
	}
	metricsText, err = getText(env.client, env.server.URL+"/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metricsText, `config_client_applied_version_distribution{namespace="orders",version="24"} 3`) {
		t.Fatalf("client version distribution missing:\n%s", metricsText)
	}
}

func TestBatchPublishSharesOneVersionAndInvalidBatchRollsBack(t *testing.T) {
	env := newTestEnvironment(t, 300*time.Millisecond)

	response, err := publish(env.client, env.server.URL, []model.Change{
		{Key: "order.timeout_ms", Value: "1200"},
		{Key: "order.retry.max_attempts", Value: "4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Version != 2 {
		t.Fatalf("batch publish version = %d; want 2", response.Version)
	}

	snapshot, err := getSnapshot(env.client, env.server.URL, 2)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ItemVersions["order.timeout_ms"] != 2 || snapshot.ItemVersions["order.retry.max_attempts"] != 2 {
		t.Fatalf("batch did not share version 2: %#v", snapshot.ItemVersions)
	}
	if len(snapshot.Config) != seed.ConfigItemCount {
		t.Fatalf("snapshot has %d items", len(snapshot.Config))
	}

	statusCode, code, err := publishError(env.client, env.server.URL, []model.Change{
		{Key: "order.timeout_ms", Value: "1300"},
		{Key: "order.retry.max_attempts", Value: "5"},
		{Key: "", Value: "bad"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if statusCode != http.StatusBadRequest || code != "E_VALIDATION" {
		t.Fatalf("invalid batch status=%d code=%s", statusCode, code)
	}
	current, err := getCurrent(env.client, env.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != 2 {
		t.Fatalf("invalid batch advanced version to %d", current.Version)
	}
	config, versions, err := env.store.SnapshotAt(context.Background(), seed.DefaultNamespace, 2)
	if err != nil {
		t.Fatal(err)
	}
	if config["order.timeout_ms"] != "1200" || versions["order.timeout_ms"] != 2 {
		t.Fatalf("invalid batch partially wrote timeout: %#v %#v", config, versions)
	}
}

func TestLongPollWaitsForNewerPendingMerge(t *testing.T) {
	env := newTestEnvironment(t, 300*time.Millisecond)

	if _, err := publish(env.client, env.server.URL, []model.Change{
		{Key: "order.timeout_ms", Value: "1000"},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(450 * time.Millisecond)

	if _, err := publish(env.client, env.server.URL, []model.Change{
		{Key: "order.timeout_ms", Value: "1200"},
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	response, err := getWatch(env.client, env.server.URL, 1, "3s")
	if err != nil {
		t.Fatal(err)
	}
	if response.Version != 3 {
		t.Fatalf("long-poll returned version %d; want newest merged version 3", response.Version)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("long-poll returned a prior broadcast after %s instead of waiting for the pending merge", elapsed)
	}
	for _, item := range response.Changes {
		if item.Key == "order.timeout_ms" && item.Value != "1200" {
			t.Fatalf("long-poll leaked intermediate timeout value %q", item.Value)
		}
	}
}

func publish(client *http.Client, baseURL string, changes []model.Change) (model.PublishResponse, error) {
	body, err := json.Marshal(model.PublishRequest{Changes: changes})
	if err != nil {
		return model.PublishResponse{}, err
	}
	response, err := client.Post(
		baseURL+"/v1/namespaces/"+seed.DefaultNamespace+"/publish",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return model.PublishResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		return model.PublishResponse{}, fmt.Errorf("publish status %d: %s", response.StatusCode, payload)
	}
	var result model.PublishResponse
	return result, json.NewDecoder(response.Body).Decode(&result)
}

func publishError(client *http.Client, baseURL string, changes []model.Change) (int, string, error) {
	body, _ := json.Marshal(model.PublishRequest{Changes: changes})
	response, err := client.Post(
		baseURL+"/v1/namespaces/"+seed.DefaultNamespace+"/publish",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	var apiError model.ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&apiError); err != nil {
		return response.StatusCode, "", err
	}
	return response.StatusCode, apiError.Code, nil
}

func getWatch(client *http.Client, baseURL string, version int64, timeout string) (model.WatchResponse, error) {
	requestURL := fmt.Sprintf(
		"%s/v1/namespaces/%s/watch?version=%d&timeout=%s",
		baseURL,
		seed.DefaultNamespace,
		version,
		timeout,
	)
	response, err := client.Get(requestURL)
	if err != nil {
		return model.WatchResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		return model.WatchResponse{}, fmt.Errorf("watch status %d: %s", response.StatusCode, payload)
	}
	var result model.WatchResponse
	return result, json.NewDecoder(response.Body).Decode(&result)
}

func getWatchError(client *http.Client, baseURL string, version int64) (int, string, error) {
	response, err := client.Get(fmt.Sprintf(
		"%s/v1/namespaces/%s/watch?version=%d&timeout=200ms",
		baseURL,
		seed.DefaultNamespace,
		version,
	))
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	var apiError model.ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&apiError); err != nil {
		return response.StatusCode, "", err
	}
	return response.StatusCode, apiError.Code, nil
}

func getSnapshot(client *http.Client, baseURL string, version int64) (model.SnapshotResponse, error) {
	response, err := client.Get(fmt.Sprintf(
		"%s/v1/namespaces/%s/snapshots/%d",
		baseURL,
		seed.DefaultNamespace,
		version,
	))
	if err != nil {
		return model.SnapshotResponse{}, err
	}
	defer response.Body.Close()
	var result model.SnapshotResponse
	return result, json.NewDecoder(response.Body).Decode(&result)
}

func getCurrent(client *http.Client, baseURL string) (model.CurrentResponse, error) {
	response, err := client.Get(baseURL + "/v1/namespaces/" + seed.DefaultNamespace + "/current")
	if err != nil {
		return model.CurrentResponse{}, err
	}
	defer response.Body.Close()
	var result model.CurrentResponse
	return result, json.NewDecoder(response.Body).Decode(&result)
}

func getStatus(client *http.Client, baseURL string) (server.StatusResponse, error) {
	response, err := client.Get(baseURL + "/v1/status")
	if err != nil {
		return server.StatusResponse{}, err
	}
	defer response.Body.Close()
	var result server.StatusResponse
	return result, json.NewDecoder(response.Body).Decode(&result)
}

func getText(client *http.Client, requestURL string) (string, error) {
	response, err := client.Get(requestURL)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	return string(payload), err
}
