package tasks

import (
	"testing"
)

func TestGenerateDeterministicShape(t *testing.T) {
	first := Generate(DefaultSeed, 20, 100)
	second := Generate(DefaultSeed, 20, 100)
	if len(first) != 20 {
		t.Fatalf("batch count = %d, want 20", len(first))
	}
	taskCount := 0
	for index := range first {
		if first[index].ID != second[index].ID {
			t.Fatalf("batch %d id changed: %s != %s", index, first[index].ID, second[index].ID)
		}
		if len(first[index].Tasks) != 100 {
			t.Fatalf("batch %s task count = %d, want 100", first[index].ID, len(first[index].Tasks))
		}
		for taskIndex := range first[index].Tasks {
			if first[index].Tasks[taskIndex] != second[index].Tasks[taskIndex] {
				t.Fatalf("task mismatch at batch %d task %d", index, taskIndex)
			}
		}
		taskCount += len(first[index].Tasks)
	}
	if taskCount != 2000 {
		t.Fatalf("task count = %d, want 2000", taskCount)
	}
}

func TestDifferentSeedChangesTasks(t *testing.T) {
	first := Generate(1, 1, 10)
	second := Generate(2, 1, 10)
	equal := true
	for index := range first[0].Tasks {
		if first[0].Tasks[index] != second[0].Tasks[index] {
			equal = false
			break
		}
	}
	if equal {
		t.Fatal("different seeds produced identical tasks")
	}
}

func TestBuildResultStable(t *testing.T) {
	batch := Generate(DefaultSeed, 20, 100)[0]
	first, err := BuildResult(batch)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildResult(batch)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest || first.PayloadJSON != second.PayloadJSON {
		t.Fatal("result changed for identical batch input")
	}
	if first.TaskCount != 100 || first.TotalAmount <= 0 {
		t.Fatalf("unexpected result: count=%d total=%d", first.TaskCount, first.TotalAmount)
	}
}
