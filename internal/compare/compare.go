package compare

import (
	"context"
	"fmt"
	"sort"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/core"
)

type Report struct {
	RunA        string   `json:"run_a"`
	RunB        string   `json:"run_b"`
	Images      []string `json:"images"`
	Steps       int      `json:"steps"`
	Blocks      int      `json:"blocks"`
	Identical   bool     `json:"identical"`
	FirstDiff   *Diff    `json:"first_diff,omitempty"`
	Differences int      `json:"differences"`
}

type Diff struct {
	Image  string         `json:"image"`
	Step   int            `json:"step"`
	Block  *core.BlockRef `json:"block,omitempty"`
	Field  string         `json:"field"`
	Detail string         `json:"detail"`
}

func Runs(ctx context.Context, store checkpoint.Store, runA, runB string, imageIDs []string) (*Report, error) {
	report := &Report{RunA: runA, RunB: runB}
	if len(imageIDs) == 0 {
		idsA, err := store.ListImages(ctx, runA)
		if err != nil {
			return report, err
		}
		idsB, err := store.ListImages(ctx, runB)
		if err != nil {
			return report, err
		}
		seen := make(map[string]bool)
		for _, id := range append(idsA, idsB...) {
			if !seen[id] {
				seen[id] = true
				imageIDs = append(imageIDs, id)
			}
		}
		sort.Strings(imageIDs)
	}
	report.Images = append([]string(nil), imageIDs...)
	for _, imageID := range imageIDs {
		metaA, okA, err := store.GetRunMeta(ctx, runA, imageID)
		if err != nil {
			return report, err
		}
		metaB, okB, err := store.GetRunMeta(ctx, runB, imageID)
		if err != nil {
			return report, err
		}
		if !okA || !okB {
			d := &Diff{Image: imageID, Field: "run.presence", Detail: fmt.Sprintf("a=%v b=%v", okA, okB)}
			return finishDiff(report, d)
		}
		if metaA.AlgorithmVersion != metaB.AlgorithmVersion {
			d := &Diff{Image: imageID, Step: 0, Field: "algorithm_version",
				Detail: fmt.Sprintf("%s != %s", metaA.AlgorithmVersion, metaB.AlgorithmVersion)}
			return finishDiff(report, d)
		}
		if metaA.ChainFingerprint != metaB.ChainFingerprint {
			d := &Diff{Image: imageID, Step: 0, Field: "chain_fingerprint",
				Detail: fmt.Sprintf("%s != %s", metaA.ChainFingerprint, metaB.ChainFingerprint)}
			return finishDiff(report, d)
		}
		report.Steps = max(report.Steps, metaA.StepCount)
		aRows, err := store.LoadArtifacts(ctx, runA, imageID)
		if err != nil {
			return report, err
		}
		bRows, err := store.LoadArtifacts(ctx, runB, imageID)
		if err != nil {
			return report, err
		}
		aMap := indexArtifacts(aRows)
		bMap := indexArtifacts(bRows)
		for step := 0; step <= metaA.StepCount; step++ {
			cols := metaA.Width / metaA.BlockSize
			rows := metaA.Height / metaA.BlockSize
			for by := 0; by < rows; by++ {
				for bx := 0; bx < cols; bx++ {
					key := fmt.Sprintf("%d|%d|%d", step, bx, by)
					a, okA := aMap[key]
					b, okB := bMap[key]
					if okA {
						report.Blocks++
					}
					if !okA || !okB {
						d := &Diff{Image: imageID, Step: step,
							Block: &core.BlockRef{X: bx * metaA.BlockSize, Y: by * metaA.BlockSize,
								W: metaA.BlockSize, H: metaA.BlockSize, Index: by*cols + bx},
							Field: "block.presence", Detail: fmt.Sprintf("a=%v b=%v", okA, okB)}
						return finishDiff(report, d)
					}
					if err := validateArtifact(a); err != nil {
						d := &Diff{Image: imageID, Step: step, Block: blockRef(a.Block, metaA.Width, metaA.BlockSize),
							Field: "artifact", Detail: "run A: " + err.Error()}
						return finishDiff(report, d)
					}
					if err := validateArtifact(b); err != nil {
						d := &Diff{Image: imageID, Step: step, Block: blockRef(b.Block, metaA.Width, metaA.BlockSize),
							Field: "artifact", Detail: "run B: " + err.Error()}
						return finishDiff(report, d)
					}
					if a.OutputHash != b.OutputHash || !bytesEqual(a.Pixels, b.Pixels) {
						d := &Diff{Image: imageID, Step: step, Block: blockRef(a.Block, metaA.Width, metaA.BlockSize),
							Field: "pixels", Detail: firstPixelDelta(a.Pixels, b.Pixels)}
						return finishDiff(report, d)
					}
				}
			}
		}
	}
	report.Identical = true
	return report, nil
}

func finishDiff(report *Report, d *Diff) (*Report, error) {
	report.Differences++
	report.FirstDiff = d
	e := core.DiffError(d.Step, d.Block, d.Field, d.Detail)
	e.Details = map[string]any{"image": d.Image}
	return report, e
}

func indexArtifacts(rows []checkpoint.Artifact) map[string]checkpoint.Artifact {
	out := make(map[string]checkpoint.Artifact, len(rows))
	for _, a := range rows {
		out[fmt.Sprintf("%d|%d|%d", a.Step, a.Block.X/a.Block.W, a.Block.Y/a.Block.H)] = a
	}
	return out
}

func validateArtifact(a checkpoint.Artifact) error {
	if a.State != core.StateCommitted {
		return fmt.Errorf("not committed")
	}
	if a.OutputHash != checkpoint.PixelHash(a.Pixels) {
		return fmt.Errorf("content hash mismatch")
	}
	return a.Validate()
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstPixelDelta(a, b []byte) string {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("first byte offset %d: %d != %d", i, a[i], b[i])
		}
	}
	if len(a) != len(b) {
		return fmt.Sprintf("pixel byte length %d != %d", len(a), len(b))
	}
	return "pixels differ"
}

func blockRef(r core.Rect, width, blockSize int) *core.BlockRef {
	cols := width / blockSize
	return &core.BlockRef{
		Index: (r.Y/blockSize)*cols + r.X/blockSize,
		X:     r.X, Y: r.Y, W: r.W, H: r.H,
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
