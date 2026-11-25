package texture_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"texturekernel"
	"texturekernel/pipeline"
	"texturekernel/sample"
	"texturekernel/store"
	"texturekernel/verify"
)

func testOptions(t *testing.T) sample.Options {
	t.Helper()
	opts := sample.DefaultOptions(42)
	opts.Width = 128
	opts.Height = 96
	opts.BitDepth = 8
	opts.Layout = []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA}
	opts.ChunkWidth = 32
	opts.ChunkHeight = 32
	opts.Workers = 3
	return opts
}

func testRunner(t *testing.T, s *store.Store, workers int) *pipeline.Runner {
	t.Helper()
	runner, err := pipeline.NewRunner(s, pipeline.Config{
		ChunkWidth: 32, ChunkHeight: 32, Workers: workers,
		PixelBufferPerWorker: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestPlanVersionsRoundTrip(t *testing.T) {
	bundle, err := sample.Generate(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1", "2", "3"} {
		loaded, err := texture.LoadPlan(bundle.PlanFiles["plan-v"+version+".json"])
		if err != nil {
			t.Fatalf("load v%s: %v", version, err)
		}
		if loaded.Version != version {
			t.Fatalf("version = %s, want %s", loaded.Version, version)
		}
		if len(loaded.Traces) == 0 {
			t.Fatalf("v%s did not preserve field traces", version)
		}
		if err := texture.EquivalentForExport(bundle.Plan, loaded); err != nil {
			t.Fatalf("v%s round trip: %v", version, err)
		}
		for _, targetVersion := range []string{"1", "2", "3"} {
			exported, losses, err := texture.ExportPlan(loaded, targetVersion)
			if err != nil || len(losses) != 0 {
				t.Fatalf("export v%s -> v%s: err=%v losses=%v", version, targetVersion, err, losses)
			}
			reloaded, err := texture.LoadPlan(exported)
			if err != nil {
				t.Fatalf("reload v%s -> v%s: %v", version, targetVersion, err)
			}
			if err := texture.EquivalentForExport(loaded, reloaded); err != nil {
				t.Fatalf("v%s -> v%s round trip: %v", version, targetVersion, err)
			}
		}
	}
}

func TestSampleGeneratorDeterministicAnd16BitOracle(t *testing.T) {
	opts := testOptions(t)
	opts.Width = 64
	opts.Height = 48
	opts.BitDepth = 16
	opts.Layout = []texture.Channel{texture.ChB, texture.ChG, texture.ChR, texture.ChA}
	opts.ChunkWidth = 16
	opts.ChunkHeight = 16
	first, err := sample.Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sample.Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source.Hash() != second.Source.Hash() || first.Truth.Fingerprint != second.Truth.Fingerprint {
		t.Fatal("sample generation is not deterministic")
	}
	final, _, err := texture.ConvertPlan(first.Source, first.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if final.Hash() != first.Truth.ExpectedFinalHash {
		t.Fatalf("16-bit BGRA final hash %s != truth %s", final.Hash(), first.Truth.ExpectedFinalHash)
	}
	for _, truthSample := range first.Truth.Samples {
		for _, expected := range truthSample.Final {
			actual, ok := final.ChannelAt(truthSample.X, truthSample.Y, expected.Channel)
			if !ok || actual != expected.Value {
				t.Fatalf("sample %s channel %s = %d, want %d", truthSample.Name, expected.Channel, actual, expected.Value)
			}
		}
	}
}

func TestPlanErrorsAndLossReport(t *testing.T) {
	_, err := texture.LoadPlan([]byte(`{"format":"texture-plan/9"}`))
	if !texture.HasCode(err, texture.CodeVersion) {
		t.Fatalf("want E_VERSION, got %v", err)
	}
	_, err = texture.LoadPlan([]byte(`{"version":1,"source":{"width":2,"height":2,"bit_depth":8,"layout":"RGBA"},"entries":[{"index":0,"coverage":{"x":0,"y":0,"width":2,"height":2},"target_bit_depth":8,"channel_order":["R","G","B","A"],"rounding":"nearest"}]}`))
	if !texture.HasCode(err, texture.CodeSchema) {
		t.Fatalf("want E_SCHEMA, got %v", err)
	}
	plan := &texture.Plan{
		Version: "3",
		Source:  texture.Source{Width: 2, Height: 2, BitDepth: 8, Layout: []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA}},
		Entries: []texture.Entry{{
			Index: 0, Coverage: texture.FullRect(2, 2), TargetBitDepth: 8,
			TargetChannels: []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA},
			Mappings: []texture.ChannelMapping{
				{Target: texture.ChR, Source: texture.ChR}, {Target: texture.ChG, Source: texture.ChG},
				{Target: texture.ChB, Source: texture.ChB}, {Target: texture.ChA, Source: texture.ChA},
			},
			Rounding: texture.RoundNearest, Alpha: texture.AlphaPremultiply,
		}},
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	data, losses, err := texture.ExportPlan(plan, "1")
	if !texture.HasCode(err, texture.CodeLossy) || len(losses) == 0 {
		t.Fatalf("want E_LOSSY, got err=%v losses=%v", err, losses)
	}
	if _, loadErr := texture.LoadPlan(data); loadErr != nil {
		t.Fatalf("lossy artifact must remain loadable: %v", loadErr)
	}
}

func TestQuantizeAndConversionMappings(t *testing.T) {
	if v, err := texture.Quantize(0, 16, 8, texture.RoundNearest); err != nil || v != 0 {
		t.Fatalf("zero endpoint: %d %v", v, err)
	}
	if v, err := texture.Quantize(65535, 16, 8, texture.RoundCeil); err != nil || v != 255 {
		t.Fatalf("max endpoint: %d %v", v, err)
	}
	if v, err := texture.Quantize(1, 8, 8, texture.RoundFloor); err != nil || v != 1 {
		t.Fatalf("identity: %d %v", v, err)
	}
	if _, err := texture.Quantize(256, 8, 16, texture.RoundNearest); !texture.HasCode(err, texture.CodePara) {
		t.Fatalf("want E_PARA, got %v", err)
	}

	source, _ := texture.NewImage(2, 1, 8, []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA})
	for i, values := range [][]uint16{{10, 20, 30, 40}, {50, 60, 70, 80}} {
		copy(source.Pix[i*4:], values)
	}
	plan := &texture.Plan{
		Version: "test",
		Source:  texture.Source{Width: 2, Height: 1, BitDepth: 8, Layout: []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA}},
		Entries: []texture.Entry{{
			Index: 0, Coverage: texture.FullRect(2, 1), TargetBitDepth: 16,
			TargetChannels: []texture.Channel{texture.ChB, texture.ChG, texture.ChR},
			Mappings: []texture.ChannelMapping{
				{Target: texture.ChB, Source: texture.ChB},
				{Target: texture.ChG, Fill: &texture.FillSource{Kind: texture.FillConstant, Value: 65535}},
				{Target: texture.ChR, Source: texture.ChR},
			},
			Rounding: texture.RoundNearest, Alpha: texture.AlphaDrop,
		}},
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	result, err := texture.ApplyEntry(source, plan, plan.Entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Image.Pix; got[0] != 30*257 || got[1] != 65535 || got[2] != 10*257 {
		t.Fatalf("converted pixel = %v", got[:3])
	}
	if len(result.DroppedChannels) != 2 || result.DroppedChannels[0] != texture.ChG || result.DroppedChannels[1] != texture.ChA {
		t.Fatalf("dropped channels = %v", result.DroppedChannels)
	}
}

func TestPremultiplyAndUnpremultiply(t *testing.T) {
	source, _ := texture.NewImage(1, 1, 8, []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA})
	copy(source.Pix, []uint16{200, 100, 50, 128})
	entry := texture.Entry{
		Index: 0, Coverage: texture.FullRect(1, 1), TargetBitDepth: 8,
		TargetChannels: []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA},
		Mappings: []texture.ChannelMapping{
			{Target: texture.ChR, Source: texture.ChR}, {Target: texture.ChG, Source: texture.ChG},
			{Target: texture.ChB, Source: texture.ChB}, {Target: texture.ChA, Source: texture.ChA},
		},
		Rounding: texture.RoundNearest, Alpha: texture.AlphaPremultiply,
	}
	plan := &texture.Plan{
		Version: "test",
		Source:  texture.Source{Width: 1, Height: 1, BitDepth: 8, Layout: []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA}},
		Entries: []texture.Entry{entry},
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	result, err := texture.ApplyEntry(source, plan, entry)
	if err != nil {
		t.Fatal(err)
	}
	if result.Image.Pix[0] != 100 || result.Image.Pix[3] != 128 {
		t.Fatalf("premultiply pixel = %v", result.Image.Pix)
	}
	entry.Alpha = texture.AlphaUnpremultiply
	entry.Mappings = []texture.ChannelMapping{
		{Target: texture.ChR, Source: texture.ChR}, {Target: texture.ChG, Source: texture.ChG},
		{Target: texture.ChB, Source: texture.ChB}, {Target: texture.ChA, Source: texture.ChA},
	}
	plan.Entries[0] = entry
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	result, err = texture.ApplyEntry(result.Image, plan, entry)
	if err != nil {
		t.Fatal(err)
	}
	if result.Image.Pix[0] < 198 || result.Image.Pix[0] > 200 {
		t.Fatalf("unpremultiply pixel = %v", result.Image.Pix)
	}
}

func TestStateMachineAndInvalidation(t *testing.T) {
	state, err := texture.NewStateMachine(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Commit(0, 0, texture.FrozenOutput{EntryIndex: 0, ChunkIndex: 0}); !texture.HasCode(err, texture.CodeState) {
		t.Fatalf("illegal transition: %v", err)
	}
	if got, _ := state.State(0, 0); got != texture.StatePending {
		t.Fatalf("state changed on failure: %s", got)
	}
	if err := state.Begin(0, 0); err != nil {
		t.Fatal(err)
	}
	snapshot := texture.FrozenOutput{EntryIndex: 0, ChunkIndex: 0, OutputHash: "x", ParameterHash: "p", PlanFingerprint: "f"}
	if err := state.Commit(0, 0, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := state.Begin(0, 0); !texture.HasCode(err, texture.CodeState) {
		t.Fatalf("committed state must be frozen: %v", err)
	}

	bundle, err := sample.Generate(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	invalidations, err := texture.Invalidate(bundle.BasePlan, bundle.Plan, 32, 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(invalidations) == 0 {
		t.Fatal("expected invalidations")
	}
	for _, item := range invalidations {
		if item.EntryIndex != 2 {
			t.Fatalf("edit of entry 2 marked entry %d", item.EntryIndex)
		}
	}
}

func TestChannelAddDropAndFillLoss(t *testing.T) {
	source, _ := texture.NewImage(1, 1, 8, []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA})
	copy(source.Pix, []uint16{11, 22, 33, 44})
	plan := &texture.Plan{
		Version: "test",
		Source:  texture.Source{Width: 1, Height: 1, BitDepth: 8, Layout: []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA}},
		Entries: []texture.Entry{
			{
				Index: 0, Coverage: texture.FullRect(1, 1), TargetBitDepth: 8,
				TargetChannels: []texture.Channel{texture.ChR, texture.ChG, texture.ChB},
				Mappings: []texture.ChannelMapping{
					{Target: texture.ChR, Source: texture.ChR},
					{Target: texture.ChG, Source: texture.ChG},
					{Target: texture.ChB, Source: texture.ChB},
				},
				Rounding: texture.RoundNearest, Alpha: texture.AlphaDrop,
			},
			{
				Index: 1, Coverage: texture.FullRect(1, 1), TargetBitDepth: 8,
				TargetChannels: []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA},
				Mappings: []texture.ChannelMapping{
					{Target: texture.ChR, Source: texture.ChR},
					{Target: texture.ChG, Source: texture.ChG},
					{Target: texture.ChB, Source: texture.ChB},
					{Target: texture.ChA, Fill: &texture.FillSource{Kind: texture.FillConstant, Value: 200}},
				},
				Rounding: texture.RoundNearest, Alpha: texture.AlphaCopy,
			},
		},
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	out, _, err := texture.ConvertPlan(source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Layout) != 4 || out.At(0, 0, 3) != 200 {
		t.Fatalf("channel-add result layout=%v pixels=%v", out.Layout, out.Pix)
	}

	lossyPlan := plan.Clone()
	lossyPlan.Entries[1].Mappings[0] = texture.ChannelMapping{Target: texture.ChR}
	lossyPlan.Entries[1].Mappings[0].Fill = &texture.FillSource{Kind: texture.FillConstant, Value: 1}
	lossyPlan.Entries[1].Mappings[1] = texture.ChannelMapping{Target: texture.ChG}
	lossyPlan.Entries[1].Mappings[1].Fill = &texture.FillSource{Kind: texture.FillConstant, Value: 2}
	if err := lossyPlan.Validate(); err != nil {
		t.Fatal(err)
	}
	data, losses, err := texture.ExportPlan(lossyPlan, "1")
	if !texture.HasCode(err, texture.CodeLossy) || len(losses) == 0 {
		t.Fatalf("multiple fill rules should be lossy: err=%v losses=%v", err, losses)
	}
	if _, loadErr := texture.LoadPlan(data); loadErr != nil {
		t.Fatalf("lossy fill artifact: %v", loadErr)
	}
}

func TestCorruptCheckpointInvalidatesWholeSnapshot(t *testing.T) {
	ctx := context.Background()
	bundle, err := sample.Generate(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "corrupt.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := pipeline.Config{ChunkWidth: 32, ChunkHeight: 32, Workers: 2, PixelBufferPerWorker: 1 << 20}
	var cancel context.CancelFunc
	runCtx, cancelRun := context.WithCancel(ctx)
	cancel = cancelRun
	commits := 0
	cfg.OnCommit = func(ref pipeline.CommitRef) {
		commits++
		if commits == 2 {
			cancel()
		}
	}
	runner, _ := pipeline.NewRunner(st, cfg)
	_, err = runner.Run(runCtx, bundle.Source, bundle.Plan, pipeline.Options{Mode: pipeline.ModeFull, RunID: "corrupt"})
	if !errors.Is(err, texture.ErrCanceled) {
		t.Fatalf("cancel: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE stage_outputs SET output_hash = 'tampered' WHERE run_id = 'corrupt' AND chunk_x = 0 AND chunk_y = 0 AND entry_index = 0`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resumeRunner, _ := pipeline.NewRunner(reopened, pipeline.Config{ChunkWidth: 32, ChunkHeight: 32, Workers: 2, PixelBufferPerWorker: 1 << 20})
	resumed, err := resumeRunner.Run(ctx, bundle.Source, bundle.Plan, pipeline.Options{Mode: pipeline.ModeResume, RunID: "corrupt"})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.CheckpointWiped {
		t.Fatal("corrupt checkpoint was not invalidated")
	}
	if resumed.FinalHash != bundle.Truth.ExpectedFinalHash {
		t.Fatalf("resumed hash %s != truth %s", resumed.FinalHash, bundle.Truth.ExpectedFinalHash)
	}
}

func TestCompareReportsFirstPixel(t *testing.T) {
	bundle, err := sample.Generate(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := texture.ConvertPlan(bundle.Source, bundle.Plan)
	if err != nil {
		t.Fatal(err)
	}
	one := full.Clone()
	two := full.Clone()
	three := full.Clone()
	three.Set(3, 2, 0, three.At(3, 2, 0)+1)
	_, err = pipeline.ComparePaths(bundle.Plan,
		pipeline.PathSnapshot{Name: "full", Final: full},
		pipeline.PathSnapshot{Name: "incremental", Final: one},
		pipeline.PathSnapshot{Name: "resume", Final: three},
	)
	if !texture.HasCode(err, texture.CodeDiff) {
		t.Fatalf("want E_DIFF, got %v", err)
	}
	ke, _ := texture.AsKernelError(err)
	if ke.Diff == nil || ke.Diff.X != 3 || ke.Diff.Y != 2 || ke.Diff.Field == "" {
		t.Fatalf("bad diff: %+v", ke.Diff)
	}
	_ = two
}

func TestPixelBufferLimit(t *testing.T) {
	bundle, err := sample.Generate(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "buffer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner, err := pipeline.NewRunner(st, pipeline.Config{
		ChunkWidth: 32, ChunkHeight: 32, Workers: 1, PixelBufferPerWorker: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), bundle.Source, bundle.Plan, pipeline.Options{Mode: pipeline.ModeFull, RunID: "buffer"})
	if !texture.HasCode(err, texture.CodePara) {
		t.Fatalf("want E_PARA, got %v", err)
	}
}

func TestClippedEdgeTilesAndIndependentVerification(t *testing.T) {
	ctx := context.Background()
	opts := testOptions(t)
	opts.Width = 67
	opts.Height = 53
	opts.ChunkWidth = 16
	opts.ChunkHeight = 16
	bundle, err := sample.Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "clipped.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner, err := pipeline.NewRunner(st, pipeline.Config{
		ChunkWidth: 16, ChunkHeight: 16, Workers: 3, PixelBufferPerWorker: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(ctx, bundle.Source, bundle.Plan, pipeline.Options{Mode: pipeline.ModeFull, RunID: "clipped"})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalHash != bundle.Truth.ExpectedFinalHash {
		t.Fatalf("clipped final hash %s != truth %s", result.FinalHash, bundle.Truth.ExpectedFinalHash)
	}
	report, err := verify.VerifyCheckpoint(ctx, st, "clipped", bundle.Plan, bundle.Truth)
	if err != nil {
		t.Fatal(err)
	}
	if report.Chunks != 5*4 {
		t.Fatalf("chunks = %d, want %d", report.Chunks, 5*4)
	}
}

func TestThreePathsResumeIncrementalAndVerifier(t *testing.T) {
	ctx := context.Background()
	bundle, err := sample.Generate(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	fullStore, err := store.Open(filepath.Join(dir, "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fullStore.Close()
	fullRunner := testRunner(t, fullStore, 3)
	full, err := fullRunner.Run(ctx, bundle.Source, bundle.Plan, pipeline.Options{Mode: pipeline.ModeFull, RunID: "full"})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if full.FinalHash != bundle.Truth.ExpectedFinalHash {
		t.Fatalf("full final hash differs from truth")
	}

	incStore, err := store.Open(filepath.Join(dir, "incremental.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer incStore.Close()
	incRunner := testRunner(t, incStore, 2)
	if _, err := incRunner.Run(ctx, bundle.Source, bundle.BasePlan, pipeline.Options{Mode: pipeline.ModeFull, RunID: "base"}); err != nil {
		t.Fatalf("base: %v", err)
	}
	incremental, err := incRunner.Run(ctx, bundle.Source, bundle.Plan, pipeline.Options{
		Mode: pipeline.ModeIncremental, RunID: "incremental", PreviousRunID: "base", PreviousPlan: bundle.BasePlan,
	})
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}

	resumePath := filepath.Join(dir, "resume.db")
	resumeStore, err := store.Open(resumePath)
	if err != nil {
		t.Fatal(err)
	}
	var cancel context.CancelFunc
	runCtx, cancelRun := context.WithCancel(ctx)
	cancel = cancelRun
	commitCount := 0
	resumeRunner, err := pipeline.NewRunner(resumeStore, pipeline.Config{
		ChunkWidth: 32, ChunkHeight: 32, Workers: 2, PixelBufferPerWorker: 1 << 20,
		OnCommit: func(ref pipeline.CommitRef) {
			commitCount++
			if commitCount == 2 {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	partial, err := resumeRunner.Run(runCtx, bundle.Source, bundle.Plan, pipeline.Options{Mode: pipeline.ModeFull, RunID: "resume"})
	if !errors.Is(err, texture.ErrCanceled) {
		t.Fatalf("expected cancel, got %v", err)
	}
	if partial == nil || len(partial.CommitSequence) != 2 {
		t.Fatalf("partial commits = %+v", partial)
	}
	if err := resumeStore.Close(); err != nil {
		t.Fatal(err)
	}

	resumedStore, err := store.Open(resumePath)
	if err != nil {
		t.Fatal(err)
	}
	defer resumedStore.Close()
	resumeRunner2 := testRunner(t, resumedStore, 2)
	resumed, err := resumeRunner2.Run(ctx, bundle.Source, bundle.Plan, pipeline.Options{Mode: pipeline.ModeResume, RunID: "resume"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.CheckpointWiped {
		t.Fatal("valid checkpoint was wiped")
	}

	report, err := pipeline.ComparePaths(bundle.Plan,
		pipeline.PathSnapshot{Name: "full", Final: full.Final, StageHashes: full.StageHashes, CommitSequence: full.CommitSequence},
		pipeline.PathSnapshot{Name: "incremental", Final: incremental.Final, StageHashes: incremental.StageHashes, CommitSequence: incremental.CommitSequence},
		pipeline.PathSnapshot{Name: "resume", Final: resumed.Final, StageHashes: resumed.StageHashes, CommitSequence: resumed.CommitSequence},
	)
	if err != nil {
		t.Logf("full stages: %+v", full.StageHashes)
		t.Logf("incremental stages: %+v", incremental.StageHashes)
		t.Logf("resume stages: %+v", resumed.StageHashes)
		t.Logf("incremental reused=%d computed=%d", incremental.ReusedChunks, incremental.ComputedChunks)
		t.Fatalf("compare: %v", err)
	}
	if report.FinalHash != full.FinalHash {
		t.Fatalf("compare report hash = %s, want %s", report.FinalHash, full.FinalHash)
	}
	verifyReport, err := verify.VerifyCheckpoint(ctx, fullStore, "full", bundle.Plan, bundle.Truth)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verifyReport.SamplesChecked < 10 {
		t.Fatalf("samples checked = %d", verifyReport.SamplesChecked)
	}
}
