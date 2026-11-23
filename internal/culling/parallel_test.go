package culling_test

import (
	"context"
	"reflect"
	"testing"

	"occlbench/internal/culling"
	"occlbench/internal/generator"
	"occlbench/internal/worker"
)

func TestParallelEqualsSerial(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	pool, err := worker.New(4, 256*256)
	if err != nil {
		t.Fatal(err)
	}
	eng := culling.NewEngine()
	params := culling.DefaultPreciseParams()

	serial, err := eng.Run(w, params)
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := eng.RunParallel(context.Background(), w, params, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serial.Samples, parallel.Samples) {
		t.Fatal("parallel culling result differs from serial (pure-function violation)")
	}
}
