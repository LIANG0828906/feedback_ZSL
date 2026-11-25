package maskqa

import "testing"

func TestDiffLocatorsReportFirstTile(t *testing.T) {
	a := []LogicalCommit{{Seq: 1, StrokeIndex: 1, TileID: 0, CacheKey: "a"}, {Seq: 2, StrokeIndex: 1, TileID: 1, CacheKey: "b"}}
	b := []LogicalCommit{{Seq: 1, StrokeIndex: 1, TileID: 0, CacheKey: "a"}, {Seq: 2, StrokeIndex: 1, TileID: 1, CacheKey: "changed"}}
	tile, idx, msg := FirstSequenceDiff(a, b)
	if tile != 1 || idx != 1 || msg == "" {
		t.Fatalf("sequence diff tile=%d idx=%d msg=%q", tile, idx, msg)
	}

	tiles := BuildTiles(80, 48, 32)
	m1 := NewMask(80, 48)
	m2 := NewMask(80, 48)
	m2.Set(40, 10, true) // tile 1 (x 32..63, y 0..31)
	tileID, p, msg := FirstMaskDifference(m1, m2, tiles)
	if tileID != 1 || p.X != 40 || msg == "" {
		t.Fatalf("mask diff tile=%d point=%v msg=%q", tileID, p, msg)
	}
}
