package engine

import (
	"fmt"

	"fixedsampler/internal/hashutil"
	"fixedsampler/internal/model"
)

func CompareCommitSequences(a, b []int) *model.DiffLocation {
	if len(a) != len(b) {
		return &model.DiffLocation{StageSeq: 0, Field: "commit_sequence.length", Expected: fmt.Sprint(len(a)), Actual: fmt.Sprint(len(b))}
	}
	for i := range a {
		if a[i] != b[i] {
			return &model.DiffLocation{
				StageSeq: 0, Field: fmt.Sprintf("commit_sequence[%d]", i),
				Expected: fmt.Sprint(a[i]), Actual: fmt.Sprint(b[i]),
			}
		}
	}
	return nil
}

func CompareChunkMaps(a, b map[int]*model.ChunkArtifact, seqA, seqB []int) *model.DiffLocation {
	if loc := CompareCommitSequences(seqA, seqB); loc != nil {
		return loc
	}
	if len(a) != len(b) {
		return &model.DiffLocation{Field: "chunks.length", Expected: fmt.Sprint(len(a)), Actual: fmt.Sprint(len(b))}
	}
	for idx := 0; idx < len(a); idx++ {
		ca, cb := a[idx], b[idx]
		if ca == nil || cb == nil {
			return &model.DiffLocation{ChunkIndex: idx, Field: "chunk.missing", Expected: "non-nil", Actual: "nil"}
		}
		if loc := CompareChunkArtifacts(ca, cb); loc != nil {
			return loc
		}
	}
	return nil
}

func CompareChunkArtifacts(a, b *model.ChunkArtifact) *model.DiffLocation {
	base := model.DiffLocation{ChunkIndex: a.ChunkIndex, ChunkX: a.ChunkX, ChunkY: a.ChunkY}
	if a.ChunkIndex != b.ChunkIndex || a.ChunkX != b.ChunkX || a.ChunkY != b.ChunkY {
		loc := base
		loc.Field = "chunk.coordinates"
		loc.Expected = fmt.Sprintf("%d,(%d,%d)", a.ChunkIndex, a.ChunkX, a.ChunkY)
		loc.Actual = fmt.Sprintf("%d,(%d,%d)", b.ChunkIndex, b.ChunkX, b.ChunkY)
		return &loc
	}
	if len(a.Levels) != len(b.Levels) {
		loc := base
		loc.Field = "levels.length"
		loc.Expected = fmt.Sprint(len(a.Levels))
		loc.Actual = fmt.Sprint(len(b.Levels))
		return &loc
	}
	for i := range a.Levels {
		la, lb := a.Levels[i], b.Levels[i]
		base.StageSeq = la.Seq
		if la.Seq != lb.Seq {
			return diff(base, fmt.Sprintf("levels[%d].seq", i), la.Seq, lb.Seq)
		}
		if la.Type != lb.Type {
			return diff(base, fmt.Sprintf("levels[%d].type", i), la.Type, lb.Type)
		}
		if hashutil.CanonicalHash(la.Config) != hashutil.CanonicalHash(lb.Config) {
			return diff(base, fmt.Sprintf("levels[%d].config", i), hashutil.CanonicalHash(la.Config), hashutil.CanonicalHash(lb.Config))
		}
		ta := tileMap(la.Tiles)
		tb := tileMap(lb.Tiles)
		if len(ta) != len(tb) {
			return diff(base, fmt.Sprintf("levels[%d].tiles.length", i), len(ta), len(tb))
		}
		for name, tileA := range ta {
			tileB, ok := tb[name]
			if !ok {
				return diff(base, fmt.Sprintf("levels[%d].tiles.%s.missing", i, name), "present", "missing")
			}
			fieldPrefix := fmt.Sprintf("levels[%d].tiles.%s", i, name)
			if tileA.Rect != tileB.Rect {
				return diff(base, fieldPrefix+".rect", tileA.Rect, tileB.Rect)
			}
			if len(tileA.Values) != len(tileB.Values) {
				return diff(base, fieldPrefix+".values.length", len(tileA.Values), len(tileB.Values))
			}
			for j := range tileA.Values {
				if tileA.Values[j] != tileB.Values[j] {
					return diff(base, fmt.Sprintf("%s.values[%d]", fieldPrefix, j), tileA.Values[j], tileB.Values[j])
				}
			}
		}
	}
	return nil
}

func tileMap(tiles []model.Tile) map[string]model.Tile {
	out := map[string]model.Tile{}
	for _, tile := range tiles {
		out[tile.Plane] = tile
	}
	return out
}

func diff(base model.DiffLocation, field string, expected, actual any) *model.DiffLocation {
	base.Field = field
	base.Expected = fmt.Sprint(expected)
	base.Actual = fmt.Sprint(actual)
	return &base
}
