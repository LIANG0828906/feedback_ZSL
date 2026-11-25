package generator

import (
	"reflect"
	"testing"
)

func TestGenerateIsDeterministic(t *testing.T) {
	a, err := Generate(Seed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(Seed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.VersionJSON, b.VersionJSON) {
		t.Fatal("version JSON differs for the same seed")
	}
	if !reflect.DeepEqual(a.Truth, b.Truth) {
		t.Fatal("reference truth differs for the same seed")
	}
	for _, name := range a.VersionOrder {
		if len(a.VersionJSON[name]) == 0 {
			t.Fatalf("%s document is empty", name)
		}
	}
}
