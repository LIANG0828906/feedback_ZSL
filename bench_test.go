package spatialpick

import "testing"

func BenchmarkPickRay(b *testing.B) {
	scene, _, err := GenerateSample()
	if err != nil {
		b.Fatal(err)
	}
	e, err := NewEngine(scene)
	if err != nil {
		b.Fatal(err)
	}
	idx := e.Index()
	query := scene.RayQueries[0]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.PickRay(query, scene.Camera); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSelectBox(b *testing.B) {
	scene, _, err := GenerateSample()
	if err != nil {
		b.Fatal(err)
	}
	e, err := NewEngine(scene)
	if err != nil {
		b.Fatal(err)
	}
	idx := e.Index()
	query := scene.BoxQueries[1]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.SelectBox(query, scene.Camera); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPivotCommit(b *testing.B) {
	scene, _, err := GenerateSample()
	if err != nil {
		b.Fatal(err)
	}
	batch := scene.EditSequence[0]
	e, err := NewEngine(scene)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		plan, err := e.PlanPivotBatch(batch)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := e.CommitBatch(plan); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFullBuild(b *testing.B) {
	scene, _, err := GenerateSample()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NewEngine(scene); err != nil {
			b.Fatal(err)
		}
	}
}
