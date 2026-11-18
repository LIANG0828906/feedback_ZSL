package business

import (
	"errors"
	"reflect"
	"testing"

	"configcenter/internal/model"
)

func TestStateAppliesMonotonicallyAndRejectsStaleResponses(t *testing.T) {
	t.Parallel()

	state := NewState()
	result, err := state.Apply(model.WatchResponse{
		Namespace:   "orders",
		BaseVersion: 0,
		Version:     1,
		Changed:     true,
		Changes: []model.Item{
			{Key: "order.timeout_ms", Value: "800", Version: 1},
			{Key: "order.retry", Value: "3", Version: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Stale {
		t.Fatalf("initial apply result = %+v", result)
	}

	result, err = state.Apply(model.WatchResponse{
		Namespace:   "orders",
		BaseVersion: 1,
		Version:     3,
		Changed:     true,
		Changes: []model.Item{
			{Key: "order.retry", Value: "5", Version: 3},
			{Key: "order.timeout_ms", Value: "1200", Version: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied {
		t.Fatalf("expected apply, got %+v", result)
	}
	expectedKeys := []string{"order.timeout_ms", "order.retry"}
	if !reflect.DeepEqual(result.Keys, expectedKeys) {
		t.Fatalf("apply order = %v; want %v", result.Keys, expectedKeys)
	}

	result, err = state.Apply(model.WatchResponse{
		Namespace:   "orders",
		BaseVersion: 2,
		Version:     2,
		Changed:     true,
		Changes:     []model.Item{{Key: "order.retry", Value: "stale", Version: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stale || result.Applied {
		t.Fatalf("stale response result = %+v", result)
	}

	version, values, history, stale := state.Snapshot()
	if version != 3 {
		t.Fatalf("version = %d; want 3", version)
	}
	if values["order.timeout_ms"] != "1200" || values["order.retry"] != "5" {
		t.Fatalf("values = %#v", values)
	}
	if !reflect.DeepEqual(history, []int64{1, 3}) {
		t.Fatalf("history = %v", history)
	}
	if stale != 1 {
		t.Fatalf("stale = %d; want 1", stale)
	}

	_, err = state.Apply(model.WatchResponse{
		Namespace:   "orders",
		BaseVersion: 3,
		Version:     4,
		Changed:     true,
		Changes: []model.Item{
			{Key: "order.timeout_ms", Value: "1500", Version: 2},
		},
	})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("expected protocol error, got %v", err)
	}
	version, values, history, _ = state.Snapshot()
	if version != 3 || values["order.timeout_ms"] != "1200" || !reflect.DeepEqual(history, []int64{1, 3}) {
		t.Fatalf("invalid response partially applied: version=%d values=%#v history=%v", version, values, history)
	}
}
