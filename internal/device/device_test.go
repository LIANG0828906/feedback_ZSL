package device_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"audioforge/internal/device"
	"audioforge/internal/engine"
	"audioforge/internal/errs"
	"audioforge/internal/generator"
	"audioforge/internal/model"
)

func TestLoopbackEchoAlignmentDetectsMissAndDuplicate(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 4, Workers: 2, BufferPerWorker: 2}
	built, err := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	service, err := device.NewService(bundle.Capability)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	client := device.NewClient(server.URL)
	if err := client.SetFaults(context.Background(), device.FaultPlan{
		Drop:      []string{"0/1"},
		Duplicate: []string{"0/2"},
		DelayMS:   map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	exchange, err := client.Send(context.Background(), built.Packets)
	if err != nil {
		t.Fatal(err)
	}
	alignment := device.Align(exchange, 2*time.Second, exchange.CompletedAt.Add(3*time.Second))
	if len(alignment.Matches) == 0 {
		t.Fatal("expected aligned packet matches")
	}
	codes := map[errs.Code]bool{}
	for _, issue := range alignment.Issues {
		codes[issue.Code] = true
		if issue.FirstSeen.IsZero() {
			t.Fatalf("issue has no first-seen time: %+v", issue)
		}
	}
	if !codes[errs.EMiss] || !codes[errs.EDup] {
		t.Fatalf("missing expected anomalies: %+v", alignment.Issues)
	}
}

func TestTimeoutAnomalyUsesFirstEchoTime(t *testing.T) {
	now := time.Now().UTC()
	exchange := device.Exchange{
		Local: []device.LocalRecord{{
			Packet: model.Packet{Seq: 3, Channel: 1, Samples: []int64{1, 2}},
			SentAt: now.Add(-2 * time.Second),
		}},
		Echoes: []device.Echo{{
			Seq:       3,
			Channel:   1,
			Samples:   []int64{1, 2},
			AppliedAt: now.Add(-500 * time.Millisecond),
		}},
		CompletedAt: now,
	}
	alignment := device.Align(exchange, 500*time.Millisecond, now)
	if len(alignment.Issues) != 1 || !errs.IsCode(alignment.Issues[0], errs.ETimeout) {
		t.Fatalf("expected one timeout, got %+v", alignment.Issues)
	}
	if alignment.Issues[0].PacketSeq != 3 || alignment.Issues[0].Channel != 1 || alignment.Issues[0].FirstSeen.IsZero() {
		t.Fatalf("unexpected anomaly context: %+v", alignment.Issues[0])
	}
}

func TestServiceBindsLoopbackOnly(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	service, err := device.NewService(bundle.Capability)
	if err != nil {
		t.Fatal(err)
	}
	baseURL, err := service.Start(0)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if len(baseURL) < 17 || baseURL[:17] != "http://127.0.0.1:" {
		t.Fatalf("service did not bind loopback: %s", baseURL)
	}
}
