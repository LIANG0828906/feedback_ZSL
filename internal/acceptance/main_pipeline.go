package acceptance

import (
	"context"
	"os"
	"path/filepath"

	"channelpipe"
	"channelpipe/internal/checkpoint"
	"channelpipe/internal/runner"
	"channelpipe/internal/sample"
)

func runMainPipeline(ctx context.Context, store *checkpoint.Store, set *sample.SampleSet, workers, tileWidth, tileHeight int) (*FailureReport, error) {
	imageByID := map[string]*sample.SourceImage{}
	for _, image := range set.Images {
		imageByID[image.ID] = image
	}
	targetPlan := set.Set.Plans[0]
	targetChunks := channelpipe.BuildChunks(targetPlan.Width, targetPlan.Height, tileWidth, tileHeight)
	targetChunk := 0
	if len(targetChunks) > 1 {
		targetChunk = 1
	}

	cfg := runner.Config{
		Workers:               workers,
		TileWidth:             tileWidth,
		TileHeight:            tileHeight,
		PerThreadBufferPixels: 1 << 22,
		FailureHook: func(imageID string, chunk channelpipe.Chunk, stageIndex, attempt int) error {
			if imageID == targetPlan.ID && chunk.Index == targetChunk && attempt == 1 {
				return channelpipe.FieldError(channelpipe.CodeInvalid, "injected.failure", "deterministic single-chunk failure before stage %d", stageIndex)
			}
			return nil
		},
	}
	pipeline := runner.New(store, cfg)

	var firstTarget *runner.Outcome
	for i, plan := range set.Set.Plans {
		image := imageByID[plan.ID]
		outcome, err := pipeline.RunPlan(ctx, plan.ID, image.Buffer, plan)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			firstTarget = outcome
		}
	}

	outputDir := filepath.Join(filepath.Dir(storePathFromStore(store)), "final")
	_ = os.MkdirAll(outputDir, 0o755)

	var retryTarget *runner.Outcome
	for i, plan := range set.Set.Plans {
		image := imageByID[plan.ID]
		outcome, err := pipeline.RunPlan(ctx, plan.ID, image.Buffer, plan)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			retryTarget = outcome
		}
		if outcome.Result != nil && outcome.Result.Final != nil {
			blob, err := channelpipe.EncodeBuffer(outcome.Result.Final)
			if err == nil {
				_ = os.WriteFile(filepath.Join(outputDir, plan.ID+".final.bin"), blob, 0o644)
				_ = channelpipe.WritePNGPreview(filepath.Join(outputDir, plan.ID+".final.png"), outcome.Result.Final)
			}
			_ = writeJSON(filepath.Join(outputDir, plan.ID+".summary.json"), outcome.Result.Summaries)
		}
	}

	sequence, err := store.CommitSequence(targetPlan.ID)
	if err != nil {
		return nil, err
	}
	report := &FailureReport{
		InjectedChunk:       targetChunk,
		FirstRunFailed:      firstTarget.Failed,
		FirstRunCommitted:   firstTarget.CommittedNow,
		RetryCommitted:      retryTarget.CommittedNow,
		FinalCommitSequence: sequence,
	}
	report.Isolated = len(firstTarget.Failed) == 1 &&
		firstTarget.Failed[0].ChunkIndex == targetChunk &&
		!containsInt(firstTarget.CommittedNow, targetChunk)
	report.Recovered = len(retryTarget.Failed) == 0 && containsInt(retryTarget.CommittedNow, targetChunk)
	return report, nil
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func storePathFromStore(store *checkpoint.Store) string {
	return store.Path()
}
