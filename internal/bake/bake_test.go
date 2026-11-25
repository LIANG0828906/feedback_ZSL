package bake_test

import (
	"math"
	"reflect"
	"testing"

	"terrainpipeline/internal/bake"
	"terrainpipeline/internal/model"
	"terrainpipeline/internal/samplegen"
)

func TestTiledBakeMatchesFullAndTruth(t *testing.T) {
	project := samplegen.Generate(model.FixedSeed)
	for _, block := range project.Blocks {
		full := bake.BakeBlock(project, block)
		tiled := bake.BakeBlockTiled(project, block, make([]model.PixelBake, 7))
		if full.NormalDigest != tiled.NormalDigest || full.SlopeDigest != tiled.SlopeDigest {
			t.Fatalf("block %s tiled bake mismatch", block.ID)
		}
		for i := range full.Pixels {
			if !reflect.DeepEqual(full.Pixels[i], tiled.Pixels[i]) {
				t.Fatalf("block %s pixel %d differs: full=%+v tiled=%+v", block.ID, i, full.Pixels[i], tiled.Pixels[i])
			}
		}
	}

	for _, truth := range project.Truth.Normals {
		block := project.BlockByID(truth.BlockID)
		result := bake.BakeBlock(project, block)
		pixel := result.Pixels[truth.J*block.NX+truth.I]
		angle := angleDeg(pixel.Normal, truth.Normal)
		slopeDelta := math.Abs(pixel.SlopeDeg - truth.SlopeDeg)
		if angle > truth.Tolerance || slopeDelta > truth.Tolerance {
			t.Fatalf("truth %s (%d,%d): angle=%g slopeDelta=%g tolerance=%g", truth.BlockID, truth.I, truth.J, angle, slopeDelta, truth.Tolerance)
		}
	}
}

func angleDeg(a, b model.Vec3) float64 {
	return bake.AngleDeg(a, b)
}
