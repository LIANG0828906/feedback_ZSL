package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

func BlockContentHash(project *Project, block *Block) string {
	h := sha256.New()
	fmt.Fprintf(h, "schema=%s\nalgo=%s\nparam=%s\n", project.SchemaVersion, AlgorithmVersion, project.ParamVersion)
	fmt.Fprintf(h, "block=%s nx=%d nz=%d dx=%.12g dz=%.12g ox=%.12g oz=%.12g\n", block.ID, block.NX, block.NZ, block.DX, block.DZ, block.OriginX, block.OriginZ)
	for _, v := range block.Elevation {
		fmt.Fprintf(h, "h=%.9f\n", v)
	}
	for p, weights := range block.Weights {
		fmt.Fprintf(h, "p=%d", p)
		for _, w := range weights {
			fmt.Fprintf(h, ",%d", w)
		}
		fmt.Fprintln(h)
	}
	for layerIndex, layer := range project.Layers {
		used := false
		for _, weights := range block.Weights {
			if layerIndex < len(weights) && weights[layerIndex] > 0 {
				used = true
				break
			}
		}
		if used {
			fmt.Fprintf(h, "layer=%s strength=%.12g freq=%.12g\n", layer.ID, layer.Strength, layer.Frequency)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func BlockCacheHash(project *Project, block *Block) string {
	h := sha256.New()
	fmt.Fprintf(h, "cache-schema=1\nsource=%s\n", BlockContentHash(project, block))
	for _, seam := range project.Seams {
		var other *Block
		var edge Edge
		switch block.ID {
		case seam.BlockA:
			other = project.BlockByID(seam.BlockB)
			edge = seam.EdgeA
		case seam.BlockB:
			other = project.BlockByID(seam.BlockA)
			edge = seam.EdgeB
		}
		if other == nil {
			continue
		}
		fmt.Fprintf(h, "seam=%s edge=%s other=%s other_source=%s threshold=%.12g\n",
			seam.ID, edge, other.ID, BlockContentHash(project, other), seam.ThresholdDeg)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ProjectContentHash(project *Project) string {
	h := sha256.New()
	fmt.Fprintf(h, "schema=%s seed=%d param=%s algo=%s\n", project.SchemaVersion, project.Seed, project.ParamVersion, AlgorithmVersion)
	for _, id := range project.SortedBlockIDs() {
		block := project.BlockByID(id)
		fmt.Fprintf(h, "block=%s content=%s\n", id, BlockContentHash(project, block))
	}
	for _, seam := range project.Seams {
		fmt.Fprintf(h, "seam=%s %s:%s->%s:%s n=%d th=%.12g\n", seam.ID, seam.BlockA, seam.EdgeA, seam.BlockB, seam.EdgeB, seam.SampleCount, seam.ThresholdDeg)
	}
	return hex.EncodeToString(h.Sum(nil))
}
