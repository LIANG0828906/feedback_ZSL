package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"texturekernel"
	"texturekernel/sample"
	"texturekernel/store"
)

type Report struct {
	RunID            string        `json:"run_id"`
	PlanFingerprint  string        `json:"plan_fingerprint"`
	TruthFingerprint string        `json:"truth_fingerprint"`
	FinalHash        string        `json:"final_hash"`
	Chunks           int           `json:"chunks"`
	StageDigests     []StageDigest `json:"stage_digests"`
	SamplesChecked   int           `json:"samples_checked"`
}

type StageDigest struct {
	EntryIndex int    `json:"entry_index"`
	Digest     string `json:"digest"`
}

// VerifyCheckpoint uses only persisted chunk outputs, the plan document, and
// the sample truth. It intentionally does not call the conversion pipeline.
func VerifyCheckpoint(ctx context.Context, st *store.Store, runID string, plan *texture.Plan, truth *sample.Truth) (*Report, error) {
	if st == nil || plan == nil || truth == nil {
		return nil, verifyError(texture.CodePara, "$", "input", "store, plan, and truth are required")
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if truth.Fingerprint == "" || truth.RecomputeFingerprint() != truth.Fingerprint {
		return nil, verifyError(texture.CodeSchema, "$.truth.fingerprint", "fingerprint", "truth fingerprint mismatch")
	}
	if truth.PlanFingerprint != plan.Fingerprint {
		return nil, verifyError(texture.CodeDiff, "$.truth.plan_fingerprint", "plan_fingerprint", "truth belongs to a different plan")
	}
	expected, err := texture.ImageFromDocument(truth.ExpectedFinal)
	if err != nil {
		return nil, err
	}
	if expected.Hash() != truth.ExpectedFinalHash {
		return nil, verifyError(texture.CodeSchema, "$.truth.expected_final", "hash", "truth final image hash mismatch")
	}
	if truth.ChunkWidth <= 0 || truth.ChunkHeight <= 0 {
		return nil, verifyError(texture.CodeSchema, "$.truth.chunk_width", "chunk_width", "truth has no valid chunk geometry")
	}
	meta := store.RunMeta{
		RunID: runID, SourceHash: truth.SourceHash, PlanFingerprint: truth.PlanFingerprint,
		ParamVersion: truth.ParamVersion, AlgorithmVersion: truth.AlgorithmVersion,
		Width: plan.Source.Width, Height: plan.Source.Height, BitDepth: plan.Source.BitDepth,
		Layout:     texture.LayoutString(plan.Source.Layout),
		ChunkWidth: truth.ChunkWidth, ChunkHeight: truth.ChunkHeight,
	}
	storedMeta, found, err := st.ReadRunMeta(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, verifyError(texture.CodeStore, "$.checkpoint.run_id", "run_id", "run does not exist")
	}
	if !metaEqual(storedMeta, meta) {
		return nil, diffVerify(0, 0, -1, "checkpoint.metadata", "persisted run metadata differs from plan/truth", "", "")
	}
	chunks, err := texture.BuildChunks(meta.Width, meta.Height, meta.ChunkWidth, meta.ChunkHeight)
	if err != nil {
		return nil, err
	}
	loaded, err := st.LoadChunksUnchecked(ctx, meta)
	if err != nil {
		return nil, err
	}
	if len(loaded) != len(chunks) {
		return nil, diffVerify(0, 0, -1, "checkpoint.chunks", fmt.Sprintf("expected %d chunks, got %d", len(chunks), len(loaded)), "", "")
	}
	last := len(plan.Entries) - 1
	final, err := texture.NewImage(expected.Width, expected.Height, expected.BitDepth, expected.Layout)
	if err != nil {
		return nil, err
	}
	stageDigests := make([]StageDigest, len(plan.Entries))
	builders := make([]interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}, len(plan.Entries))
	for i := range builders {
		builders[i] = sha256.New()
	}
	channels := len(final.Layout)
	var firstHashDiff *texture.DiffInfo
	for i, commit := range loaded {
		if commit.CommitSeq != int64(i) || commit.ChunkX != chunks[i].X || commit.ChunkY != chunks[i].Y {
			return nil, diffVerify(chunks[i].Rect.X, chunks[i].Rect.Y, -1, "checkpoint.commit_sequence", fmt.Sprintf("chunk %d is not in stable row-major order", i), "", "")
		}
		if len(commit.Stages) != len(plan.Entries) {
			return nil, diffVerify(chunks[i].Rect.X, chunks[i].Rect.Y, -1, "checkpoint.stages", fmt.Sprintf("chunk %d has %d stages, want %d", i, len(commit.Stages), len(plan.Entries)), "", "")
		}
		for e, stage := range commit.Stages {
			if stage.EntryIndex != e || stage.PlanFingerprint != plan.Fingerprint ||
				stage.ParamVersion != truth.ParamVersion || stage.AlgorithmVersion != truth.AlgorithmVersion {
				return nil, diffVerify(chunks[i].Rect.X, chunks[i].Rect.Y, e, "checkpoint.stage_versions",
					fmt.Sprintf("processed stage metadata mismatch at entry %d chunk (%d,%d)", e, commit.ChunkX, commit.ChunkY), "", "")
			}
			snapshot := texture.EntryParameterSnapshot(plan.Entries[e])
			if stage.ParameterSnapshot != snapshot {
				return nil, diffVerify(chunks[i].Rect.X, chunks[i].Rect.Y, e, "checkpoint.parameter_snapshot",
					"frozen parameter snapshot differs from plan", stage.ParameterSnapshot, snapshot)
			}
			if stage.Image == nil {
				return nil, diffVerify(chunks[i].Rect.X, chunks[i].Rect.Y, e, "checkpoint.output", "stage output is missing", "", "")
			}
			if stage.Image.Width != chunks[i].Rect.Width || stage.Image.Height != chunks[i].Rect.Height {
				return nil, diffVerify(chunks[i].Rect.X, chunks[i].Rect.Y, e, "checkpoint.dimensions",
					fmt.Sprintf("tile dimensions mismatch at entry %d chunk (%d,%d)", e, commit.ChunkX, commit.ChunkY), "", "")
			}
			actualHash := stage.Image.Hash()
			if stage.OutputHash != actualHash && firstHashDiff == nil {
				firstHashDiff = &texture.DiffInfo{
					X: chunks[i].Rect.X, Y: chunks[i].Rect.Y, EntryIndex: e,
					Field: "checkpoint.output_hash", Reason: "stored output hash does not match persisted pixels",
					Values: map[string]string{"stored": stage.OutputHash, "actual": actualHash},
				}
			}
			fmt.Fprintf(builders[e], "%d:%d:%d:%d:%s:%s:%d:%d:%s\n",
				commit.CommitSeq, commit.ChunkX, commit.ChunkY, e, stage.CacheKey,
				stage.OutputHash, stage.Image.Width, stage.Image.Height, texture.LayoutString(stage.Image.Layout))
		}
		stage := commit.Stages[last]
		if stage.Image.BitDepth != final.BitDepth ||
			texture.LayoutString(stage.Image.Layout) != texture.LayoutString(final.Layout) {
			return nil, diffVerify(chunks[i].Rect.X, chunks[i].Rect.Y, last, "checkpoint.final_metadata", "final stage tile metadata mismatch", "", "")
		}
		for y := 0; y < stage.Image.Height; y++ {
			dst := ((chunks[i].Rect.Y+y)*final.Width + chunks[i].Rect.X) * channels
			copy(final.Pix[dst:dst+stage.Image.Width*channels], stage.Image.Pix[y*stage.Image.Width*channels:(y+1)*stage.Image.Width*channels])
		}
	}
	for i, h := range builders {
		stageDigests[i] = StageDigest{EntryIndex: i, Digest: hex.EncodeToString(h.Sum(nil))}
	}
	equal, diff := final.EqualWithDiff(expected)
	if !equal {
		if diff == nil {
			diff = &texture.PixelDiff{Field: "final_image"}
		}
		valueMap := map[string]string{}
		valueMap["checkpoint"] = fmt.Sprintf("%d", diff.Left)
		valueMap["truth"] = fmt.Sprintf("%d", diff.Right)
		return nil, &texture.KernelError{
			Code: texture.CodeDiff, Path: "$.final", Message: "persisted final image differs from analytical truth",
			Diff: &texture.DiffInfo{
				X: diff.X, Y: diff.Y, EntryIndex: last, Field: diff.Field,
				Reason: "first pixel mismatch against truth", Values: valueMap,
			},
		}
	}
	samples := append([]sample.TruthSample(nil), truth.Samples...)
	sort.SliceStable(samples, func(i, j int) bool {
		if samples[i].Y != samples[j].Y {
			return samples[i].Y < samples[j].Y
		}
		return samples[i].X < samples[j].X
	})
	for _, truthSample := range samples {
		for _, expectedValue := range truthSample.Final {
			actual, ok := final.ChannelAt(truthSample.X, truthSample.Y, expectedValue.Channel)
			if !ok {
				return nil, verifyError(texture.CodeDiff, "$.truth.samples", "channel", "truth sample %q references missing channel %s", truthSample.Name, expectedValue.Channel)
			}
			if actual != expectedValue.Value {
				return nil, &texture.KernelError{
					Code: texture.CodeDiff, Path: "$.truth.samples", Message: "truth sample mismatch",
					Diff: &texture.DiffInfo{
						X: truthSample.X, Y: truthSample.Y, EntryIndex: last,
						Field:  fmt.Sprintf("image.%s", expectedValue.Channel),
						Reason: fmt.Sprintf("sample %q", truthSample.Name),
						Values: map[string]string{"checkpoint": fmt.Sprintf("%d", actual), "truth": fmt.Sprintf("%d", expectedValue.Value)},
					},
				}
			}
		}
	}
	if firstHashDiff != nil {
		return nil, &texture.KernelError{
			Code: texture.CodeDiff, Path: "$.checkpoint", Message: "persisted output hash mismatch",
			Diff: firstHashDiff,
		}
	}
	return &Report{
		RunID: runID, PlanFingerprint: plan.Fingerprint,
		TruthFingerprint: truth.Fingerprint, FinalHash: final.Hash(),
		Chunks: len(chunks), StageDigests: stageDigests, SamplesChecked: len(samples),
	}, nil
}

func metaEqual(a, b store.RunMeta) bool {
	return a.RunID == b.RunID && a.SourceHash == b.SourceHash &&
		a.PlanFingerprint == b.PlanFingerprint && a.ParamVersion == b.ParamVersion &&
		a.AlgorithmVersion == b.AlgorithmVersion && a.Width == b.Width &&
		a.Height == b.Height && a.BitDepth == b.BitDepth && a.Layout == b.Layout &&
		a.ChunkWidth == b.ChunkWidth && a.ChunkHeight == b.ChunkHeight
}

func verifyError(code texture.Code, path, field, format string, args ...any) *texture.KernelError {
	return &texture.KernelError{Code: code, Path: path, Field: field, Message: fmt.Sprintf(format, args...)}
}

func diffVerify(x, y, entry int, field, reason, actual, expected string) *texture.KernelError {
	values := map[string]string{}
	if actual != "" || expected != "" {
		values["checkpoint"] = actual
		values["truth"] = expected
	}
	return &texture.KernelError{
		Code: texture.CodeDiff, Path: "$", Message: reason,
		Diff: &texture.DiffInfo{X: x, Y: y, EntryIndex: entry, Field: field, Reason: reason, Values: values},
	}
}
