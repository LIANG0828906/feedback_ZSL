package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"textureworkbench/internal/sample"
	"textureworkbench/internal/tex"
)

type testFixture struct {
	image  *tex.Image
	desc   tex.Descriptor
	dir    string
	sample string
}

func makeFixture(t *testing.T, sampleID, descriptorID string) testFixture {
	t.Helper()
	dir := t.TempDir()
	manifest, err := sample.Generate(dir, 987654321)
	if err != nil {
		t.Fatalf("generate samples: %v", err)
	}
	info, ok := manifest.Sample(sampleID)
	if !ok {
		t.Fatalf("sample %s missing", sampleID)
	}
	desc, ok := manifest.Descriptor(descriptorID)
	if !ok {
		t.Fatalf("descriptor %s missing", descriptorID)
	}
	image, err := tex.DecodeImageFile(filepath.Join(dir, info.File))
	if err != nil {
		t.Fatalf("decode sample: %v", err)
	}
	image.ID = info.ID
	image.EnsureHash()
	return testFixture{image: image, desc: desc, dir: dir, sample: sampleID}
}

func testConfig(t *testing.T, fixture testFixture) Config {
	t.Helper()
	return Config{
		BlockWidth:            16,
		BlockHeight:           16,
		MaxWorkers:            4,
		BufferPixelsPerWorker: 16 * 16,
		QueueDepth:            2,
		ParameterVersion:      "params-v1",
		SessionKey:            "fixture-" + fixture.sample,
		CheckpointRoot:        filepath.Join(t.TempDir(), "checkpoints"),
	}
}

func requireCode(t *testing.T, err error, code string) *tex.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	var texErr *tex.Error
	if !errors.As(err, &texErr) {
		t.Fatalf("expected *tex.Error, got %T: %v", err, err)
	}
	if texErr.Code != code {
		t.Fatalf("code=%s, want %s (%v)", texErr.Code, code, err)
	}
	return texErr
}

func TestTransitionRules(t *testing.T) {
	legal := [][2]BlockState{
		{BlockPending, BlockProcessing},
		{BlockProcessing, BlockCommitted},
		{BlockProcessing, BlockFailed},
		{BlockFailed, BlockProcessing},
	}
	for _, pair := range legal {
		if err := Transition(pair[0], pair[1]); err != nil {
			t.Fatalf("%s -> %s should be legal: %v", pair[0], pair[1], err)
		}
	}
	illegal := [][2]BlockState{
		{BlockPending, BlockCommitted},
		{BlockCommitted, BlockProcessing},
		{BlockCommitted, BlockFailed},
		{BlockPending, BlockFailed},
	}
	for _, pair := range illegal {
		t.Run(fmt.Sprintf("%s_to_%s", pair[0], pair[1]), func(t *testing.T) {
			err := Transition(pair[0], pair[1])
			requireCode(t, err, tex.CodeState)
		})
	}
}

func TestCheckpointResumeMatchesFull(t *testing.T) {
	fixture := makeFixture(t, "rgba8", "rgba8-to-bgra16-repeat")
	cfg := testConfig(t, fixture)
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if err := session.RunSegmented(RunOptions{MaxBlocks: 7}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := session.Snapshot()
	if first.Progress.Committed != 7 {
		t.Fatalf("committed=%d, want 7", first.Progress.Committed)
	}
	if first.Progress.Committed >= first.Progress.Total {
		t.Fatalf("fixture completed before resume")
	}
	session.Close()

	restoredSession, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	restored := restoredSession.Snapshot()
	if !restored.Restore.Used || restored.Restore.Invalidated {
		t.Fatalf("restore=%+v, want a valid persisted snapshot", restored.Restore)
	}
	if restored.Restore.RestoredBlocks != 7 || restored.Progress.Committed != 7 {
		t.Fatalf("restored=%d committed=%d, want 7", restored.Restore.RestoredBlocks, restored.Progress.Committed)
	}
	if err := restoredSession.RunSegmented(RunOptions{}); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	result, err := restoredSession.Result()
	if err != nil {
		snapshot := restoredSession.Snapshot()
		t.Fatalf("assemble resumed result: %v snapshot=%+v first_failure=%+v", err, snapshot.Progress, snapshot.Blocks[7])
	}
	full, err := tex.ConvertFull(fixture.image, fixture.desc, tex.ConvertOptions{})
	if err != nil {
		t.Fatalf("full conversion: %v", err)
	}
	if err := CompareConvertedOrError(result, full, cfg.BlockWidth, cfg.BlockHeight); err != nil {
		t.Fatalf("resumed and full results differ: %v", err)
	}
	restoredSession.Close()
}

func TestVersionMismatchInvalidatesEntireSnapshot(t *testing.T) {
	fixture := makeFixture(t, "rgba8", "rgba8-to-bgra16-repeat")
	cfg := testConfig(t, fixture)
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if err := session.RunSegmented(RunOptions{MaxBlocks: 5}); err != nil {
		t.Fatalf("partial run: %v", err)
	}
	session.Close()

	newDesc := fixture.desc
	newDesc.TargetBitDepth = 8
	newDesc.TargetLayout = tex.LayoutRGB
	newDesc.Permutation = []int{0, 1, 2, 3}
	newDesc.Fill = tex.FillZero
	newDesc.ID = "changed-target"
	changed, err := OpenSession(fixture.image, newDesc, cfg)
	if err != nil {
		t.Fatalf("open changed descriptor: %v", err)
	}
	snapshot := changed.Snapshot()
	if !snapshot.Restore.Invalidated {
		t.Fatalf("snapshot was not invalidated: %+v", snapshot.Restore)
	}
	foundReason := false
	for _, reason := range snapshot.Restore.Reasons {
		if reason == "target_descriptor_version_mismatch" {
			foundReason = true
		}
	}
	if !foundReason {
		t.Fatalf("reasons=%v, want target_descriptor_version_mismatch", snapshot.Restore.Reasons)
	}
	if snapshot.Progress.Committed != 0 || snapshot.Restore.RestoredBlocks != 0 {
		t.Fatalf("old blocks were partially reused: progress=%+v restore=%+v", snapshot.Progress, snapshot.Restore)
	}
	for _, block := range snapshot.Blocks {
		if block.State != BlockPending {
			t.Fatalf("block %d state=%s, want pending", block.Index, block.State)
		}
	}
	if err := changed.RunSegmented(RunOptions{}); err != nil {
		t.Fatalf("run changed descriptor: %v", err)
	}
	changedResult, err := changed.Result()
	if err != nil {
		t.Fatalf("assemble changed result: %v", err)
	}
	full, err := tex.ConvertFull(fixture.image, newDesc, tex.ConvertOptions{})
	if err != nil {
		t.Fatalf("full changed conversion: %v", err)
	}
	if err := CompareConvertedOrError(changedResult, full, cfg.BlockWidth, cfg.BlockHeight); err != nil {
		t.Fatalf("changed path differs: %v", err)
	}
	changed.Close()

	reopened, err := OpenSession(fixture.image, newDesc, cfg)
	if err != nil {
		t.Fatalf("reopen changed: %v", err)
	}
	defer reopened.Close()
	reopenedSnapshot := reopened.Snapshot()
	if !reopenedSnapshot.Restore.Used || reopenedSnapshot.Restore.RestoredBlocks == 0 {
		t.Fatalf("new complete snapshot was not restored: %+v", reopenedSnapshot.Restore)
	}
}

func TestParameterVersionMismatchInvalidatesEntireSnapshot(t *testing.T) {
	fixture := makeFixture(t, "rgb8", "rgb8-to-bgra8-zero")
	cfg := testConfig(t, fixture)
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RunSegmented(RunOptions{MaxBlocks: 3}); err != nil {
		t.Fatal(err)
	}
	session.Close()
	cfg.ParameterVersion = "params-v2"
	changed, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer changed.Close()
	snapshot := changed.Snapshot()
	if !snapshot.Restore.Invalidated {
		t.Fatalf("parameter mismatch did not invalidate snapshot")
	}
	found := false
	for _, reason := range snapshot.Restore.Reasons {
		if reason == "parameter_version_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons=%v, want parameter_version_mismatch", snapshot.Restore.Reasons)
	}
}

func TestContentHashMismatchInvalidatesEntireSnapshot(t *testing.T) {
	fixture := makeFixture(t, "rgb8", "rgb8-to-bgr8-swap")
	cfg := testConfig(t, fixture)
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RunSegmented(RunOptions{MaxBlocks: 2}); err != nil {
		t.Fatal(err)
	}
	session.Close()

	altered := *fixture.image
	altered.Pixels = append([]uint16(nil), fixture.image.Pixels...)
	altered.Pixels[0] ^= 1
	altered.ContentHash = altered.ComputeContentHash()
	changed, err := OpenSession(&altered, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer changed.Close()
	snapshot := changed.Snapshot()
	if !snapshot.Restore.Invalidated {
		t.Fatalf("content hash mismatch did not invalidate snapshot")
	}
	found := false
	for _, reason := range snapshot.Restore.Reasons {
		if reason == "content_hash_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons=%v, want content_hash_mismatch", snapshot.Restore.Reasons)
	}
}

func TestAlgorithmVersionMismatchInvalidatesEntireSnapshot(t *testing.T) {
	fixture := makeFixture(t, "gray16", "gray16-to-gray8")
	cfg := testConfig(t, fixture)
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RunSegmented(RunOptions{MaxBlocks: 2}); err != nil {
		t.Fatal(err)
	}
	session.Close()
	manifestPath := filepath.Join(cfg.CheckpointRoot, cfg.SessionKey, "checkpoint.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest checkpointManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Header.AlgorithmVersion = "quant-v0"
	data, _ = json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer changed.Close()
	snapshot := changed.Snapshot()
	if !snapshot.Restore.Invalidated {
		t.Fatalf("algorithm mismatch did not invalidate snapshot")
	}
	found := false
	for _, reason := range snapshot.Restore.Reasons {
		if reason == "algorithm_version_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons=%v, want algorithm_version_mismatch", snapshot.Restore.Reasons)
	}
}

func TestPerBlockVersionMismatchInvalidatesEntireSnapshot(t *testing.T) {
	fixture := makeFixture(t, "rgb16", "rgb16-to-rgba16-repeat")
	cfg := testConfig(t, fixture)
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RunSegmented(RunOptions{MaxBlocks: 3}); err != nil {
		t.Fatal(err)
	}
	session.Close()
	manifestPath := filepath.Join(cfg.CheckpointRoot, cfg.SessionKey, "checkpoint.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest checkpointManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Records[1].ParameterVersion = "tampered-params"
	data, _ = json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer changed.Close()
	snapshot := changed.Snapshot()
	if !snapshot.Restore.Invalidated || snapshot.Restore.RestoredBlocks != 0 {
		t.Fatalf("per-block mismatch was not invalidated wholesale: %+v", snapshot.Restore)
	}
	found := false
	for _, reason := range snapshot.Restore.Reasons {
		if reason == "block_1_version_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons=%v, want block_1_version_mismatch", snapshot.Restore.Reasons)
	}
}

func TestFailedBlockCanBeRetriedWithoutPollutingOthers(t *testing.T) {
	fixture := makeFixture(t, "bgr8", "bgr8-to-bgra8-zero")
	cfg := testConfig(t, fixture)
	cfg.MaxWorkers = 3
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.InjectFault(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := session.RunSegmented(RunOptions{}); err != nil {
		t.Fatalf("run with fault: %v", err)
	}
	before := session.Snapshot()
	if before.Progress.Failed != 1 {
		t.Fatalf("failed=%d, want 1", before.Progress.Failed)
	}
	if before.Blocks[1].State != BlockFailed {
		t.Fatalf("block 1 state=%s", before.Blocks[1].State)
	}
	committedHashes := make(map[int]string)
	for _, block := range before.Blocks {
		if block.State == BlockCommitted {
			committedHashes[block.Index] = block.OutputHash
		}
	}
	if _, err := session.Result(); err == nil {
		t.Fatalf("incomplete result should fail")
	}
	retried, err := session.RetryBlock(1)
	if err != nil {
		t.Fatalf("RetryBlock: %v", err)
	}
	if retried.State != BlockCommitted {
		t.Fatalf("retried state=%s", retried.State)
	}
	after := session.Snapshot()
	if after.Progress.Committed != after.Progress.Total || after.Progress.Failed != 0 {
		t.Fatalf("progress after retry: %+v", after.Progress)
	}
	for index, hash := range committedHashes {
		if after.Blocks[index].OutputHash != hash {
			t.Fatalf("retry changed committed block %d", index)
		}
	}
	result, err := session.Result()
	if err != nil {
		t.Fatal(err)
	}
	full, err := tex.ConvertFull(fixture.image, fixture.desc, tex.ConvertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareConvertedOrError(result, full, cfg.BlockWidth, cfg.BlockHeight); err != nil {
		t.Fatalf("retried result differs: %v", err)
	}
	if _, err := session.RetryBlock(0); err == nil {
		t.Fatalf("retrying committed block must fail")
	} else {
		requireCode(t, err, tex.CodeState)
		if session.Snapshot().Blocks[0].State != BlockCommitted {
			t.Fatalf("illegal retry changed committed state")
		}
	}
}

func TestReconfigureInvalidatesExplicitlyAndFreezesOldHashes(t *testing.T) {
	fixture := makeFixture(t, "rgba8", "rgba8-to-rgb8-drop")
	cfg := testConfig(t, fixture)
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.RunSegmented(RunOptions{}); err != nil {
		t.Fatal(err)
	}
	before := session.Snapshot()
	oldHash := before.Blocks[0].OutputHash
	newDesc := fixture.desc
	newDesc.TargetBitDepth = 16
	newDesc.TargetLayout = tex.LayoutBGRA
	newDesc.Fill = tex.FillRepeatAlpha
	newDesc.Permutation = []int{2, 1, 0, 3}
	invalidated, err := session.Reconfigure(newDesc, "params-v2")
	if err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if len(invalidated) == 0 || invalidated[0].OutputHash == "" {
		t.Fatalf("invalidated blocks not reported with old hashes: %+v", invalidated)
	}
	if invalidated[0].OutputHash != oldHash {
		t.Fatalf("old committed hash was mutated: got %s want %s", invalidated[0].OutputHash, oldHash)
	}
	after := session.Snapshot()
	if len(after.Generations) != 1 || after.Generations[0].Reason != "target_descriptor_changed" {
		t.Fatalf("generation audit missing: %+v", after.Generations)
	}
	if after.Progress.Committed != 0 || after.Progress.Pending != after.Progress.Total {
		t.Fatalf("blocks were not reset after explicit invalidation: %+v", after.Progress)
	}
	if !after.Blocks[0].Invalidated || after.Blocks[0].InvalidationReason == "" {
		t.Fatalf("UI invalidation marker missing")
	}
	if err := session.RunSegmented(RunOptions{}); err != nil {
		t.Fatal(err)
	}
	result, err := session.Result()
	if err != nil {
		t.Fatal(err)
	}
	full, err := tex.ConvertFull(fixture.image, newDesc, tex.ConvertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareConvertedOrError(result, full, cfg.BlockWidth, cfg.BlockHeight); err != nil {
		t.Fatalf("reconfigured result differs: %v", err)
	}
	for _, block := range session.Snapshot().Blocks {
		if block.Invalidated {
			t.Fatalf("recomputed block %d still carries an obsolete invalidation marker", block.Index)
		}
	}
}

func TestCheckpointManifestIsOrderedAndAtomic(t *testing.T) {
	fixture := makeFixture(t, "gray8", "gray8-to-rgba16-repeat")
	cfg := testConfig(t, fixture)
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RunSegmented(RunOptions{}); err != nil {
		t.Fatal(err)
	}
	session.Close()
	manifestPath := filepath.Join(cfg.CheckpointRoot, cfg.SessionKey, "checkpoint.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest checkpointManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if len(manifest.Records) != session.Snapshot().Progress.Total {
		t.Fatalf("records=%d total=%d", len(manifest.Records), session.Snapshot().Progress.Total)
	}
	for i, record := range manifest.Records {
		if record.Sequence != i {
			t.Fatalf("record %d sequence=%d, want %d", i, record.Sequence, i)
		}
		if record.OutputHash == "" || record.ErrorHash == "" {
			t.Fatalf("record %d missing content hashes", i)
		}
	}
}

func TestBufferBoundRejectedAndQueueWaitRecorded(t *testing.T) {
	fixture := makeFixture(t, "rgba16", "rgba16-to-bgra16-repeat")
	cfg := testConfig(t, fixture)
	cfg.BlockWidth = 8
	cfg.BlockHeight = 8
	cfg.BufferPixelsPerWorker = 63
	if _, err := OpenSession(fixture.image, fixture.desc, cfg); err == nil {
		t.Fatalf("undersized per-worker buffer should be rejected")
	} else {
		requireCode(t, err, tex.CodePara)
	}
	cfg.BufferPixelsPerWorker = 64
	cfg.MaxWorkers = 2
	cfg.QueueDepth = 1
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.RunSegmented(RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if session.Snapshot().Stats.BufferQueueWaits == 0 {
		t.Fatalf("expected bounded queue waits to be recorded")
	}
}

func TestPauseAndResume(t *testing.T) {
	fixture := makeFixture(t, "rgba16", "rgba16-to-bgra16-repeat")
	cfg := testConfig(t, fixture)
	cfg.BlockWidth = 8
	cfg.BlockHeight = 8
	cfg.BufferPixelsPerWorker = 64
	cfg.MaxWorkers = 1
	cfg.QueueDepth = 1
	session, err := OpenSession(fixture.image, fixture.desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.StartSegmented(RunOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	session.Pause()
	session.Wait()
	paused := session.Snapshot()
	if paused.Progress.Committed >= paused.Progress.Total {
		t.Skip("fixture completed before pause could take effect")
	}
	if paused.Running {
		t.Fatalf("session still running after pause wait")
	}
	if err := session.RunSegmented(RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if session.Snapshot().Progress.Committed != paused.Progress.Total {
		t.Fatalf("resume did not complete all blocks")
	}
}
