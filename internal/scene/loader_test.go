package scene_test

import (
	"testing"

	"occlbench/internal/core"
	"occlbench/internal/generator"
	"occlbench/internal/scene"
)

func cloneWorld() *scene.World {
	w := generator.Generate(generator.DefaultConfig())
	return w
}

func TestValidWorldPasses(t *testing.T) {
	if err := cloneWorld().Validate(); err != nil {
		t.Fatalf("valid world rejected: %v", err)
	}
}

func TestSchemaErrorOnMissingField(t *testing.T) {
	w := cloneWorld()
	w.SchemaVersion = ""
	err := w.Validate()
	if core.CodeOf(err) != core.ESchema {
		t.Fatalf("want E_SCHEMA got %v", err)
	}
}

func TestBoxErrorLocatesPartitionAndChunk(t *testing.T) {
	w := cloneWorld()
	w.Partitions[1].Chunks[0].Box.Max.X = w.Partitions[1].Chunks[0].Box.Min.X - 1
	err := w.Validate()
	if core.CodeOf(err) != core.EBox {
		t.Fatalf("want E_BOX got %v", err)
	}
	ce := err.(*core.CodedError)
	if ce.Fields["partition"] != 1 {
		t.Fatalf("error should locate partition=1, got %v", ce.Fields)
	}
	if _, ok := ce.Fields["chunk"]; !ok {
		t.Fatalf("error should locate chunk id, got %v", ce.Fields)
	}
}

func TestRefErrorOnOutOfRangeOccluder(t *testing.T) {
	w := cloneWorld()
	w.Partitions[0].Chunks[0].OccluderRefs = []int{999}
	err := w.Validate()
	if core.CodeOf(err) != core.ERef {
		t.Fatalf("want E_REF got %v", err)
	}
}

func TestTrajectoryMonotonic(t *testing.T) {
	w := cloneWorld()
	w.Trajectory.Samples[3].T = w.Trajectory.Samples[2].T
	err := w.Validate()
	if core.CodeOf(err) != core.ESchema {
		t.Fatalf("want E_SCHEMA for non-monotonic time got %v", err)
	}
}

func TestNonFiniteBoxRejected(t *testing.T) {
	w := cloneWorld()
	w.Partitions[0].Chunks[0].Box.Min.Y = float64(0)
	w.Partitions[0].Chunks[0].Box.Max.Y = float64(1)
	// 引入 NaN。
	w.Partitions[0].Chunks[0].Box.Max.Z = float64(0) * float64(1) // keep finite baseline
	w.Partitions[0].Chunks[0].Box.Max.Z = nan()
	err := w.Validate()
	if core.CodeOf(err) != core.EBox {
		t.Fatalf("want E_BOX for non-finite got %v", err)
	}
}

func nan() float64 {
	var z float64
	return z / z
}
