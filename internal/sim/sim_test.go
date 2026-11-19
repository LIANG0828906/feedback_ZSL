package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig() *Config {
	cfg := SampleConfig()
	for i := range cfg.Resolvers {
		cfg.Resolvers[i].Address = "127.0.0.1:0"
	}
	cfg.Limits.DefaultTimeoutMS = 500
	if err := PrepareConfig(cfg); err != nil {
		panic(err)
	}
	return cfg
}

func testService(t *testing.T, mutate func(*Config)) *Service {
	t.Helper()
	cfg := testConfig()
	if mutate != nil {
		mutate(cfg)
	}
	service, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service
}

func TestTraceFollowsDelegationChain(t *testing.T) {
	service := testService(t, nil)
	result, err := service.Trace(context.Background(), TraceRequest{
		Site: "shanghai", Domain: "www.example.test", Type: "A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Conclusion != "SUCCESS" || !result.ConsistentWithAuthority {
		t.Fatalf("unexpected result: %+v", result)
	}
	var resolvers []string
	for _, hop := range result.Hops {
		resolvers = append(resolvers, hop.Resolver)
	}
	want := []string{"rec-shanghai", "root", "tld", "auth"}
	if !reflect.DeepEqual(resolvers, want) {
		t.Fatalf("resolvers = %v, want %v", resolvers, want)
	}
	if result.Hops[0].CacheStatus != "miss" || result.Hops[3].RCode != "NOERROR" {
		t.Fatalf("unexpected hops: %+v", result.Hops)
	}
}

func TestCacheTTLHitAndExpiry(t *testing.T) {
	service := testService(t, func(cfg *Config) {
		cfg.Zones[0].Records = append(cfg.Zones[0].Records, Record{
			Name: "ttl.example.test.", Type: "A", TTL: 1, Value: "10.20.0.20",
		})
		if err := PrepareConfig(cfg); err != nil {
			panic(err)
		}
	})
	query := TraceRequest{Site: "shanghai", Domain: "ttl.example.test", Type: "A"}
	first, err := service.Trace(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hops[0].CacheStatus != "miss" {
		t.Fatalf("first cache status = %s", first.Hops[0].CacheStatus)
	}
	second, err := service.Trace(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Hops) != 1 || second.Hops[0].CacheStatus != "hit" {
		t.Fatalf("expected a cache hit only, got %+v", second.Hops)
	}
	if second.Hops[0].TTL != 1 {
		t.Fatalf("cached TTL = %d, want 1", second.Hops[0].TTL)
	}
	time.Sleep(1100 * time.Millisecond)
	third, err := service.Trace(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Hops) < 4 || third.Hops[0].CacheStatus != "miss" {
		t.Fatalf("expected cache expiry and a full chain, got %+v", third.Hops)
	}
}

func TestStaticMappingAndClear(t *testing.T) {
	service := testService(t, nil)
	query := TraceRequest{Site: "nanjing", Domain: "www.example.test", Type: "A"}
	before, err := service.Trace(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if before.Conclusion != "INCONSISTENT" || before.MismatchCause != "static_mapping" {
		t.Fatalf("unexpected static result: %+v", before)
	}
	if before.Attribution == nil || before.Attribution.Role != "hosts" {
		t.Fatalf("static attribution = %+v", before.Attribution)
	}
	removed, err := service.ClearStaticMappings("nanjing")
	if err != nil || removed != 1 {
		t.Fatalf("clear mapping: removed=%d err=%v", removed, err)
	}
	after, err := service.Trace(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if after.Conclusion != "SUCCESS" || len(after.Resolution.Values) != 1 || after.Resolution.Values[0] != "10.20.0.10" {
		t.Fatalf("after clearing mapping: %+v", after)
	}
	if after.Attribution != nil {
		t.Fatalf("unexpected attribution after clearing mapping: %+v", after.Attribution)
	}
}

func TestNegativeCacheIsDistinguished(t *testing.T) {
	service := testService(t, nil)
	result, err := service.Trace(context.Background(), TraceRequest{
		Site: "beijing", Domain: "api.example.test", Type: "A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Conclusion != "INCONSISTENT" || result.MismatchCause != "negative_cache" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Attribution == nil || result.Attribution.Anomaly != "negative_cache" {
		t.Fatalf("unexpected attribution: %+v", result.Attribution)
	}
}

func TestNegativeCacheExpires(t *testing.T) {
	service := testService(t, func(cfg *Config) {
		for i := range cfg.Sites {
			if cfg.Sites[i].ID == "beijing" {
				cfg.Sites[i].NegativeCache[0].TTL = 1
			}
		}
		if err := PrepareConfig(cfg); err != nil {
			panic(err)
		}
	})
	query := TraceRequest{Site: "beijing", Domain: "api.example.test", Type: "A"}
	before, err := service.Trace(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if before.MismatchCause != "negative_cache" {
		t.Fatalf("unexpected pre-expiry result: %+v", before)
	}
	time.Sleep(1100 * time.Millisecond)
	after, err := service.Trace(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if after.Conclusion != "SUCCESS" || len(after.Resolution.Values) != 1 || after.Resolution.Values[0] != "10.20.0.11" {
		t.Fatalf("negative cache did not expire: %+v", after)
	}
}

func TestUDPTruncationKeepsBothAttempts(t *testing.T) {
	service := testService(t, nil)
	result, err := service.Trace(context.Background(), TraceRequest{
		Site: "shanghai", Domain: "big.example.test", Type: "TXT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Conclusion != "SUCCESS" {
		t.Fatalf("conclusion = %s", result.Conclusion)
	}
	var udp, tcp bool
	for _, hop := range result.Hops {
		if hop.Resolver == "auth" && hop.Transport == "udp" && hop.Truncated {
			udp = true
		}
		if hop.Resolver == "auth" && hop.Transport == "tcp" && !hop.Truncated {
			tcp = true
		}
	}
	if !udp || !tcp {
		t.Fatalf("missing truncation/TCP fallback records: %+v", result.Hops)
	}
}

func TestFaultAttributionUsesEarliestFaultHop(t *testing.T) {
	service := testService(t, func(cfg *Config) {
		for i := range cfg.Faults {
			if cfg.Faults[i].ID == "root-wrong-delegation" {
				cfg.Faults[i].WrongAddress = ""
				cfg.Faults[i].WrongTarget = "tld"
				cfg.Faults[i].Domain = "both.example.test."
			}
		}
		cfg.Zones[0].Records = append(cfg.Zones[0].Records,
			Record{Name: "both.example.test.", Type: "A", TTL: 60, Value: "10.20.0.30"})
		cfg.Faults = append(cfg.Faults, Fault{
			ID: "tld-both", Resolver: "tld", Domain: "both.example.test.", Type: "servfail",
		})
		if err := PrepareConfig(cfg); err != nil {
			panic(err)
		}
	})
	result, err := service.Trace(context.Background(), TraceRequest{
		Site: "shanghai", Domain: "both.example.test", Type: "A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Attribution == nil {
		t.Fatal("expected attribution")
	}
	if result.Attribution.Resolver != "root" || result.Attribution.Anomaly != "wrong_delegation" {
		t.Fatalf("attribution = %+v", result.Attribution)
	}
}

func TestTransientTimeoutRecoversButRemainsAttributed(t *testing.T) {
	service := testService(t, nil)
	result, err := service.Trace(context.Background(), TraceRequest{
		Site: "wuhan", Domain: "broken.example.test", Type: "A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Conclusion != "SUCCESS" {
		t.Fatalf("conclusion = %s", result.Conclusion)
	}
	if result.Attribution == nil || result.Attribution.Resolver != "tld" || result.Attribution.Anomaly != "timeout" {
		t.Fatalf("attribution = %+v", result.Attribution)
	}
}

func TestNoFaultMatchDoesNotInventAttribution(t *testing.T) {
	service := testService(t, nil)
	result, err := service.Trace(context.Background(), TraceRequest{
		Site: "shanghai", Domain: "api.example.test", Type: "A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Attribution != nil {
		t.Fatalf("unexpected attribution: %+v", result.Attribution)
	}
}

func TestValidationRejectsForwardCycleBeforeBind(t *testing.T) {
	cfg := testConfig()
	for i := range cfg.Resolvers {
		if cfg.Resolvers[i].ID == "rec-shanghai" {
			cfg.Resolvers[i].Upstreams = []string{"rec-shenzhen"}
		}
		if cfg.Resolvers[i].ID == "fwd-shenzhen" {
			cfg.Resolvers[i].Upstreams = []string{"rec-shanghai"}
		}
	}
	_, err := NewService(cfg)
	if err == nil {
		t.Fatal("expected validation error")
	}
	be := asBusinessError(err)
	if be.Code != CodeConfigForwardCycle {
		t.Fatalf("code = %s, want %s", be.Code, CodeConfigForwardCycle)
	}
}

func TestValidationRejectsMissingResolver(t *testing.T) {
	cfg := testConfig()
	cfg.Sites[0].Recursive = "does-not-exist"
	_, err := NewService(cfg)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if asBusinessError(err).Code != CodeConfigMissingRef {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeterministicResultExceptRTT(t *testing.T) {
	run := func() TraceResult {
		service := testService(t, nil)
		result, err := service.Trace(context.Background(), TraceRequest{
			Site: "shenzhen", Domain: "www.example.test", Type: "A",
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := run()
	second := run()
	for i := range first.Hops {
		first.Hops[i].RTTMS = 0
		second.Hops[i].RTTMS = 0
		// Port 0 lets the OS choose a different ephemeral port for each run.
		first.Hops[i].Server = ""
		second.Hops[i].Server = ""
	}
	if !reflect.DeepEqual(first, second) {
		firstJSON, _ := json.MarshalIndent(first, "", "  ")
		secondJSON, _ := json.MarshalIndent(second, "", "  ")
		t.Fatalf("results differ:\n%s\n---\n%s", firstJSON, secondJSON)
	}
}

func TestConcurrentTracesRespectServerLimit(t *testing.T) {
	service := testService(t, func(cfg *Config) {
		cfg.Limits.MaxConcurrentQueries = 4
		cfg.Limits.MaxConcurrentTraces = 3
		cfg.Limits.TraceQueueSize = 32
		if err := PrepareConfig(cfg); err != nil {
			panic(err)
		}
	})
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		site := []string{"shanghai", "shenzhen", "hangzhou"}[i%3]
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.Trace(context.Background(), TraceRequest{Site: site, Domain: "api.example.test", Type: "A"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent trace failed: %v", err)
		}
	}
	stats := service.Stats()
	peak := stats["peak_queries"].(int64)
	if peak > 4 {
		t.Fatalf("peak queries = %d, limit 4", peak)
	}
}

func TestShutdownReleasesEveryListener(t *testing.T) {
	cfg := testConfig()
	service, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	addresses := service.Engine().Addresses()
	service.Close()
	for id, address := range addresses {
		udpAddr, err := net.ResolveUDPAddr("udp", address)
		if err != nil {
			t.Fatal(err)
		}
		udp, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			t.Fatalf("UDP %s (%s) was not released: %v", id, address, err)
		}
		_ = udp.Close()
		tcp, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("TCP %s (%s) was not released: %v", id, address, err)
		}
		_ = tcp.Close()
	}
}

func TestHTTPTraceAndStaticMappingAPI(t *testing.T) {
	service := testService(t, nil)
	server := httptest.NewServer(NewAPI(service, nil).Handler())
	defer server.Close()

	traceBody := `{"site":"nanjing","domain":"www.example.test","type":"A"}`
	response, err := http.Post(server.URL+"/api/v1/traces", "application/json", strings.NewReader(traceBody))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var first TraceResult
	if err := json.NewDecoder(response.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first.MismatchCause != "static_mapping" {
		t.Fatalf("unexpected first result: %+v", first)
	}

	request, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/sites/nanjing/static-mappings?all=true", nil)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", response.StatusCode)
	}

	response, err = http.Post(server.URL+"/api/v1/traces", "application/json", strings.NewReader(traceBody))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var second TraceResult
	if err := json.NewDecoder(response.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.Conclusion != "SUCCESS" || second.MismatchCause != "none" {
		t.Fatalf("unexpected second result: %+v", second)
	}
}

func TestBusinessErrorJSONIsStable(t *testing.T) {
	err := bizError(CodeBusy, "trace", "trace queue is full")
	data, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	want := fmt.Sprintf(`{"code":"%s","message":"trace queue is full","field":"trace"}`, CodeBusy)
	if string(data) != want {
		t.Fatalf("JSON = %s, want %s", data, want)
	}
}
