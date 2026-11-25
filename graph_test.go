package textureconv

import (
	"testing"
)

func TestDependencyGraphAndLocalizedInvalidation(t *testing.T) {
	b := testBundle(t, SamplePlan())
	im := b.Images["rgba8"]
	g, err := BuildDependencyGraph(b.Plan, im, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != len(b.Plan.Entries) {
		t.Fatalf("node count %d != entries %d", len(g.Nodes), len(b.Plan.Entries))
	}
	for i, node := range g.Nodes {
		if i == 0 && len(node.DependsOn) != 0 {
			t.Fatalf("first node has upstream dependency: %v", node.DependsOn)
		}
		if i > 0 && (len(node.DependsOn) != 1 || node.DependsOn[0] != b.Plan.Entries[i-1].Seq) {
			t.Fatalf("node %d dependencies: %v", i, node.DependsOn)
		}
		if len(node.Tiles) == 0 {
			t.Fatalf("node %d has no covered tiles", i)
		}
	}
	oldPlan := clonePlan(b.Plan)
	newPlan := clonePlan(b.Plan)
	r := Rect{X: 40, Y: 30, W: 50, H: 40}
	newPlan.Entries[1].Region = &r
	manifest, err := ComputeInvalidation(oldPlan, newPlan, im, 16)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FirstChangedPosition != 1 {
		t.Fatalf("first change position %d", manifest.FirstChangedPosition)
	}
	seen := map[int]*Invalidation{}
	for i := range manifest.Affected {
		seen[manifest.Affected[i].Entry] = &manifest.Affected[i]
	}
	if _, ok := seen[1]; ok {
		t.Fatalf("upstream entry 1 was invalidated: %+v", manifest.Affected)
	}
	for _, seq := range []int{2, 3, 4, 5} {
		a, ok := seen[seq]
		if !ok {
			t.Fatalf("entry %d missing from invalidation", seq)
		}
		if len(a.Tiles) == 0 || len(a.Tiles) >= g.TileCount {
			t.Fatalf("entry %d affected %d/%d tiles; expected localized subset", seq, len(a.Tiles), g.TileCount)
		}
	}
	outside, err := TileByIndex(g.Tiles, g.TileCount-1)
	if err != nil {
		t.Fatal(err)
	}
	if oldPlan.Entries[1].Region.Intersects(outside.Rect()) || newPlan.Entries[1].Region.Intersects(outside.Rect()) {
		t.Fatal("test tile unexpectedly intersects both regions")
	}
	oldParams := EffectiveEntryFingerprint(oldPlan.Entries[1], outside)
	newParams := EffectiveEntryFingerprint(newPlan.Entries[1], outside)
	if oldParams != newParams {
		t.Fatal("outside-region effective params changed")
	}
	if CacheKey(2, outside.Index, oldParams, "in", "up") != CacheKey(2, outside.Index, newParams, "in", "up") {
		t.Fatal("outside-region cache key changed")
	}
	inside, err := TileByIndex(g.Tiles, TilesForRect(g.Tiles, r)[0])
	if err != nil {
		t.Fatal(err)
	}
	if EffectiveEntryFingerprint(oldPlan.Entries[1], inside) == EffectiveEntryFingerprint(newPlan.Entries[1], inside) {
		t.Fatal("inside-region effective params did not change")
	}
}

func TestReorderInvalidatesFromFirstChangedStage(t *testing.T) {
	im := testImage(t, 160, 120, Depth8, LayoutRGBA)
	oldPlan := &Plan{SchemaVersion: SchemaVersion, Entries: []Entry{
		{Seq: 1, TargetDepth: Depth16, TargetChannels: 4, Permutation: []int{2, 1, 0, 3},
			Fill: []FillSpec{}, Drop: []int{}, Quantization: QuantRound, Alpha: AlphaPreserve},
		{Seq: 2, TargetDepth: Depth8, TargetChannels: 4, Permutation: []int{0, 1, 2, 3},
			Fill: []FillSpec{}, Drop: []int{}, Quantization: QuantRound, Alpha: AlphaPreserve},
		{Seq: 3, TargetDepth: Depth16, TargetChannels: 4, Permutation: []int{0, 1, 2, 3},
			Fill: []FillSpec{}, Drop: []int{}, Quantization: QuantRound, Alpha: AlphaPreserve},
	}}
	okPlan := clonePlan(oldPlan)
	okPlan.Entries[1], okPlan.Entries[2] = okPlan.Entries[2], okPlan.Entries[1]
	okPlan.Entries[1].Seq, okPlan.Entries[2].Seq = 2, 3
	if err := okPlan.ValidateForImage(im); err != nil {
		t.Fatalf("reordered plan should be valid: %v", err)
	}
	manifest, err := ComputeInvalidation(oldPlan, okPlan, im, 24)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FirstChangedPosition != 1 {
		t.Fatalf("first change %d", manifest.FirstChangedPosition)
	}
	if len(manifest.Affected) == 0 || manifest.Affected[0].Entry != 2 {
		t.Fatalf("unexpected reorder manifest: %+v", manifest)
	}
}

func TestRegionEntryReorderUsesSuffixCoverage(t *testing.T) {
	im := testImage(t, 200, 160, Depth8, LayoutRGBA)
	r1 := Rect{X: 10, Y: 10, W: 25, H: 25}
	r2 := Rect{X: 120, Y: 90, W: 30, H: 20}
	base := func(seq int) Entry {
		return Entry{Seq: seq, TargetDepth: Depth16, TargetChannels: 4,
			Permutation: []int{0, 1, 2, 3}, Fill: []FillSpec{}, Drop: []int{},
			Quantization: QuantRound, Alpha: AlphaPreserve}
	}
	e1 := base(1)
	e2 := base(2)
	e2.Permutation = []int{2, 1, 0, 3}
	e2.Region = &r1
	e3 := base(3)
	e3.Alpha = AlphaOpaque
	e3.Region = &r2
	oldPlan := &Plan{SchemaVersion: SchemaVersion, Entries: []Entry{e1, e2, e3}}
	newPlan := &Plan{SchemaVersion: SchemaVersion, Entries: []Entry{e1, e3, e2}}
	newPlan.Entries[1].Seq = 2
	newPlan.Entries[2].Seq = 3
	if err := newPlan.ValidateForImage(im); err != nil {
		t.Fatal(err)
	}
	manifest, err := ComputeInvalidation(oldPlan, newPlan, im, 32)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FirstChangedPosition != 1 || len(manifest.Affected) != 2 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	for _, a := range manifest.Affected {
		if a.Entry == 1 {
			t.Fatalf("upstream entry 1 invalidated: %+v", a)
		}
		if len(a.Tiles) >= 7*5 {
			t.Fatalf("reorder invalidated the whole image: %+v", a)
		}
	}
}
