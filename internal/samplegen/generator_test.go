package samplegen_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"inputkernel/internal/samplegen"
)

func TestGeneratorIsDeterministicAndSeedSensitive(t *testing.T) {
	a, err := samplegen.Generate(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := samplegen.Generate(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Fatal("same seed produced different samples")
	}
	c, err := samplegen.Generate(samplegen.DefaultSeed + 1)
	if err != nil {
		t.Fatal(err)
	}
	jc, _ := json.Marshal(c)
	if bytes.Equal(ja, jc) {
		t.Fatal("different seeds produced identical samples")
	}
}

func TestWriteIsByteReproducible(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	if _, err := samplegen.Write(first, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	if _, err := samplegen.Write(second, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		a, err := os.ReadFile(filepath.Join(first, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(second, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("%s differs between identical generation runs", entry.Name())
		}
	}
}
