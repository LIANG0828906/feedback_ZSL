package samplegen_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"uvenhance/internal/samplegen"
)

func TestGeneratorIsDeterministic(t *testing.T) {
	root := t.TempDir()
	cfg := samplegen.Config{ID: "deterministic", Width: 96, Height: 64}
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if _, err := samplegen.Generate(context.Background(), a, cfg, 4242); err != nil {
		t.Fatal(err)
	}
	if _, err := samplegen.Generate(context.Background(), b, cfg, 4242); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"metadata.json", "Y.plane", "U.plane", "V.plane"} {
		left, err := os.ReadFile(filepath.Join(a, name))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(b, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(left) != string(right) {
			t.Fatalf("%s differs between identical seeds", name)
		}
	}
}
