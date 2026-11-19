package priority

import (
	"net/http"
	"testing"

	"feedback_zsl/internal/config"
)

func mapper() *Mapper {
	return New(&config.Config{
		DefaultPrio: "normal",
		Priorities: []config.PriorityConfig{
			{Name: "high", Rank: 0, H2WeightMin: 192, H2WeightMax: 256, H3Urgency: []int{0, 1}, HeaderValues: []string{"high"}},
			{Name: "normal", Rank: 10, H2WeightMin: 64, H2WeightMax: 191, H3Urgency: []int{2, 3, 4}, HeaderValues: []string{"normal"}},
			{Name: "low", Rank: 20, H2WeightMin: 1, H2WeightMax: 63, H3Urgency: []int{5, 6, 7}, HeaderValues: []string{"low"}},
		},
	})
}

func TestH2WeightMapping(t *testing.T) {
	m := mapper()
	if got := m.FromH2Weight(255, http.Header{}).Priority.Name; got != "high" {
		t.Fatalf("weight 256 mapped to %s", got)
	}
	if got := m.FromH2Weight(100, http.Header{}).Priority.Name; got != "normal" {
		t.Fatalf("weight 101 mapped to %s", got)
	}
	if got := m.FromH2Weight(10, http.Header{}).Priority.Name; got != "low" {
		t.Fatalf("weight 11 mapped to %s", got)
	}
}

func TestRFC9218HeaderMapping(t *testing.T) {
	m := mapper()
	h := http.Header{}
	h.Set("Priority", "u=6")
	if got, _ := m.FromHeaders(h); got.Priority.Name != "low" {
		t.Fatalf("u=6 mapped to %s", got.Priority.Name)
	}
	h.Set("Priority", "u=0")
	if got, _ := m.FromHeaders(h); got.Priority.Name != "high" {
		t.Fatalf("u=0 mapped to %s", got.Priority.Name)
	}
}

func TestExplicitClassWins(t *testing.T) {
	h := http.Header{}
	h.Set("Priority", "u=7")
	h.Set("X-Gateway-Priority", "high")
	if got, _ := mapper().FromHeaders(h); got.Priority.Name != "high" {
		t.Fatalf("explicit class mapped to %s", got.Priority.Name)
	}
}
