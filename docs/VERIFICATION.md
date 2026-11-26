# Verification Record

Environment: Linux, Go 1.22.12, standard library only, no network or container.

## Commands

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...

go run ./cmd/strokediag generate -seed 424242 -out artifacts/final-demo
go run ./cmd/strokediag verify   -seed 424242 -out artifacts/final-acceptance
```

Final run result:

```text
generated 3 samples with seed 424242
  v1-pixel-64        mask-work/v1    64x64  strokes=9
  v2-relative-96     mask-work/v2    96x96  strokes=9
  v3-grouped-128     mask-work/v3   128x128 strokes=9

acceptance: PASS (seed=424242 sample=v3-grouped-128)
  full=6c79b0865ec26d99
  incremental=6c79b0865ec26d99
  resume=6c79b0865ec26d99
  interrupted=3 checkpoint_records=3 resumed_commits=9 complete=true
  invalidation_affected=7 recomputed=7 cache_reuse_equal=true
```

Full SHA-256 for all three paths:

```text
6c79b0865ec26d9926abc6ecac6dd0859cbf3d2d8f11b74d1ef7f45838cda765
```

`artifacts/final-acceptance/verification.json` records the machine-readable
comparison. The resume and edited HTML reports are:

```text
artifacts/final-acceptance/reports/resume/report.html
artifacts/final-acceptance/reports/edited/report.html
```

The edited acceptance scenario lowers `body-top` by 600 pp10000. It invalidates
exactly blocks 8..14, recomputes exactly 8..14, reuses all other persisted block
outputs, and matches a clean full run after the edit.

## Forced Termination

Actual command sequence:

```bash
go run ./cmd/strokediag run \
  -dir artifacts/final-demo/v3-grouped-128 -mode incremental \
  -checkpoint artifacts/hard-abort.ckpt -hard-abort-after 3 \
  -out artifacts/hard-abort-partial
# output: HARD ABORT simulated after 3 committed strokes; no report promised

go run ./cmd/strokediag run \
  -dir artifacts/final-demo/v3-grouped-128 -mode resume \
  -checkpoint artifacts/hard-abort.ckpt -out artifacts/hard-abort-resumed
```

Restart output:

```text
composite=6c79b0865ec26d99 mode=resume blocks=64
topology=true differences=0 issues=0
checkpoint records=9 committed_strokes=9 complete=true
```

The full-run output in the same environment also reports
`composite=6c79b0865ec26d99`. The journal contained no exit report and was
recovered solely from nine CRC-validated, fsynced stroke transactions.

## Automated Test Matrix

| Requirement | Tests |
|---|---|
| deterministic multi-resolution generator | `TestGeneratorDeterministicAndMultiResolution` |
| v1/v2/v3 field names, scales, bases, hardness presence | `TestVersionLoadersShapeAndRules` |
| unknown version / required field | `TestUnknownVersionAndMissingRequiredField` |
| default backfill trace | `TestV1DefaultBackfillTraces`, `TestV2DefaultBackfillTraces` |
| exact cross-version round trip | `TestLosslessExportsAndExactCrossVersionRoundTrip` |
| E_LOSSY loss list/disposition | `TestLossyExportListsAndE_LOSSY` |
| integer rounding, residual, E_OVF | `TestIntegerWeightAndCoordinateConversions`, `TestIntegerConversionOverflowDetails` |
| E_ORD ambiguity | `TestOrderTieReturnsE_ORD` |
| declaration/scheduler determinism | `TestSchedulingAndDeclarationOrderDoNotChangePixelsOrAttribution` |
| topology + attribution + E_TOP evidence | `TestTopologyFailureAndStrokeAttribution`, `TestTopologyHoleAndRingIssuesIncludeMeasuredValues` |
| block cache/invalidation precision | `TestEditInvalidatesOnlyAffectedBlocksAndReusesCache` |
| budget, queue wait, retry | `TestSchedulerQueueBudgetPixelLimitAndRetry` |
| failed stroke does not commit | `TestFailureAfterCommittedStrokeDoesNotPolluteCheckpoint` |
| interruption + resume | `TestIncrementalStopAndResumeMatchesFull` |
| tail truncation / stale version | `TestCheckpointTailTruncationAndVersionMismatch` |
| middle corruption rejection | `TestCorruptMiddleCheckpointRecordIsRejected` |
| first block/stroke mismatch localization | `TestCompareEvaluationsLocatesBlockAndStroke` |
| diagnostic artifacts | `TestDiagnosticArtifactsAndSampleLoader` |
| end-to-end acceptance | `TestRunAcceptance` |

`go test -race ./...` passes. `go vet ./...` reports no issues.

## Generated Samples

The fixed seed creates:

- `v1-pixel-64`: 64x64, 9 strokes, v1 flat operations;
- `v2-relative-96`: 96x96, 9 strokes, relative offsets;
- `v3-grouped-128`: 128x128, 9 strokes, grouped v3 constructs.

Each sample includes:

- a PNG icon with near-color gradient background, foreground blocks,
  semi-transparent boundary pixels, and embedded deterministic noise;
- a candidate mask with one-pixel inset, irregular partial-alpha outset, and
  deliberate interior holes;
- declared stroke order, brush radius, hardness, and coverage weight;
- reference mask, domain stats (centroid/area/bbox/holes/ring width), and
  per-stroke pixel contributions.

All artifacts are local under `artifacts/` and ignored by Git.
