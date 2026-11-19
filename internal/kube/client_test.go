package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestMergeEnvPreservesUnchangedOrder(t *testing.T) {
	current := []corev1.EnvVar{
		{Name: "Z", Value: "1"},
		{Name: "A", Value: "old"},
		{Name: "EXTRA", Value: "remove"},
	}
	expected := []corev1.EnvVar{
		{Name: "A", Value: "new"},
		{Name: "Z", Value: "1"},
		{Name: "NEW", Value: "2"},
	}
	merged, changed := mergeEnv(current, expected)
	if !changed {
		t.Fatal("expected env change")
	}
	if len(merged) != 3 {
		t.Fatalf("unexpected env length: %+v", merged)
	}
	got := []string{merged[0].Name, merged[1].Name, merged[2].Name}
	want := []string{"Z", "A", "NEW"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("env order changed: got %v want %v", got, want)
		}
	}
	if merged[1].Value != "new" {
		t.Fatalf("env value not updated: %+v", merged)
	}
}

func TestMergeEnvIgnoresOrderOnlyDifference(t *testing.T) {
	current := []corev1.EnvVar{{Name: "Z", Value: "1"}, {Name: "A", Value: "2"}}
	expected := []corev1.EnvVar{{Name: "A", Value: "2"}, {Name: "Z", Value: "1"}}
	merged, changed := mergeEnv(current, expected)
	if changed {
		t.Fatal("order-only difference must not trigger a pod template update")
	}
	if merged[0].Name != "Z" {
		t.Fatalf("current order should be preserved: %+v", merged)
	}
}
