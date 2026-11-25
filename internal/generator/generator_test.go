package generator_test

import (
	"bytes"
	"testing"

	"skeletonretarget/internal/codec"
	"skeletonretarget/internal/generator"
)

func TestGeneratorIsDeterministicAndTruthIsAnalyticSample(t *testing.T) {
	first, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Studio, second.Studio) ||
		!bytes.Equal(first.Engine, second.Engine) ||
		!bytes.Equal(first.Legacy, second.Legacy) {
		t.Fatal("fixed seed did not produce byte-identical formats")
	}
	different, err := generator.Generate(43)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Studio, different.Studio) {
		t.Fatal("different seeds produced the same studio document")
	}
	doc, err := codec.Load(first.Studio)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Truth) != 30 {
		t.Fatalf("expected five sampled frames x six target joints of truth, got %d", len(doc.Truth))
	}
}
