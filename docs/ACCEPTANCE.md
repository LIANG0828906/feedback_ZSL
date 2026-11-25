# Acceptance Record

Executed in this workspace on 2026-11-25 (Linux host, Go 1.22.12) using only local deterministic samples.

## Commands

```bash
go test ./...
go run ./cmd/atelier-cli -verify
wails build -platform windows/amd64 -skipbindings -clean
```

All three commands exited 0.

The same CLI suite was run for all three region profiles:

| Seed | Region | Zones | Truth points | Rebuilt points | Result |
| ---: | --- | ---: | ---: | ---: | --- |
| 2042 | 边境盆地 | 4 | 3319 | 3323 | PASS |
| 7183 | 赤谷裂原 | 3 | 2899 | 2906 | PASS |
| 9911 | 潮汐边缘 | 4 | 3177 | 3178 | PASS |

The persisted `artifacts/verification.json` is the seed-2042 run; the 7183/9911 runs were also checked with the same command and all 18 checks passed for each.

## Result

```text
seed=2042 algorithm=reconstruction-engine/1.0.0
truth points=3319 rebuilt points=3323 heatmap=60x40

PASS checkpoint-active-cancel-source
PASS checkpoint-complete-snapshot
PASS checkpoint-forced-recovery-source
PASS checkpoint-mismatch-invalidates-all
PASS checkpoint-resume-equals-full
PASS density-edit-reuses-sampling
PASS deterministic-rebuild
PASS exclusion-precise-propagation
PASS first-difference-locator
PASS heatmap-at-observed-grid
PASS incremental-equals-full-after-exclusion
PASS incremental-equals-full-after-zone-edit
PASS rarity-edit-reuses-heatmap
PASS rarity-mixture-tolerance
PASS reconstruction-count-tolerance
PASS schema-and-geometry-validation
PASS worker-resource-boundaries
PASS zone-density-tolerance
result=true
```

Measured values:

| Metric | Measured | Declared tolerance |
| --- | ---: | ---: |
| Reconstructed/truth count delta | +4 / 3319 | max(5, 10%) |
| Max zone density absolute error | 0.000200 | 0.012 |
| Max rarity L1 error | 0.001953 | 0.055 |
| Max heatmap error at observed grid | 0.005000 | 0.018 |

Invalidation measurements:

| Operation | Affected zones | Reused zones | Reused pixels | Recomputed pixels |
| --- | ---: | ---: | ---: | ---: |
| Density scale edit | 1 | 3 | 1996 | 404 |
| Rarity shift edit | 1 | 3 | 2400 | 0 |
| Exclusion move | 1 | 3 | 1176 | 1224 |

Recovery:

- Completed checkpoint: 4 zones, `completed_checkpoint`.
- Forced termination: 2 contiguous zones, `forced_termination`, resume result equal to full.
- Active cancel: `active_cancel`.
- Parameter version mismatch: entire snapshot discarded, zero rows remain.

Machine-readable evidence: `artifacts/verification.json`.

## GUI verification boundary

`wails build -platform windows/amd64` produced `build/bin/atelier.exe` successfully. The current execution host is Linux and does not provide a Windows/WebView2 desktop session, so the window was not launched here. The application target itself is Windows + WebView2 as required.
