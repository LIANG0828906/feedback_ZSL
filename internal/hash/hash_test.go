package hash_test

import (
	"testing"

	"github.com/gameaudio/workbench/internal/generator"
	"github.com/gameaudio/workbench/internal/hash"
	"github.com/gameaudio/workbench/internal/model"
)

func TestContentStableAndSensitive(t *testing.T) {
	h1 := hash.Content(map[string]int{"a": 1, "b": 2})
	h2 := hash.Content(map[string]int{"a": 1, "b": 2})
	if h1 != h2 || len(h1) != 64 {
		t.Fatal("identical content must yield stable sha256 hex")
	}
	h3 := hash.Content(map[string]int{"a": 1, "b": 3})
	if h1 == h3 {
		t.Fatal("changed content must change hash")
	}
}

func TestPairHashChangesWithRegionParam(t *testing.T) {
	p := generator.Generate()
	s, l, region := p.Sources[0], p.Listeners[0], p.Regions[0]
	in := hash.PairInput{
		Source: s, Listener: l, Region: region,
		Walls: p.Walls, ScaleBits: 4, AlgoVersion: "algo-1",
	}
	h1 := hash.PairHash(in)
	region.ReverbTime = 9.9
	in.Region = region
	h2 := hash.PairHash(in)
	if h1 == h2 {
		t.Fatal("region param edit must change pair input hash")
	}
}

func TestWallFingerprintOrderIndependent(t *testing.T) {
	p := generator.Generate()
	h1 := hash.WallFingerprint(p.Walls)
	rev := make([]model.Wall, len(p.Walls))
	for i, w := range p.Walls {
		rev[len(p.Walls)-1-i] = w
	}
	h2 := hash.WallFingerprint(rev)
	if h1 != h2 {
		t.Fatal("wall fingerprint must be order independent")
	}
}
