package dialoguecal

import (
	"bytes"
	"reflect"
	"testing"
)

func TestGeneratorIsReproducibleForFixedSeed(t *testing.T) {
	a := mustGenerate(t, 314159)
	b := mustGenerate(t, 314159)
	if a.Project.SemanticFingerprint() != b.Project.SemanticFingerprint() {
		t.Fatal("same seed produced different project fingerprints")
	}
	if !reflect.DeepEqual(a.Truth, b.Truth) {
		t.Fatal("same seed produced different frame truth")
	}
	for _, v := range []SchemaVersion{VersionV1, VersionV2, VersionV3} {
		if !bytes.Equal(a.Docs[v], b.Docs[v]) {
			t.Fatalf("same seed produced different %s document bytes", v)
		}
	}
	c := mustGenerate(t, 314160)
	if a.Project.SemanticFingerprint() == c.Project.SemanticFingerprint() {
		t.Fatal("different seeds produced identical projects")
	}
	if bytes.Equal(a.Docs[VersionV3], c.Docs[VersionV3]) {
		t.Fatal("different seeds produced identical v3 documents")
	}
}
